// Package boot serves what a client needs after iPXE starts: the scripts that
// decide what it does next, and the files for booting WinPE.
package boot

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// exitScript leaves iPXE, so UEFI moves on to the next entry: the local disk.
const exitScript = "#!ipxe\nexit\n"

// AutoexecName is the file iPXE asks the boot server for right after it starts.
const AutoexecName = "autoexec.ipxe"

// Paths inside the served directory that booting WinPE needs. Nothing else from
// Microsoft is required: wimboot extracts the boot manager and BCD from boot.wim.
const (
	WimbootPath = "wimboot"
	BootWimPath = "WinPE/boot.wim"
	AgentPath   = "agent/fogs-agent.exe"
)

// winpeshl replaces the WinPE shell. wpeinit has to run first: a custom
// winpeshl.ini skips the usual startup, and with it the network. WinPE reboots
// when the last program exits.
const winpeshl = "[LaunchApps]\r\n" +
	"%SYSTEMROOT%\\System32\\wpeinit.exe\r\n" +
	"%SYSTEMROOT%\\System32\\fogs-agent.exe\r\n"

// Autoexec generates autoexec.ipxe for a server reachable at httpAddr.
//
// When this file exists iPXE runs it instead of its automatic boot, so the
// script has to configure the network itself. Every failure ends in exit,
// which hands the PC back to UEFI: a network problem must not stop it booting
// from disk. netX is the interface dhcp just opened.
func Autoexec(httpAddr string) []byte {
	return fmt.Appendf(nil, `#!ipxe
echo FOGs: requesting an address
dhcp || dhcp || goto disk
echo FOGs: ${netX/mac} got ${netX/ip}
chain http://%s/boot.ipxe?mac=${netX/mac} || goto disk
:disk
exit
`, httpAddr)
}

// agentScript boots WinPE from the official boot.wim. Index 1 is Windows PE;
// wimboot would otherwise pick the installer that ISO images mark as bootable.
// wimboot places the initrd files in X:\Windows\System32.
func agentScript(httpAddr string) string {
	return fmt.Sprintf(`#!ipxe
echo FOGs: loading WinPE
kernel http://%[1]s/files/%[2]s index=1 || goto disk
initrd http://%[1]s/winpeshl.ini winpeshl.ini || goto disk
initrd http://%[1]s/files/%[3]s fogs-agent.exe || goto disk
initrd http://%[1]s/files/%[4]s boot.wim || goto disk
boot || goto disk
:disk
echo FOGs: cannot load WinPE, booting from disk
exit
`, httpAddr, WimbootPath, AgentPath, BootWimPath)
}

// Config is what the handler needs from the server.
type Config struct {
	HTTPAddr string                      // host:port clients reach this server at
	Known    func(net.HardwareAddr) bool // the MAC list
	HasTask  func(net.HardwareAddr) bool // PCs that should boot the agent
	Started  func(net.HardwareAddr)      // a PC was sent into WinPE
	Files    fs.FS                       // served under /files/
	Log      *slog.Logger
}

// New returns the handler for /boot.ipxe, /winpeshl.ini and /files/.
func New(c Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boot.ipxe", func(w http.ResponseWriter, r *http.Request) {
		mac, err := net.ParseMAC(r.URL.Query().Get("mac"))
		if err != nil || len(mac) != 6 {
			http.Error(w, "bad mac", http.StatusBadRequest)
			return
		}
		known := c.Known(mac)
		task := known && c.HasTask(mac)
		script := exitScript
		switch {
		case task:
			script = agentScript(c.HTTPAddr)
			c.Started(mac)
		case known:
			script = fmt.Sprintf("#!ipxe\necho FOGs: host %s has no task, booting from disk\nexit\n", mac)
		}
		c.Log.Info("boot script requested", "mac", mac.String(), "known", known, "task", task, "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, script)
	})
	mux.HandleFunc("GET /winpeshl.ini", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, winpeshl)
	})
	files := http.StripPrefix("/files/", http.FileServerFS(c.Files))
	mux.HandleFunc("GET /files/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") { // no directory listings
			http.NotFound(w, r)
			return
		}
		c.Log.Info("file requested", "path", r.URL.Path, "remote", r.RemoteAddr)
		files.ServeHTTP(w, r)
	})
	return mux
}
