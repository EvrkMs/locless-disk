package boot

import (
	"fmt"

	"fogs/proto"
)

// RefindPath is the rEFInd loader inside the served directory, used by ExitRefind.
const RefindPath = "refind.efi"

// bootManager is where Windows keeps its loader on the EFI partition.
const bootManager = `\EFI\Microsoft\Boot\bootmgfw.efi`

// leave returns the script lines that take a PC from iPXE to its local disk.
//
// A plain exit hands the PC back to UEFI, and some firmware then opens its setup
// instead of trying the disk, so the default finds the loader itself. --drive 0
// means any disk holding the file. Without --filename iPXE uses the standard
// \EFI\BOOT\BOOTX64.EFI. Each line falls back to exit, so nothing can get stuck.
func leave(mode, httpAddr string) string {
	switch mode {
	case proto.ExitFirmware:
		return "exit\n"
	case proto.ExitRefind:
		return fmt.Sprintf("chain http://%s/files/%s || exit\n", httpAddr, RefindPath)
	}
	return fmt.Sprintf("sanboot --no-describe --drive 0 --filename %s || sanboot --no-describe --drive 0 || exit\n", bootManager)
}

// exitMode returns the chosen way of leaving iPXE.
func (c Config) exitMode() string {
	if c.ExitMode == nil {
		return proto.ExitSanboot
	}
	return c.ExitMode()
}

// exitScript is the whole script for a PC with nothing to do.
func exitScript(c Config) string {
	return "#!ipxe\n" + leave(c.exitMode(), c.HTTPAddr)
}
