package proto

// AgentConfigName is the file the server injects next to the agent in WinPE.
const AgentConfigName = "fogs-agent.json"

// TaskKindCheck boots the agent to check the link with the server. It does not
// touch the disk.
const TaskKindCheck = "agent"

// TaskKindFormat wipes the disk and leaves one empty volume on all of it, with no
// EFI or other system partitions. It needs no image.
const TaskKindFormat = "format"

// FormatLabel is the name of that volume.
const FormatLabel = "WB"

// AgentConfig tells the agent who it is and where the server is.
type AgentConfig struct {
	Server string `json:"server"` // base URL, for example http://192.168.88.105:8080
	MAC    string `json:"mac"`
	Host   string `json:"host"` // name shown on the screen
}

// AgentTask is what the server wants the agent to do.
type AgentTask struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	Image string `json:"image,omitempty"` // for TaskKindClone
	// Table is the partition table saved before a shrink that was never undone, for
	// example because the PC lost power in the middle of a clone.
	Table []byte `json:"table,omitempty"`
}

// StageFailed marks a report that the agent stopped on an error and waits on its screen.
const StageFailed = "Ошибка"

// StageWaiting marks a task waiting for its turn on an image.
const StageWaiting = "Ожидание"

// AgentProgress is a report about a running task.
type AgentProgress struct {
	MAC     string `json:"mac"`
	Percent int    `json:"percent"` // 0-100
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"` // the stack of a crash, for the log of the server
	// Metrics come with the report of a failure: how far the task got and how long it took.
	Metrics *TaskMetrics `json:"metrics,omitempty"`
}

// AgentResult ends a task.
type AgentResult struct {
	MAC     string       `json:"mac"`
	OK      bool         `json:"ok"`
	Message string       `json:"message"`
	Metrics *TaskMetrics `json:"metrics,omitempty"`
}

// TaskMetrics says where a clone or a deploy spent its time and what it used, so a
// slow task can be explained afterwards and a change can be measured.
type TaskMetrics struct {
	TotalMs int64 `json:"totalMs"`
	// Steps are the stages of the task in order, by the clock; they add up to the total.
	Steps []MetricTime `json:"steps"`
	// Work is the time each kind of work took. In a pipeline the kinds run at once,
	// so they overlap; a stage that waits a lot shows which other one is too slow.
	Work    []MetricTime   `json:"work"`
	Amounts []MetricAmount `json:"amounts"`

	CPUMs      int64  `json:"cpuMs"`      // processor time of the agent, all cores together
	Cores      int    `json:"cores"`      // logical processors of the PC
	PeakMemory uint64 `json:"peakMemory"` // the most memory the agent held, in bytes
	GCCycles   uint32 `json:"gcCycles"`   // garbage collections during the task
}

// MetricTime is a named span of time.
type MetricTime struct {
	Name string `json:"name"`
	Ms   int64  `json:"ms"`
}

// MetricAmount is a named quantity: bytes when Bytes is set, a count otherwise.
type MetricAmount struct {
	Name  string `json:"name"`
	Value uint64 `json:"value"`
	Bytes bool   `json:"bytes,omitempty"`
}

// Ways for a PC to leave iPXE and boot from its disk.
const (
	ExitSanboot  = "sanboot" // iPXE finds the loader on the disk, the default
	ExitFirmware = "exit"    // back to UEFI, which picks the next boot entry
	ExitRefind   = "refind"  // chain to refind.efi from the served directory
)

// ValidExitMode reports whether mode is a known way of leaving iPXE.
func ValidExitMode(mode string) bool {
	return mode == ExitSanboot || mode == ExitFirmware || mode == ExitRefind
}
