package main

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"fogs/agent/ui"
	"fogs/proto"
	"fogs/recovery"
)

// cloneBatch is how many blocks go to the server in one round. It bounds memory
// in WinPE, which lives in RAM, and what a lost request costs.
const cloneBatch = 32

// reportEvery limits progress reports; a clone makes many batches a second.
const reportEvery = 2 * time.Second

// restoreTries is how many times a clone tries to give the volume its size back.
const restoreTries = 5

var cloneStages = []string{"Сеть", "Сервер", "Чтение диска", "Завершение"}

// source is a disk being read. A clone must leave the PC as it was.
type source interface {
	// Table returns the partition table as it is now.
	Table() ([]byte, error)
	// Shrink makes the Windows volume as small as its data, like FOG, and reports
	// whether it did. Repair puts back a table from before a shrink and stretches the
	// file system over the partition again, like FOG after a capture. It may run any
	// number of times, also from another boot, and does only what is still missing.
	// It returns errStaleTable if the table no longer belongs to this disk. These
	// two are the only steps of a clone that change the disk.
	Shrink(log func(string)) bool
	Repair(table []byte) error
	// Describe names the disk for the log.
	Describe() string
	// Manifest describes the partition table.
	Manifest() proto.Manifest
	// UsedBlocks estimates how many blocks of a partition hold data.
	UsedBlocks(partition uint32, blockSize uint32) uint64
	// ReadBlock fills dst with the data at offset within a partition, unused
	// clusters zeroed. It returns false, without reading, if nothing there is used.
	ReadBlock(partition uint32, offset uint64, dst []byte) (bool, error)
	Close() error
}

// errStaleTable means a saved table describes another layout: the disk was
// replaced or deployed since, and the table must not be applied.
var errStaleTable = errors.New("сохранённая таблица разделов не подходит к диску")

// pending is a block read from the disk and waiting to be sent.
type pending struct {
	partition uint32
	position  uint32
	raw       []byte
	hash      proto.Sum
}

// runClone reads the disk into a new version of the image.
func (w *worker) runClone(task *proto.AgentTask) error {
	w.names = cloneStages
	w.setStage(stageCheck)
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseCapture, 0
		st.Center, st.Caption = "0%", "подготовка"
		st.Title, st.Detail = "Клонирование диска", "Образ "+task.Image+" · не выключайте компьютер"
	})

	src, err := w.openDisk()
	if err != nil {
		return fmt.Errorf("не удалось открыть диск: %w", err)
	}
	defer src.Close()
	w.screen.log("диск: " + src.Describe())

	// One clone per image, and the turn comes before the shrink: a volume must not
	// sit shrunk for as long as somebody else takes.
	err = w.waitTurn(task, func() error { return w.client.cloneClaim(task.ID) })
	if errors.Is(err, errTaskGone) {
		return w.cancelled()
	}
	if err != nil {
		return fmt.Errorf("сервер не дал очередь на образ: %w", err)
	}

	// Like FOG: the table is on the server before the disk changes, so the volume
	// can get its size back even if this PC loses power in the middle of the clone.
	table, err := src.Table()
	if err != nil {
		return fmt.Errorf("не удалось прочитать таблицу разделов: %w", err)
	}
	if err := w.client.saveTable(table); err != nil {
		return fmt.Errorf("сервер не сохранил таблицу разделов: %w", err)
	}
	shrunk := src.Shrink(w.screen.log)

	// Not even a bug in the capture may leave the volume shrunk.
	var m proto.Manifest
	err = recovery.Run(func() (err error) {
		m, err = w.capture(task, src)
		return err
	})
	if err != nil && !errors.Is(err, errTaskGone) {
		w.client.send(task.ID, "abort", "application/json", nil) // best effort: frees the server now
	}

	// The volume comes back before anything else happens, whatever the capture did:
	// the result of the task has to say whether it worked.
	var back error
	if shrunk {
		if back = w.giveBack(src, table); back != nil {
			back = fmt.Errorf("том эталона не восстановлен: %w", back)
			w.screen.log(back.Error())
		}
	} else {
		w.forgetTable()
	}
	switch {
	case back != nil && err != nil && !errors.Is(err, errTaskGone):
		return fmt.Errorf("%v; %w", err, back)
	case back != nil && errors.Is(err, errTaskGone):
		return back
	case errors.Is(err, errTaskGone):
		return w.cancelled()
	case err != nil:
		return err
	}

	w.setStage(stageFinish)
	res, err := w.client.cloneFinish(task.ID, m)
	if errors.Is(err, errTaskGone) {
		return w.cancelled()
	}
	if err != nil {
		return fmt.Errorf("сервер не принял образ: %w", err)
	}
	message := fmt.Sprintf("%s, %s", res.File, formatBytes(uint64(res.Size)))
	if back != nil {
		// The image is good and stays. The task still fails, so the panel shows that
		// this PC has work left: the table waits on the server for its next task.
		if err := w.finish(task.ID, false, "образ сохранён ("+message+"), но "+back.Error()); err != nil {
			return err
		}
		return back
	}
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log("сервер: сохранён " + message)
	w.showResult("Готово", "Образ "+task.Image+" сохранён: "+message)
	return nil
}

// capture sends the disk to the server and returns the table the image was taken
// with, which is the shrunk one.
func (w *worker) capture(task *proto.AgentTask, src source) (proto.Manifest, error) {
	info, err := w.client.cloneStart(task.ID)
	if errors.Is(err, errTaskGone) {
		return proto.Manifest{}, err
	}
	if err != nil {
		return proto.Manifest{}, fmt.Errorf("сервер не начал приём: %w", err)
	}
	w.screen.log(fmt.Sprintf("сервер: образ %s, версия %d", info.Image, info.Version))

	c := cloneRun{w: w, src: src, task: task.ID, blockSize: info.BlockSize, started: time.Now()}
	if err := c.readDisk(); err != nil {
		return proto.Manifest{}, err
	}
	return src.Manifest(), nil
}

// giveBack returns the volume its size, trying a few times: what fails here is
// usually Windows being busy with a volume that has just changed.
func (w *worker) giveBack(src source, table []byte) error {
	w.screen.log("возврат размера тома")
	var err error
	for try := range restoreTries {
		if try > 0 {
			w.sleep(retryDelay)
		}
		if err = src.Repair(table); err == nil {
			break
		}
		if errors.Is(err, errStaleTable) {
			w.screen.log(err.Error() + ", она удалена")
			err = nil
			break
		}
		w.screen.log("том не восстановлен: " + err.Error())
	}
	if err != nil {
		return err
	}
	w.forgetTable()
	return nil
}

// forgetTable removes the saved table. If the server cannot be told, the next task
// finds the volume whole and has nothing to do.
func (w *worker) forgetTable() {
	if err := w.client.saveTable(nil); err != nil {
		w.screen.log("сервер не удалил таблицу разделов: " + err.Error())
	}
}

// repair finishes what an earlier clone left: it died with the volume shrunk.
func (w *worker) repair(table []byte) error {
	w.screen.log("том остался сжатым после прошлого клонирования")
	src, err := w.openDisk()
	if err != nil {
		return fmt.Errorf("не удалось открыть диск: %w", err)
	}
	defer src.Close()
	if err := w.giveBack(src, table); err != nil {
		return fmt.Errorf("том эталона не восстановлен: %w", err)
	}
	return nil
}

func (w *worker) cancelled() error {
	w.screen.log("сервер: задача отменена")
	w.showResult("Задача отменена", "Компьютер загрузится с диска")
	return nil
}

// cloneRun is the state of one pass over the disk.
type cloneRun struct {
	w         *worker
	src       source
	task      int64
	blockSize uint32
	started   time.Time
	reported  time.Time
	total     uint64 // blocks expected to hold data
	done      uint64
	read      uint64 // bytes taken from the disk
	sent      uint64 // bytes put on the wire
	batch     []pending
}

// readDisk walks every stored partition block by block.
func (c *cloneRun) readDisk() error {
	m := c.src.Manifest()
	for _, p := range m.Partitions {
		if p.Content == proto.ContentBlocks {
			c.total += c.src.UsedBlocks(p.Number, c.blockSize)
		}
	}
	for _, p := range m.Partitions {
		if p.Content != proto.ContentBlocks {
			continue
		}
		c.w.screen.log(fmt.Sprintf("раздел %d: %s", p.Number, formatBytes(p.Size)))
		if err := c.readPartition(p); err != nil {
			return err
		}
	}
	return c.flush()
}

func (c *cloneRun) readPartition(p proto.Partition) error {
	size := uint64(c.blockSize)
	for offset := uint64(0); offset < p.Size; offset += size {
		raw := make([]byte, min(size, p.Size-offset))
		used, err := c.src.ReadBlock(p.Number, offset, raw)
		if err != nil {
			return fmt.Errorf("ошибка чтения раздела %d: %w", p.Number, err)
		}
		if !used {
			continue
		}
		block := pending{p.Number, uint32(offset / size), raw, proto.HashBlock(raw)}
		if isEmpty(raw) {
			// Used and all zeros: deploy must overwrite this place, so it stays in the
			// index as a zero entry, with no data.
			block.raw, block.hash = nil, proto.Sum{}
		}
		c.batch = append(c.batch, block)
		c.read += uint64(len(raw))
		if len(c.batch) == cloneBatch {
			if err := c.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

func isEmpty(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// flush sends a batch: first hashes, then only the blocks the server lacks.
func (c *cloneRun) flush() error {
	if len(c.batch) == 0 {
		return nil
	}
	hashes := make([]proto.Sum, len(c.batch))
	for i, b := range c.batch {
		hashes[i] = b.hash
	}
	missing, err := c.w.client.cloneCheck(c.task, hashes)
	if err != nil {
		return err
	}
	body, err := c.encode(missing)
	if err != nil {
		return err
	}
	if err := c.w.client.clonePut(c.task, body); err != nil {
		return err
	}
	c.sent += uint64(len(body))
	c.done += uint64(len(c.batch))
	c.batch = c.batch[:0]
	return c.report()
}

// encode compresses the missing blocks in parallel and builds the request.
func (c *cloneRun) encode(missing []byte) ([]byte, error) {
	records := make([]proto.PutRecord, len(c.batch))
	errs := make([]error, len(c.batch))
	slots := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	seen := map[proto.Sum]bool{}
	for i, b := range c.batch {
		records[i] = proto.PutRecord{Partition: b.partition, Position: b.position, Hash: b.hash}
		if b.raw == nil || missing[i] == 0 || seen[b.hash] {
			continue // equal blocks in one batch travel once
		}
		seen[b.hash] = true
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			errs[i] = recovery.Run(func() error { // a panic here would end the whole agent
				h, payload, err := proto.EncodeBlock(b.raw)
				records[i].Block, records[i].Payload = &h, payload
				return err
			})
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	var body []byte
	for _, r := range records {
		body = proto.AppendPut(body, r)
	}
	return body, nil
}

// report updates the screen and, now and then, the panel.
func (c *cloneRun) report() error {
	percent := 99
	if c.total > 0 {
		percent = min(int(c.done*100/c.total), 99)
	}
	seconds := max(time.Since(c.started).Seconds(), 0.001)
	caption := formatBytes(uint64(float64(c.read)/seconds)) + "/с"
	c.w.screen.update(func(st *ui.State) {
		st.Percent, st.Center, st.Caption = float64(percent), fmt.Sprintf("%d%%", percent), caption
	})
	if time.Since(c.reported) < reportEvery {
		return nil
	}
	c.reported = time.Now()
	message := fmt.Sprintf("прочитано %s, отправлено %s", formatBytes(c.read), formatBytes(c.sent))
	err := c.w.client.progress(c.task, percent, cloneStages[stageCheck], message)
	if errors.Is(err, errTaskGone) {
		return err
	}
	if err != nil {
		c.w.screen.log("отчёт не доставлен: " + err.Error())
	}
	return nil
}

// formatBytes shows a size for people.
func formatBytes(n uint64) string {
	const unit = 1024
	switch {
	case n >= unit*unit*unit:
		return fmt.Sprintf("%.1f ГБ", float64(n)/(unit*unit*unit))
	case n >= unit*unit:
		return fmt.Sprintf("%.0f МБ", float64(n)/(unit*unit))
	}
	return fmt.Sprintf("%d КБ", n/unit)
}
