package boot

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// started counts the PCs the handler sent into WinPE.
var started int

func get(t *testing.T, method, url string) (int, string) {
	t.Helper()
	is := func(macs ...string) func(net.HardwareAddr) bool {
		return func(mac net.HardwareAddr) bool { return slices.Contains(macs, mac.String()) }
	}
	h := New(Config{
		HTTPAddr: "192.168.88.105:8080",
		Known:    is("aa:bb:cc:00:00:01", "aa:bb:cc:00:00:03"),
		HasTask:  is("aa:bb:cc:00:00:03", "aa:bb:cc:00:00:04"),
		Started: func(mac net.HardwareAddr) {
			if mac.String() != "aa:bb:cc:00:00:03" {
				t.Errorf("task started for %s", mac)
			}
			started++
		},
		Files: fstest.MapFS{
			"wimboot":        {Data: []byte("loader")},
			"WinPE/boot.wim": {Data: []byte("image")},
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, url, nil))
	return w.Code, w.Body.String()
}

func TestTaskBootsWinPE(t *testing.T) {
	started = 0
	code, body := get(t, http.MethodGet, "/boot.ipxe?mac=aa:bb:cc:00:00:03")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if started != 1 {
		t.Fatalf("task marked started %d times", started)
	}
	for _, want := range []string{
		"kernel http://192.168.88.105:8080/files/wimboot index=1 || goto disk",
		"initrd http://192.168.88.105:8080/winpeshl.ini winpeshl.ini || goto disk",
		"initrd http://192.168.88.105:8080/files/agent/fogs-agent.exe fogs-agent.exe || goto disk",
		"initrd http://192.168.88.105:8080/files/WinPE/boot.wim boot.wim || goto disk",
		"boot || goto disk",
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("no line %q in:\n%s", want, body)
		}
	}
	if !strings.HasSuffix(body, "exit\n") {
		t.Fatalf("a failed boot must fall back to the disk:\n%s", body)
	}
}

// TestTaskNeedsKnownHost guards the allow list: a task alone must not start
// WinPE on a PC that is not on the MAC list.
func TestTaskNeedsKnownHost(t *testing.T) {
	if _, body := get(t, http.MethodGet, "/boot.ipxe?mac=aa:bb:cc:00:00:04"); body != exitScript {
		t.Fatalf("script: %q", body)
	}
}

func TestWinpeshlStartsNetworkBeforeAgent(t *testing.T) {
	code, body := get(t, http.MethodGet, "/winpeshl.ini")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	net, agent := strings.Index(body, "wpeinit.exe"), strings.Index(body, "fogs-agent.exe")
	if !strings.HasPrefix(body, "[LaunchApps]\r\n") || net < 0 || agent < net {
		t.Fatalf("winpeshl.ini: %q", body)
	}
}

func TestServesFiles(t *testing.T) {
	code, body := get(t, http.MethodGet, "/files/WinPE/boot.wim")
	if code != http.StatusOK || body != "image" {
		t.Fatalf("status %d, body %q", code, body)
	}
	for _, url := range []string{"/files/", "/files/WinPE/", "/files/missing", "/files/../go.mod"} {
		if code, _ := get(t, http.MethodGet, url); code == http.StatusOK {
			t.Errorf("%s: status %d", url, code)
		}
	}
}

func TestAutoexec(t *testing.T) {
	script := string(Autoexec("192.168.88.105:8080"))
	lines := strings.Split(strings.TrimSpace(script), "\n")
	if lines[0] != "#!ipxe" {
		t.Fatalf("first line: %q", lines[0])
	}
	for _, want := range []string{
		"dhcp || dhcp || goto disk",
		"chain http://192.168.88.105:8080/boot.ipxe?mac=${netX/mac} || goto disk",
	} {
		if !strings.Contains(script, want+"\n") {
			t.Errorf("no line %q in:\n%s", want, script)
		}
	}
	// Every path must end in exit, or a failure would leave the PC stuck in iPXE.
	if n := len(lines); lines[n-2] != ":disk" || lines[n-1] != "exit" {
		t.Fatalf("script does not end by leaving iPXE:\n%s", script)
	}
}

func TestKnownHost(t *testing.T) {
	code, body := get(t, http.MethodGet, "/boot.ipxe?mac=AA-BB-CC-00-00-01")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !strings.HasPrefix(body, "#!ipxe\n") || !strings.Contains(body, "aa:bb:cc:00:00:01") || !strings.HasSuffix(body, "exit\n") {
		t.Fatalf("script: %q", body)
	}
}

func TestUnknownHostBootsFromDisk(t *testing.T) {
	code, body := get(t, http.MethodGet, "/boot.ipxe?mac=aa:bb:cc:00:00:02")
	if code != http.StatusOK || body != exitScript {
		t.Fatalf("status %d, script %q", code, body)
	}
}

func TestBadRequests(t *testing.T) {
	tests := []struct {
		name, method, url string
		want              int
	}{
		{"no mac", http.MethodGet, "/boot.ipxe", http.StatusBadRequest},
		{"bad mac", http.MethodGet, "/boot.ipxe?mac=nope", http.StatusBadRequest},
		{"long mac", http.MethodGet, "/boot.ipxe?mac=00:00:00:00:fe:80:00:00", http.StatusBadRequest},
		{"wrong method", http.MethodPost, "/boot.ipxe?mac=aa:bb:cc:00:00:01", http.StatusMethodNotAllowed},
		{"other path", http.MethodGet, "/other", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := get(t, tc.method, tc.url); code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
		})
	}
}
