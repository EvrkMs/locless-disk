// Package boot serves the iPXE scripts that decide what a client does next.
package boot

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
)

// exitScript leaves iPXE, so UEFI moves on to the next entry: the local disk.
const exitScript = "#!ipxe\nexit\n"

// New returns the handler for GET /boot.ipxe?mac=...
//
// There are no tasks yet, so a known client only proves the chain works: it
// prints a line and boots from disk like everyone else.
func New(known func(net.HardwareAddr) bool, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boot.ipxe", func(w http.ResponseWriter, r *http.Request) {
		mac, err := net.ParseMAC(r.URL.Query().Get("mac"))
		if err != nil || len(mac) != 6 {
			http.Error(w, "bad mac", http.StatusBadRequest)
			return
		}
		script := exitScript
		if known(mac) {
			script = fmt.Sprintf("#!ipxe\necho FOGs: host %s has no task, booting from disk\nexit\n", mac)
		}
		log.Info("boot script requested", "mac", mac.String(), "known", known(mac), "remote", r.RemoteAddr)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, script)
	})
	return mux
}
