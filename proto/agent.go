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
}

// AgentResult ends a task.
type AgentResult struct {
	MAC     string `json:"mac"`
	OK      bool   `json:"ok"`
	Message string `json:"message"`
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
