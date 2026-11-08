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

	"fogs/proto"
)

// AutoexecName is the file iPXE asks the boot server for right after it starts.
const AutoexecName = "autoexec.ipxe"

// Paths inside the served directory that booting WinPE needs. Nothing else from
// Microsoft is required: wimboot extracts the boot manager and BCD from boot.wim.
const (
	// ShimPath is what a PC gets first. Microsoft signed it, so it passes Secure Boot,
	// and it loads IPXEPath, which has to be the build signed for it. The name
	// matters: the shim finds iPXE by its own name.
	ShimPath    = "ipxe-shim.efi"
	IPXEPath    = "ipxe.efi"
	WimbootPath = "wimboot"
	BootWimPath = "WinPE/boot.wim"
	AgentPath   = "agent/partclone.exe"
)

// winpeshl replaces the WinPE shell. wpeinit has to run first: a custom
// winpeshl.ini skips the usual startup, and with it the network. WinPE reboots
// when the last program exits.
const winpeshl = "[LaunchApps]\r\n" +
	"%SYSTEMROOT%\\System32\\wpeinit.exe\r\n" +
	"%SYSTEMROOT%\\System32\\partclone.exe\r\n"

// Autoexec generates autoexec.ipxe for a server reachable at httpAddr.
//
// When this file exists iPXE runs it instead of its automatic boot, so the
// script has to configure the network itself. Every failure ends in leaving iPXE,
// so a network problem must not stop the PC booting
// from disk. netX is the interface dhcp just opened.
//
// idle means the server already knows this PC has nothing to do. Then the script
// only leaves, without an address and HTTP, so the PC reaches its disk sooner.
// rEFInd comes over HTTP, so that mode always gets the whole script.
func Autoexec(httpAddr, exitMode string, idle bool) []byte {
	if idle && exitMode != proto.ExitRefind {
		return fmt.Appendf(nil, "#!ipxe\necho FOGs: no task, booting from disk\n%s", leave(exitMode, httpAddr))
	}
	return fmt.Appendf(nil, `#!ipxe
echo FOGs: requesting an address
dhcp || dhcp || goto disk
echo FOGs: ${netX/mac} got ${netX/ip}
chain http://%s/boot.ipxe?mac=${netX/mac} || goto disk
:disk
%s`, httpAddr, leave(exitMode, httpAddr))
}

// agentScript boots WinPE from the official boot.wim. Index 1 is Windows PE;
// wimboot would otherwise pick the installer that ISO images mark as bootable.
// wimboot places the initrd files in X:\Windows\System32. iPXE names an image
// after the last part of the URL path, so the config URL must end in its file
// name and carry the MAC in the path, not in a query.
func agentScript(httpAddr string, mac net.HardwareAddr) string {
	urlMAC := strings.ReplaceAll(mac.String(), ":", "-")
	return fmt.Sprintf(`#!ipxe
echo FOGs: loading WinPE
kernel http://%[1]s/files/%[2]s index=1 || goto disk
initrd http://%[1]s/winpeshl.ini winpeshl.ini || goto disk
initrd http://%[1]s/agent/%[5]s/%[6]s %[6]s || goto disk
initrd http://%[1]s/files/%[3]s partclone.exe || goto disk
initrd http://%[1]s/files/%[4]s boot.wim || goto disk
boot || goto disk
:disk
echo FOGs: cannot load WinPE, booting from disk
exit
`, httpAddr, WimbootPath, AgentPath, BootWimPath, urlMAC, proto.AgentConfigName)
}

// Config is what the handler needs from the server.
type Config struct {
	HTTPAddr string                      // host:port clients reach this server at
	Known    func(net.HardwareAddr) bool // the MAC list
	HasTask  func(net.HardwareAddr) bool // PCs that should boot the agent
	Started  func(net.HardwareAddr)      // a PC was sent into WinPE
	Files    fs.FS                       // served under /files/
	ExitMode func() string               // how a PC leaves iPXE; nil means the default
	Agent    AgentStore                  // tasks as the agent sees them
	Clone    CloneManager                // receives images
	Deploy   DeployManager               // sends images
	Log      *slog.Logger
	// MetricsDir gets a JSON file with the metrics of every task, for tools that
	// compare runs or draw them; "" keeps them only in the database.
	MetricsDir string

	boots *bootClock // made by New
}

// New returns the handler for everything clients ask for: /boot.ipxe,
// /winpeshl.ini, /files/ and the agent's /agent/.
func New(c Config) http.Handler {
	mux := http.NewServeMux()
	c.boots = newBootClock()
	mux.HandleFunc("GET /boot.ipxe", func(w http.ResponseWriter, r *http.Request) {
		mac, err := net.ParseMAC(r.URL.Query().Get("mac"))
		if err != nil || len(mac) != 6 {
			http.Error(w, "bad mac", http.StatusBadRequest)
			return
		}
		known := c.Known(mac)
		task := known && c.HasTask(mac)
		script := exitScript(c)
		switch {
		case task:
			script = agentScript(c.HTTPAddr, mac)
			c.Started(mac)
			c.boots.scriptSent(mac.String())
		case known:
			script = fmt.Sprintf("#!ipxe\necho FOGs: host %s has no task, booting from disk\n%s", mac, leave(c.exitMode(), c.HTTPAddr))
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
	addAgentRoutes(mux, c)
	return mux
}
