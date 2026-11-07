package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"fogs/agent/ui"
	"fogs/proto"
	"fogs/recovery"
)

// confirmSeconds is how long the screen warns before the disk is touched. It is
// the last chance to stop a deploy started on the wrong PC.
const confirmSeconds = 20

// imageWait is how often a waiting task asks whether its turn has come.
const imageWait = 10 * time.Second

var deployStages = []string{"Сеть", "Сервер", "Запись диска", "Завершение"}

var formatStages = []string{"Сеть", "Сервер", "Форматирование", "Завершение"}

// target is a disk about to be overwritten.
type target interface {
	Describe() string
	// Fit builds the table of the image for this disk, or refuses a disk that cannot
	// hold it. It changes nothing.
	Fit(m proto.Manifest) (proto.Manifest, error)
	// Prepare takes the disk from Windows and writes the partition table. From
	// here on the old contents are lost.
	Prepare(m proto.Manifest) error
	// WriteBlock writes data at offset within a partition.
	WriteBlock(partition uint32, offset uint64, data []byte) error
	// Finish flushes everything to the disk.
	Finish() error
	Close() error
	// Grow stretches the file system over the partition. It runs after Close.
	Grow(m proto.Manifest) error
	// Format wipes the disk and leaves one empty NTFS volume with this label on all
	// of it, without Prepare. Nothing else may hold the disk.
	Format(label string) error
	// MakeBootable adds Windows to the boot menu of the firmware, which a disk
	// image cannot carry. It runs after Close, with the partitions back in Windows.
	MakeBootable(m proto.Manifest) error
	// SetName gives the Windows on the disk its computer name. It runs after
	// MakeBootable, which mounts that Windows.
	SetName(name string) error
}

// diskInfo is what is known about a disk before choosing one to overwrite.
type diskInfo struct {
	number   int
	internal bool // inside the PC: not a network disk, USB, a card or a VHD
	hasESP   bool // GPT with an EFI system partition: a disk Windows boots from
	empty    bool // no partitions at all
}

// chooseTarget picks the disk to overwrite. A disk Windows boots from comes first,
// by the same rule as a clone. Without one the PC has a new or a foreign disk, which
// is fine as long as there is no doubt which one: the only disk, or the only empty
// one among several. Otherwise a data disk could be wiped, so the deploy stops.
func chooseTarget(disks []diskInfo) (int, error) {
	var inside, empty []int
	for _, d := range disks {
		if !d.internal {
			continue
		}
		if d.hasESP {
			return d.number, nil
		}
		inside = append(inside, d.number)
		if d.empty {
			empty = append(empty, d.number)
		}
	}
	switch {
	case len(inside) == 0:
		return 0, errors.New("в компьютере не найден внутренний диск")
	case len(inside) == 1:
		return inside[0], nil
	case len(empty) == 1:
		return empty[0], nil
	}
	return 0, fmt.Errorf("внутренних дисков %d, и ни один не загрузочный: непонятно, какой стирать", len(inside))
}

// gptArea returns where partitions may lie on a disk that gets a new GPT: after the
// protective MBR, the header and 16 KiB of entries, and before their backup copy.
func gptArea(size uint64, sector uint32) (uint64, uint64) {
	entries := uint64(16384)
	first := 2*uint64(sector) + entries
	return first, size - entries - uint64(sector)
}

// runDeploy writes the image over the disk.
func (w *worker) runDeploy(task *proto.AgentTask) error {
	w.names = deployStages
	w.setStage(stageCheck)
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseDeploy, 0
		st.Center, st.Caption = "0%", "подготовка"
		st.Title, st.Detail = "Развёртывание образа", "Образ "+task.Image
	})
	w.rec = newRecorder(w.attempts)
	w.rec.Step("Открытие диска")

	dst, err := w.openTarget()
	if err != nil {
		return fmt.Errorf("не удалось открыть диск: %w", err)
	}
	defer dst.Close()
	w.screen.log("диск: " + dst.Describe())

	w.rec.Step("Ожидание очереди на образ")
	var info proto.DeployInfo
	err = w.waitTurn(task, func() (err error) {
		info, err = w.client.deployStart(task.ID)
		return err
	})
	if errors.Is(err, errTaskGone) {
		return w.cancelled()
	}
	if err != nil {
		return fmt.Errorf("сервер не начал отдачу: %w", err)
	}
	// Best effort: however this ends, the server can close the image files now.
	defer w.client.post2(w.client.deployURL(task.ID, "abort"), "application/json", nil)
	fitted, err := dst.Fit(info.Manifest)
	if err != nil {
		return err
	}

	w.rec.Step("Отсчёт перед стиранием")
	if !w.confirmWipe(dst, "образ "+task.Image) {
		w.screen.log("отменено на компьютере")
		if err := w.finish(task.ID, false, "отменено на компьютере"); err != nil {
			return err
		}
		w.showResult("Отменено", "Диск не изменён, компьютер загрузится с него")
		return nil
	}

	w.screen.update(func(st *ui.State) { st.Hint = "Не выключайте компьютер" })
	w.rec.Step("Таблица разделов")
	if err := dst.Prepare(fitted); err != nil {
		return fmt.Errorf("не удалось подготовить диск: %w", err)
	}
	w.screen.log("таблица разделов записана")
	if len(task.Table) > 0 {
		w.forgetTable() // the volume it was saved for is gone
	}

	d := deployRun{w: w, dst: dst, task: task.ID, info: info, started: time.Now()}
	switch err := d.write(); {
	case errors.Is(err, errTaskGone):
		// The disk is already overwritten in part: it must not look finished.
		w.screen.log("сервер: задача отменена, диск записан не полностью")
		w.showResult("Задача отменена", "Диск записан не полностью, Windows может не загрузиться")
		return nil
	case err != nil:
		return err
	}

	w.setStage(stageFinish)
	w.rec.Step("Сброс записи на диск")
	if err := dst.Finish(); err != nil {
		return fmt.Errorf("не удалось завершить запись: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("не удалось завершить запись: %w", err)
	}
	writing := time.Since(d.started)
	w.rec.Step("Расширение тома")
	if err := dst.Grow(fitted); err != nil {
		// Windows still boots, on a smaller volume.
		w.screen.log("том не расширен: " + err.Error())
	}
	// The image is on the disk either way: without this entry the PC still boots
	// through the network or the fallback loader, so a failure only goes to the log.
	w.rec.Step("Загрузочная запись")
	if err := dst.MakeBootable(fitted); err != nil {
		w.screen.log("загрузочная запись: " + err.Error())
	}
	w.rec.Step("Имя ПК")
	named := w.nameWindows(dst)
	w.rec.Step("Отчёт серверу")
	message := fmt.Sprintf("%s v%d, записано %s за %s, %s", task.Image, info.Version, formatBytes(d.written),
		formatDuration(writing), named)
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log("готово: " + message)
	w.showResult("Готово", "Образ "+task.Image+" развёрнут")
	return nil
}

// nameWindows gives the deployed Windows the name the PC has in the panel and says
// how that went. Windows boots under the name of the image without it, so a failure
// does not fail the deploy, but the panel has to show it.
func (w *worker) nameWindows(dst target) string {
	err := checkComputerName(w.cfg.Host)
	if err == nil {
		err = dst.SetName(w.cfg.Host)
	}
	if err != nil {
		return "имя ПК не задано: " + err.Error()
	}
	return "имя ПК " + w.cfg.Host
}

// maxComputerName is the limit of a NetBIOS name, which Windows still enforces.
const maxComputerName = 15

// checkComputerName refuses what Windows would not accept as a computer name. The
// panel allows any name, and changing it here would hide that from the operator.
func checkComputerName(name string) error {
	if name == "" || len(name) > maxComputerName {
		return fmt.Errorf("%q: нужно от 1 до %d символов", name, maxComputerName)
	}
	digits := true
	for _, c := range []byte(name) {
		isDigit := c >= '0' && c <= '9'
		digits = digits && isDigit
		if !isDigit && c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return fmt.Errorf("%q: только латиница, цифры и дефис", name)
		}
	}
	if digits || name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("%q: не может быть из одних цифр, начинаться или кончаться дефисом", name)
	}
	return nil
}

// controlSet finds the control set Windows boots with in what reg query prints for
// the value Current of the key Select.
func controlSet(query string) (string, error) {
	fields := strings.Fields(query)
	for i, f := range fields[:max(len(fields)-1, 0)] {
		if f == "REG_DWORD" {
			n, err := strconv.ParseUint(fields[i+1], 0, 32)
			if err != nil {
				break
			}
			return fmt.Sprintf("ControlSet%03d", n), nil
		}
	}
	return "", errors.New("в реестре не найден текущий ControlSet")
}

// runFormat wipes the disk into one empty volume.
func (w *worker) runFormat(task *proto.AgentTask) error {
	w.names = formatStages
	w.setStage(stageCheck)
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseDeploy, 0
		st.Center, st.Caption = "0%", "подготовка"
		st.Title, st.Detail = "Форматирование диска", "Том "+proto.FormatLabel
	})
	w.rec = newRecorder(w.attempts)
	w.rec.Step("Открытие диска")
	dst, err := w.openTarget()
	if err != nil {
		return fmt.Errorf("не удалось открыть диск: %w", err)
	}
	defer dst.Close()
	w.screen.log("диск: " + dst.Describe())

	w.rec.Step("Отсчёт перед стиранием")
	if !w.confirmWipe(dst, "том "+proto.FormatLabel) {
		w.screen.log("отменено на компьютере")
		if err := w.finish(task.ID, false, "отменено на компьютере"); err != nil {
			return err
		}
		w.showResult("Отменено", "Диск не изменён")
		return nil
	}
	w.screen.update(func(st *ui.State) {
		st.Center, st.Caption, st.Hint = "…", "форматирование", "Не выключайте компьютер"
	})
	w.rec.Step("Разметка и форматирование")
	if err := dst.Format(proto.FormatLabel); err != nil {
		return fmt.Errorf("не удалось отформатировать диск: %w", err)
	}
	if len(task.Table) > 0 {
		w.forgetTable() // the volume it was saved for is gone
	}
	w.setStage(stageFinish)
	w.rec.Step("Отчёт серверу")
	message := dst.Describe() + ", том " + proto.FormatLabel
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log("готово: " + message)
	w.showResult("Готово", "Диск отформатирован: том "+proto.FormatLabel)
	return nil
}

// waitTurn repeats ask while another PC has the image. The disk is still as it was,
// so waiting costs nothing.
func (w *worker) waitTurn(task *proto.AgentTask, ask func() error) error {
	var before ui.State
	waiting := false
	for {
		err := ask()
		if !errors.Is(err, errImageBusy) {
			if waiting {
				w.screen.log("очередь подошла")
				w.screen.update(func(st *ui.State) {
					st.Title, st.Detail, st.Center, st.Caption = before.Title, before.Detail, before.Center, before.Caption
				})
			}
			return err
		}
		if !waiting {
			waiting = true
			before = w.screen.snapshot()
			w.screen.log("образ занят другим ПК, ожидание")
			w.screen.update(func(st *ui.State) {
				st.Title, st.Detail = "Ожидание очереди", "Образ "+task.Image+" занят другим компьютером, диск не изменён"
				st.Center, st.Caption = "…", "ожидание"
			})
		}
		// The report is also what tells a waiting agent that the task was cancelled.
		err = w.client.progress(task.ID, 0, proto.StageWaiting, "ждёт очереди на образ "+task.Image)
		if errors.Is(err, errTaskGone) {
			return err
		}
		if err != nil {
			w.screen.log("отчёт не доставлен: " + err.Error())
		}
		w.sleep(imageWait)
	}
}

// confirmWipe warns on the screen and returns false if the operator pressed Esc.
func (w *worker) confirmWipe(dst target, result string) bool {
	before := w.screen.snapshot()
	w.screen.update(func(st *ui.State) {
		st.Title = "Диск будет стёрт"
		st.Detail = dst.Describe() + " → " + result
	})
	for left := confirmSeconds; left > 0; left-- {
		w.screen.update(func(st *ui.State) {
			st.Center, st.Caption = fmt.Sprint(left), "до начала записи"
			st.Hint = "Esc — отменить"
		})
		select {
		case <-w.abort:
			return false
		default:
		}
		w.sleep(time.Second)
	}
	w.screen.update(func(st *ui.State) { st.Title, st.Detail = before.Title, before.Detail })
	return true
}

// deployRun is the state of one pass over the image.
type deployRun struct {
	w        *worker
	dst      target
	task     int64
	info     proto.DeployInfo
	started  time.Time
	reported time.Time
	entries  []proto.DeployEntry
	done     int
	written  uint64
	sizes    map[uint32]uint64
}

// write fills every place of the list on the disk.
func (d *deployRun) write() error {
	rec := d.w.rec
	rec.Step("Список блоков")
	var err error
	if d.entries, err = d.w.client.deployList(d.task); err != nil {
		return err
	}
	d.sizes = map[uint32]uint64{}
	for _, p := range d.info.Manifest.Partitions {
		if p.Content == proto.ContentBlocks {
			d.sizes[p.Number] = p.Size
		}
	}

	// Every place of the list is a different one, so the order of writing is free.
	var batches [][]uint32
	var batch []uint32
	zeros := make([]byte, d.info.BlockSize)
	rec.Step("Запись нулевых блоков")
	for i, e := range d.entries {
		if e.Zero() {
			if err := d.writeZero(e, zeros); err != nil {
				return err
			}
			rec.Count("нулевых блоков", 1)
			continue
		}
		rec.Count("блоков с данными", 1)
		batch = append(batch, uint32(i))
		if len(batch) == cloneBatch {
			batches = append(batches, batch)
			batch = nil
		}
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	rec.Step("Загрузка и запись блоков")
	return d.pipeline(batches)
}

// Kinds of work of a deploy, as the metrics name them.
const (
	workFetch       = "загрузка с сервера"
	workDecode      = "распаковка и проверка"
	workWrite       = "запись на диск"
	workUnpackWaits = "распаковка ждёт запись"
	workWriteWaits  = "запись ждёт загрузку"
)

// pipeline keeps the network, the processor and the disk busy at once: one batch is
// on its way from the server while the one before it is unpacked and the one before
// that is written. Only this goroutine writes. Nothing runs on after it returns.
func (d *deployRun) pipeline(batches [][]uint32) error {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()

	fetched := make(chan deployBatch, 1)
	unpacked := make(chan deployBatch, 1)
	free := newSlabs(d.info.BlockSize)
	wg.Go(func() { d.fetch(ctx, batches, fetched) })
	wg.Go(func() { d.unpack(ctx, fetched, unpacked, free) })

	rec := d.w.rec
	for {
		waited := rec.Time(workWriteWaits)
		b, ok := <-unpacked
		waited()
		if !ok {
			return nil
		}
		if b.err != nil {
			return b.err
		}
		written := rec.Time(workWrite)
		err := d.store(b)
		written()
		if err != nil {
			return err
		}
		free <- b.slab
	}
}

// deployBatch is one batch on its way through the pipeline. A stage that fails puts
// its error here and stops, so the error arrives behind the batches before it.
type deployBatch struct {
	numbers []uint32
	body    []byte   // the stored blocks, as the server sent them
	slab    []byte   // where the blocks are unpacked to
	raw     [][]byte // the blocks, checked and ready to write
	err     error
}

// fetch downloads the batches one request at a time.
func (d *deployRun) fetch(ctx context.Context, batches [][]uint32, out chan<- deployBatch) {
	defer close(out)
	for _, numbers := range batches {
		b := deployBatch{numbers: numbers}
		fetched := d.w.rec.Time(workFetch)
		b.err = recovery.Run(func() (err error) { // a panic here would end the whole agent
			b.body, err = d.w.client.deployBlocks(ctx, d.task, numbers)
			return err
		})
		fetched()
		d.w.rec.Bytes("загружено по сети", uint64(len(b.body)))
		d.w.rec.Count("пачек", 1)
		select {
		case out <- b:
		case <-ctx.Done():
			return
		}
		if b.err != nil {
			return
		}
	}
}

// unpack checks and unpacks each batch. It waits for a free slab, which is what
// holds the network back when the disk is the slowest.
func (d *deployRun) unpack(ctx context.Context, in <-chan deployBatch, out chan<- deployBatch, free <-chan []byte) {
	defer close(out)
	for b := range in {
		if b.err == nil {
			waited := d.w.rec.Time(workUnpackWaits)
			select {
			case b.slab = <-free:
				waited()
			case <-ctx.Done():
				return
			}
			decoded := d.w.rec.Time(workDecode)
			b.err = recovery.Run(func() error { return d.decode(&b) })
			decoded()
		}
		select {
		case out <- b:
		case <-ctx.Done():
			return
		}
		if b.err != nil {
			return
		}
	}
}

// decode unpacks the blocks of a batch in parallel and checks each against the list.
func (d *deployRun) decode(b *deployBatch) error {
	b.raw = make([][]byte, len(b.numbers))
	errs := make([]error, len(b.numbers))
	slots := make(chan struct{}, runtime.NumCPU())
	size := int(d.info.BlockSize)
	var wg sync.WaitGroup
	defer wg.Wait()
	r := bytes.NewReader(b.body)
	for i, n := range b.numbers {
		e := d.entries[n]
		h, payload, err := proto.ReadStored(r)
		if err != nil {
			return fmt.Errorf("сервер прислал неполные данные: %w", err)
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			errs[i] = recovery.Run(func() error { // a panic here would end the whole agent
				raw, err := proto.DecodeBlockInto(b.slab[i*size:i*size:(i+1)*size], h, payload)
				if err != nil {
					return fmt.Errorf("блок %d раздела %d повреждён: %w", e.Position, e.Partition, err)
				}
				if h.Hash != e.Hash {
					return fmt.Errorf("сервер прислал другой блок вместо %d раздела %d", e.Position, e.Partition)
				}
				b.raw[i] = raw
				return nil
			})
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}

// place returns where an entry goes and how long the block must be.
func (d *deployRun) place(e proto.DeployEntry) (uint64, uint64, error) {
	size, ok := d.sizes[e.Partition]
	offset := uint64(e.Position) * uint64(d.info.BlockSize)
	if !ok || offset >= size {
		return 0, 0, fmt.Errorf("блок %d раздела %d вне образа", e.Position, e.Partition)
	}
	return offset, min(uint64(d.info.BlockSize), size-offset), nil
}

func (d *deployRun) writeZero(e proto.DeployEntry, zeros []byte) error {
	offset, length, err := d.place(e)
	if err != nil {
		return err
	}
	if err := d.dst.WriteBlock(e.Partition, offset, zeros[:length]); err != nil {
		return fmt.Errorf("ошибка записи раздела %d: %w", e.Partition, err)
	}
	d.written += length
	d.w.rec.Bytes("записано на диск", length)
	d.done++
	return nil
}

// store writes the checked blocks of a batch to the disk.
func (d *deployRun) store(b deployBatch) error {
	for i, n := range b.numbers {
		e := d.entries[n]
		raw := b.raw[i]
		offset, length, err := d.place(e)
		if err != nil {
			return err
		}
		if uint64(len(raw)) != length {
			return fmt.Errorf("блок %d раздела %d: %d байт вместо %d", e.Position, e.Partition, len(raw), length)
		}
		if err := d.dst.WriteBlock(e.Partition, offset, raw); err != nil {
			return fmt.Errorf("ошибка записи раздела %d: %w", e.Partition, err)
		}
		d.written += length
		d.w.rec.Bytes("записано на диск", length)
		d.done++
	}
	return d.report()
}

// report updates the screen and, now and then, the panel.
func (d *deployRun) report() error {
	d.w.rec.Sample()
	percent := min(d.done*100/max(len(d.entries), 1), 99)
	seconds := max(time.Since(d.started).Seconds(), 0.001)
	caption := formatBytes(uint64(float64(d.written)/seconds)) + "/с"
	d.w.screen.update(func(st *ui.State) {
		st.Percent, st.Center, st.Caption = float64(percent), fmt.Sprintf("%d%%", percent), caption
	})
	if time.Since(d.reported) < reportEvery {
		return nil
	}
	d.reported = time.Now()
	err := d.w.client.progress(d.task, percent, deployStages[stageCheck], "записано "+formatBytes(d.written)+", "+caption)
	if errors.Is(err, errTaskGone) {
		return err
	}
	if err != nil {
		d.w.screen.log("отчёт не доставлен: " + err.Error())
	}
	return nil
}
