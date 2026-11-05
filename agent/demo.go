package main

import (
	"fmt"
	"time"

	"fogs/agent/ui"
)

// demoCycle is how long a fake task takes to reach 100%.
const demoCycle = 90 * time.Second

// demoState fakes what a running task will report.
func demoState(phase ui.Phase, elapsed time.Duration) ui.State {
	s := ui.State{
		Phase:  phase,
		Host:   "PC-12 · 04:7c:16:e7:d2:1e",
		Server: "сервер 192.168.88.105",
		Hint:   "1-5 — состояние · Esc — выход",
	}
	steps := []string{"Разметка диска", "Раздел EFI", "Раздел C:", "Настройка хоста"}

	switch phase {
	case ui.PhaseIdle:
		s.Center, s.Caption, s.Percent = "Агент", "на связи", -1
		s.Title, s.Detail = "Подключено", "Ждём задачу от сервера"
		s.Log = []string{"сеть: адрес 192.168.88.104", "сервер: подключено", "задача: нет"}

	case ui.PhaseCapture, ui.PhaseDeploy:
		s.Percent = float64(elapsed%demoCycle) / float64(demoCycle) * 100
		s.Center = fmt.Sprintf("%.0f%%", s.Percent)
		s.Caption = fmt.Sprintf("112 МБ/с · %d мин", max(1, int((100-s.Percent)/12)))
		s.Title, s.Detail = "Установка образа", "win11-club · не выключайте компьютер"
		if phase == ui.PhaseCapture {
			s.Title = "Отправка образа"
			steps = []string{"Подготовка диска", "Раздел EFI", "Раздел C:", "Проверка на сервере"}
		}
		active := 3
		switch {
		case s.Percent < 8:
			active = 0
		case s.Percent < 14:
			active = 1
		case s.Percent < 92:
			active = 2
		}
		s.Stages = stages(steps, active, ui.StageActive)
		s.Log = []string{
			"разметка: GPT, диск 476 ГБ",
			"EFI: записано, CRC совпал",
			fmt.Sprintf("C: чанк %d из 39210", int(s.Percent*392.1)),
		}

	case ui.PhaseDone:
		s.Center, s.Caption, s.Percent = "100%", "5 мин 42 с", 100
		s.Title, s.Detail = "Готово", "Перезагрузка через 10 секунд"
		s.Stages = stages(steps, len(steps), ui.StageDone)
		s.Log = []string{"C: 38.7 ГБ записано", "имя хоста: PC-12", "задача завершена за 5 мин 42 с"}

	case ui.PhaseFailed:
		s.Center, s.Caption, s.Percent = "47%", "остановлено", 47
		s.Title, s.Detail = "Не удалось записать раздел C:", "Сервер уведомлён. R — повторить, C — командная строка"
		s.Stages = stages(steps, 2, ui.StageFailed)
		s.Log = []string{"C: чанк 18420: CRC не совпал", "повтор 3 из 3: ошибка", "задача остановлена"}
	}
	return s
}

// stages marks everything before index current as done and current as status.
func stages(names []string, current int, status ui.StageStatus) []ui.Stage {
	out := make([]ui.Stage, len(names))
	for i, name := range names {
		out[i].Name = name
		switch {
		case i < current:
			out[i].Status = ui.StageDone
		case i == current:
			out[i].Status = status
		}
	}
	return out
}
