package main

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"fogs/agent/ui"
	"fogs/proto"
)

// confirmSeconds is how long the screen warns before the disk is touched. It is
// the last chance to stop a deploy started on the wrong PC.
const confirmSeconds = 20

// imageWait is how often a waiting task asks whether its turn has come.
const imageWait = 10 * time.Second

var deployStages = []string{"Сеть", "Сервер", "Запись диска", "Завершение"}

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
	// MakeBootable adds Windows to the boot menu of the firmware, which a disk
	// image cannot carry. It runs after Close, with the partitions back in Windows.
	MakeBootable(m proto.Manifest) error
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

	dst, err := w.openTarget()
	if err != nil {
		return fmt.Errorf("не удалось открыть диск: %w", err)
	}
	defer dst.Close()
	w.screen.log("диск: " + dst.Describe())

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

	if !w.confirmWipe(dst, task.Image) {
		w.screen.log("отменено на компьютере")
		if err := w.finish(task.ID, false, "отменено на компьютере"); err != nil {
			return err
		}
		w.showResult("Отменено", "Диск не изменён, компьютер загрузится с него")
		return nil
	}

	w.screen.update(func(st *ui.State) { st.Hint = "Не выключайте компьютер" })
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
	if err := dst.Finish(); err != nil {
		return fmt.Errorf("не удалось завершить запись: %w", err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("не удалось завершить запись: %w", err)
	}
	if err := dst.Grow(fitted); err != nil {
		// Windows still boots, on a smaller volume.
		w.screen.log("том не расширен: " + err.Error())
	}
	// The image is on the disk either way: without this entry the PC still boots
	// through the network or the fallback loader, so a failure only goes to the log.
	if err := dst.MakeBootable(fitted); err != nil {
		w.screen.log("загрузочная запись: " + err.Error())
	}
	message := fmt.Sprintf("%s v%d, записано %s", task.Image, info.Version, formatBytes(d.written))
	if err := w.finish(task.ID, true, message); err != nil {
		return err
	}
	w.screen.log("готово: " + message)
	w.showResult("Готово", "Образ "+task.Image+" развёрнут")
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
func (w *worker) confirmWipe(dst target, image string) bool {
	w.screen.update(func(st *ui.State) {
		st.Title = "Диск будет стёрт"
		st.Detail = dst.Describe() + " → образ " + image
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
	w.screen.update(func(st *ui.State) {
		st.Title, st.Detail = "Развёртывание образа", "Образ "+image
	})
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

	var batch []uint32
	for i, e := range d.entries {
		if e.Zero() {
			if err := d.writeZero(e); err != nil {
				return err
			}
			continue
		}
		batch = append(batch, uint32(i))
		if len(batch) == cloneBatch {
			if err := d.flush(batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	return d.flush(batch)
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

func (d *deployRun) writeZero(e proto.DeployEntry) error {
	offset, length, err := d.place(e)
	if err != nil {
		return err
	}
	if err := d.dst.WriteBlock(e.Partition, offset, make([]byte, length)); err != nil {
		return fmt.Errorf("ошибка записи раздела %d: %w", e.Partition, err)
	}
	d.written += length
	d.done++
	return nil
}

// flush fetches a batch, checks every block against the list, and writes it.
func (d *deployRun) flush(batch []uint32) error {
	if len(batch) == 0 {
		return nil
	}
	body, err := d.w.client.deployBlocks(d.task, batch)
	if err != nil {
		return err
	}
	r := bytes.NewReader(body)
	for _, n := range batch {
		e := d.entries[n]
		h, payload, err := proto.ReadStored(r)
		if err != nil {
			return fmt.Errorf("сервер прислал неполные данные: %w", err)
		}
		raw, err := proto.DecodeBlock(h, payload)
		if err != nil {
			return fmt.Errorf("блок %d раздела %d повреждён: %w", e.Position, e.Partition, err)
		}
		if h.Hash != e.Hash {
			return fmt.Errorf("сервер прислал другой блок вместо %d раздела %d", e.Position, e.Partition)
		}
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
		d.done++
	}
	return d.report()
}

// report updates the screen and, now and then, the panel.
func (d *deployRun) report() error {
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
	err := d.w.client.progress(d.task, percent, deployStages[stageCheck], "записано "+formatBytes(d.written))
	if errors.Is(err, errTaskGone) {
		return err
	}
	if err != nil {
		d.w.screen.log("отчёт не доставлен: " + err.Error())
	}
	return nil
}
