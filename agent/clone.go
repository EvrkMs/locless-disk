package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"fogs/agent/ui"
	"fogs/proto"
	"fogs/recovery"
)

// cloneBatch is how many blocks go to the server in one round. It bounds memory
// in WinPE, which lives in RAM, and what a lost request costs.
const cloneBatch = 32

// pipelineSlabs is how many batches of raw blocks a clone or a deploy holds at once:
// one for each stage that works on them, and one waiting in between.
const pipelineSlabs = 3

// newSlabs returns the buffers of a pipeline, each with room for a whole batch. They
// go round instead of a new buffer for every block, and a stage with none free waits,
// so a slow disk or network cannot fill the memory of WinPE.
func newSlabs(blockSize uint32) chan []byte {
	free := make(chan []byte, pipelineSlabs)
	for range pipelineSlabs {
		free <- make([]byte, cloneBatch*int(blockSize))
	}
	return free
}

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
	w.rec = newRecorder(w.attempts)
	w.rec.Step("Открытие диска")

	src, err := w.openDisk()
	if err != nil {
		return fmt.Errorf("не удалось открыть диск: %w", err)
	}
	defer src.Close()
	w.screen.log("диск: " + src.Describe())
	w.rec.Note("Диск", src.Describe())

	// One clone per image, and the turn comes before the shrink: a volume must not
	// sit shrunk for as long as somebody else takes.
	w.rec.Step("Ожидание очереди на образ")
	err = w.waitTurn(task, func() error { return w.client.cloneClaim(task.ID) })
	if errors.Is(err, errTaskGone) {
		return w.cancelled()
	}
	if err != nil {
		return fmt.Errorf("сервер не дал очередь на образ: %w", err)
	}

	// Like FOG: the table is on the server before the disk changes, so the volume
	// can get its size back even if this PC loses power in the middle of the clone.
	w.rec.Step("Сжатие тома")
	table, err := src.Table()
	if err != nil {
		return fmt.Errorf("не удалось прочитать таблицу разделов: %w", err)
	}
	if err := w.client.saveTable(table); err != nil {
		return fmt.Errorf("сервер не сохранил таблицу разделов: %w", err)
	}
	shrunk := src.Shrink(w.shrinkLog())
	if !shrunk {
		w.rec.Note("Сжатие тома", "не сжат"+w.shrinkWhy())
	}

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
	w.rec.Step("Возврат размера тома")
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
	w.rec.Step("Проверка и сохранение на сервере")
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
	w.rec.Step("Чтение диска и отправка")
	info, err := w.client.cloneStart(task.ID)
	if errors.Is(err, errTaskGone) {
		return proto.Manifest{}, err
	}
	if err != nil {
		return proto.Manifest{}, fmt.Errorf("сервер не начал приём: %w", err)
	}
	w.screen.log(fmt.Sprintf("сервер: образ %s, версия %d", info.Image, info.Version))

	c := cloneRun{w: w, src: src, task: task.ID, blockSize: info.BlockSize, started: time.Now()}
	c.speeds.update(c.started, "", 0, w.client.traffic) // the start, for the first speed
	if err := c.readDisk(); err != nil {
		return proto.Manifest{}, err
	}
	return src.Manifest(), nil
}

// shrinkLog passes what Shrink says to the screen and keeps the last word about
// the volume for the metrics: whether it was shrunk, and from what size to what.
func (w *worker) shrinkLog() func(string) {
	w.shrinkSaid = ""
	return func(line string) {
		w.screen.log(line)
		if strings.HasPrefix(line, "сжатие") || strings.HasPrefix(line, "том сжат") {
			w.shrinkSaid = line
			w.rec.Note("Сжатие тома", line)
		}
	}
}

// shrinkWhy is the reason Shrink gave for leaving the volume, if any.
func (w *worker) shrinkWhy() string {
	if w.shrinkSaid == "" {
		return ""
	}
	return ": " + w.shrinkSaid
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
	speeds    speeds
}

// readBatch is what the disk gave for one round. An error ends the reading and
// arrives behind the batches before it.
type readBatch struct {
	blocks []pending
	slab   []byte // holds the raw data of the blocks
	read   uint64 // bytes taken from the disk
	err    error
}

// readDisk sends every stored partition through three stages that run at once.
// The reader takes blocks off the disk; the packer hashes them, asks the server
// which it lacks and compresses those into a request; this goroutine sends the
// requests. On a whole clone the network is the slowest part, so it is kept busy
// while the next batch is packed; on a delta the disk is, and the hashing no longer
// holds it up. The reading has ended by the time this returns: the volume gets its
// size back right after, and nothing may touch the disk meanwhile.
func (c *cloneRun) readDisk() error {
	m := c.src.Manifest()
	for _, p := range m.Partitions {
		if p.Content == proto.ContentBlocks {
			c.total += c.src.UsedBlocks(p.Number, c.blockSize)
		}
	}

	stop := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel() // a check under way gives up instead of holding the volume shrunk
	defer close(stop)

	batches := make(chan readBatch, 1)
	packed := make(chan sendBatch, 1)
	free := newSlabs(c.blockSize)
	r := diskReader{c: c, stop: stop, out: batches, free: free}
	wg.Go(func() {
		defer close(batches)
		// A panic here would end the whole agent with the volume shrunk.
		err := recovery.Run(func() error { return r.run(m) })
		if err != nil && !errors.Is(err, errReadStopped) {
			select {
			case batches <- readBatch{err: err}:
			case <-stop:
			}
		}
	})
	p := packer{c: c, ctx: ctx, stop: stop, in: batches, out: packed, free: free, zeros: make([]byte, c.blockSize)}
	p.bufs.New = func() any {
		b := make([]byte, 0, proto.MaxEncodedSize(int(c.blockSize)))
		return &b
	}
	wg.Go(func() {
		defer close(packed)
		err := recovery.Run(p.run)
		if err != nil && !errors.Is(err, errReadStopped) {
			select {
			case packed <- sendBatch{err: err}:
			case <-stop:
			}
		}
	})

	for {
		waited := c.w.rec.Time(workSendWaits)
		b, ok := <-packed
		waited()
		if !ok {
			return nil
		}
		if b.err != nil {
			return b.err
		}
		if err := c.send(b); err != nil {
			return err
		}
	}
}

// Kinds of work of a clone, as the metrics name them.
const (
	workDiskRead      = "чтение диска"
	workReadWaits     = "чтение ждёт подготовку"
	workHash          = "хэш SHA-256"
	workCheck         = "проверка хэшей на сервере"
	workCompress      = "сжатие zstd"
	workBuild         = "сборка запроса"
	workPackWaits     = "подготовка ждёт чтение"
	workPackSendWaits = "подготовка ждёт отправку"
	workSendWaits     = "отправка ждёт подготовку"
	workUpload        = "передача на сервер"
)

// errReadStopped means nobody wants the batches any more.
var errReadStopped = errors.New("чтение диска остановлено")

// diskReader walks the partitions block by block on a goroutine of its own. It
// only reads: telling zeros from data and hashing is the work of the packer.
type diskReader struct {
	c     *cloneRun
	stop  <-chan struct{}
	out   chan<- readBatch
	free  <-chan []byte
	batch readBatch
}

func (r *diskReader) run(m proto.Manifest) error {
	for _, p := range m.Partitions {
		if p.Content != proto.ContentBlocks {
			continue
		}
		r.c.w.screen.log(fmt.Sprintf("раздел %d: %s", p.Number, formatBytes(p.Size)))
		if err := r.readPartition(p); err != nil {
			return err
		}
	}
	if len(r.batch.blocks) == 0 {
		return nil
	}
	return r.send()
}

func (r *diskReader) readPartition(p proto.Partition) error {
	size := uint64(r.c.blockSize)
	rec := r.c.w.rec
	for offset := uint64(0); offset < p.Size; offset += size {
		if r.batch.slab == nil {
			waited := rec.Time(workReadWaits)
			select {
			case r.batch.slab = <-r.free:
				waited()
			case <-r.stop:
				return errReadStopped
			}
		}
		start := uint64(len(r.batch.blocks)) * size
		raw := r.batch.slab[start : start+min(size, p.Size-offset)]
		read := rec.Time(workDiskRead)
		used, err := r.c.src.ReadBlock(p.Number, offset, raw)
		read()
		if err != nil {
			return fmt.Errorf("ошибка чтения раздела %d: %w", p.Number, err)
		}
		if !used {
			rec.Count("свободных блоков пропущено", 1)
			continue
		}
		rec.Bytes("прочитано с диска", uint64(len(raw)))
		r.batch.blocks = append(r.batch.blocks, pending{partition: p.Number, position: uint32(offset / size), raw: raw})
		r.batch.read += uint64(len(raw))
		if len(r.batch.blocks) == cloneBatch {
			if err := r.send(); err != nil {
				return err
			}
		}
	}
	return nil
}

// send hands the batch over and starts a new one.
func (r *diskReader) send() error {
	defer r.c.w.rec.Time(workReadWaits)()
	select {
	case r.out <- r.batch:
		r.batch = readBatch{}
		return nil
	case <-r.stop:
		return errReadStopped
	}
}

// sendBatch is a request ready for the server.
type sendBatch struct {
	body   []byte
	blocks uint64 // places of the disk it covers
	read   uint64 // bytes taken from the disk for it
	err    error
}

// packer turns batches read from the disk into requests, on a goroutine of its own.
type packer struct {
	c     *cloneRun
	ctx   context.Context
	stop  <-chan struct{}
	in    <-chan readBatch
	out   chan<- sendBatch
	free  chan<- []byte
	zeros []byte    // a block of zeros to compare with
	bufs  sync.Pool // *[]byte with room for one compressed block, used again
}

func (p *packer) run() error {
	rec := p.c.w.rec
	for {
		waited := rec.Time(workPackWaits)
		b, ok := <-p.in
		waited()
		if !ok {
			return nil
		}
		if b.err != nil {
			return b.err
		}
		body, err := p.pack(b.blocks)
		if b.slab != nil {
			p.free <- b.slab // the request holds its own copy of everything
		}
		if err != nil {
			return err
		}
		waited = rec.Time(workPackSendWaits)
		select {
		case p.out <- sendBatch{body: body, blocks: uint64(len(b.blocks)), read: b.read}:
			waited()
		case <-p.stop:
			return errReadStopped
		}
	}
}

// pack hashes a batch, asks the server which blocks it lacks and builds the
// request: hashes for every place, data only for the missing blocks.
func (p *packer) pack(blocks []pending) ([]byte, error) {
	rec := p.c.w.rec
	// Used and all zeros: deploy must overwrite such a place, so it stays in the
	// index as a zero entry, with no data.
	hashed := rec.Time(workHash)
	err := forEach(len(blocks), func(i int) error {
		b := &blocks[i]
		if bytes.Equal(b.raw, p.zeros[:len(b.raw)]) {
			b.raw = nil
		} else {
			b.hash = proto.HashBlock(b.raw)
		}
		return nil
	})
	hashed()
	if err != nil {
		return nil, err
	}
	hashes := make([]proto.Sum, len(blocks))
	for i, b := range blocks {
		hashes[i] = b.hash
		if b.raw == nil {
			rec.Count("нулевых блоков", 1)
		} else {
			rec.Count("блоков с данными", 1)
		}
	}
	checked := rec.Time(workCheck)
	missing, err := p.c.w.client.cloneCheck(p.ctx, p.c.task, hashes)
	checked()
	if err != nil {
		return nil, err
	}
	return p.encode(blocks, missing)
}

// encode compresses the missing blocks in parallel and builds the request in one
// buffer of the right size. The request holds its own copy of everything, so the
// blocks and the compression buffers are free afterwards.
func (p *packer) encode(blocks []pending, missing []byte) ([]byte, error) {
	rec := p.c.w.rec
	records := make([]proto.PutRecord, len(blocks))
	send := make([]bool, len(blocks))
	seen := map[proto.Sum]bool{}
	for i, b := range blocks {
		records[i] = proto.PutRecord{Partition: b.partition, Position: b.position, Hash: b.hash}
		switch {
		case b.raw == nil:
		case missing[i] == 0:
			rec.Count("блоков уже было на сервере", 1)
		case seen[b.hash]:
			rec.Count("повторов внутри пачки", 1) // equal blocks in one batch travel once
		default:
			rec.Count("новых блоков отправлено", 1)
			seen[b.hash], send[i] = true, true
		}
	}

	bufs := make([]*[]byte, len(blocks))
	defer func() {
		for _, buf := range bufs {
			if buf != nil {
				p.bufs.Put(buf)
			}
		}
	}()
	compressed := rec.Time(workCompress)
	err := forEach(len(blocks), func(i int) error {
		if !send[i] {
			return nil
		}
		bufs[i] = p.bufs.Get().(*[]byte)
		h, payload, err := proto.EncodeHashedInto((*bufs[i])[:0], blocks[i].raw, blocks[i].hash)
		records[i].Block, records[i].Payload = &h, payload
		return err
	})
	compressed()
	if err != nil {
		return nil, err
	}

	defer rec.Time(workBuild)()
	size := 0
	for _, r := range records {
		size += proto.PutRecordSize(r)
	}
	body := make([]byte, 0, size)
	for _, r := range records {
		body = proto.AppendPut(body, r)
	}
	return body, nil
}

// forEach runs f for 0..n-1 on all cores and returns the errors. A panic in f is an
// error, as it would otherwise end the whole agent.
func forEach(n int, f func(i int) error) error {
	errs := make([]error, n)
	slots := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for i := range n {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			errs[i] = recovery.Run(func() error { return f(i) })
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// send puts a request on the server and counts what it carried.
func (c *cloneRun) send(b sendBatch) error {
	rec := c.w.rec
	uploaded := rec.Time(workUpload)
	err := c.w.client.clonePut(c.task, b.body)
	uploaded()
	if err != nil {
		return err
	}
	rec.Bytes("отправлено по сети", uint64(len(b.body)))
	rec.Count("пачек", 1)
	c.read += b.read
	c.sent += uint64(len(b.body))
	c.done += b.blocks
	return c.report()
}

// report updates the screen and, now and then, the panel.
func (c *cloneRun) report() error {
	c.w.rec.Sample()
	percent := 99
	if c.total > 0 {
		percent = min(int(c.done*100/c.total), 99)
	}
	rates, summary := c.speeds.update(time.Now(), "Чтение диска", c.read, c.w.client.traffic)
	caption := "прочитано " + formatBytes(c.read)
	c.w.screen.update(func(st *ui.State) {
		st.Percent, st.Center, st.Caption = float64(percent), fmt.Sprintf("%d%%", percent), caption
		st.Rates = rates
	})
	if time.Since(c.reported) < reportEvery {
		return nil
	}
	c.reported = time.Now()
	message := fmt.Sprintf("прочитано %s, отправлено %s · %s", formatBytes(c.read), formatBytes(c.sent), summary)
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

// formatDuration shows how long a task took, to the second.
func formatDuration(d time.Duration) string {
	s := int(d.Round(time.Second).Seconds())
	if s < 60 {
		return fmt.Sprintf("%d с", s)
	}
	return fmt.Sprintf("%d мин %d с", s/60, s%60)
}
