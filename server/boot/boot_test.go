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

	"fogs/proto"
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
		"initrd http://192.168.88.105:8080/agent/aa-bb-cc-00-00-03/fogs-agent.json fogs-agent.json || goto disk",
		"initrd http://192.168.88.105:8080/files/agent/partclone.exe partclone.exe || goto disk",
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
	if _, body := get(t, http.MethodGet, "/boot.ipxe?mac=aa:bb:cc:00:00:04"); body != exitScript(Config{HTTPAddr: "192.168.88.105:8080"}) {
		t.Fatalf("script: %q", body)
	}
}

func TestWinpeshlStartsNetworkBeforeAgent(t *testing.T) {
	code, body := get(t, http.MethodGet, "/winpeshl.ini")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	net, agent := strings.Index(body, "wpeinit.exe"), strings.Index(body, "partclone.exe")
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
	script := string(Autoexec("192.168.88.105:8080", proto.ExitFirmware, false))
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
	if code != http.StatusOK || body != exitScript(Config{HTTPAddr: "192.168.88.105:8080"}) {
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

func TestKnownHostWithoutTaskUsesSanboot(t *testing.T) {
	_, body := get(t, http.MethodGet, "/boot.ipxe?mac=aa:bb:cc:00:00:01")
	if !strings.Contains(body, "\nsanboot --no-describe") {
		t.Fatalf("script: %q", body)
	}
}

func TestLeaveDefaultsToSanboot(t *testing.T) {
	script := exitScript(Config{HTTPAddr: "a:1"})
	if !strings.HasPrefix(script, "#!ipxe\nsanboot --no-describe --drive 0 --filename "+bootManager+" || ") {
		t.Fatalf("script: %q", script)
	}
	if !strings.HasSuffix(script, " || exit\n") {
		t.Fatalf("no way out if sanboot fails: %q", script)
	}
}

func TestLeaveModes(t *testing.T) {
	if got := leave(proto.ExitFirmware, "a:1"); got != "exit\n" {
		t.Fatalf("exit: %q", got)
	}
	if got := leave(proto.ExitRefind, "a:1"); got != "chain http://a:1/files/refind.efi || exit\n" {
		t.Fatalf("refind: %q", got)
	}
	s := string(Autoexec("a:1", proto.ExitSanboot, false))
	if !strings.Contains(s, ":disk\nsanboot ") {
		t.Fatalf("autoexec: %s", s)
	}
}

func TestExitModeIsReadOnEveryRequest(t *testing.T) {
	mode := proto.ExitFirmware
	h := New(Config{
		HTTPAddr: "a:1",
		Known:    func(net.HardwareAddr) bool { return false },
		HasTask:  func(net.HardwareAddr) bool { return false },
		ExitMode: func() string { return mode },
		Files:    fstest.MapFS{},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	request := func() string {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/boot.ipxe?mac=aa:bb:cc:00:00:09", nil))
		return w.Body.String()
	}
	if got := request(); got != "#!ipxe\nexit\n" {
		t.Fatalf("script: %q", got)
	}
	mode = proto.ExitRefind
	if got := request(); !strings.Contains(got, "refind.efi") {
		t.Fatalf("script: %q", got)
	}
}

// TestIdleAutoexecOnlyLeaves: a PC the server knows to have no task skips the
// address and HTTP. rEFInd still needs HTTP.
func TestIdleAutoexecOnlyLeaves(t *testing.T) {
	s := string(Autoexec("a:1", proto.ExitSanboot, true))
	if strings.Contains(s, "dhcp") || strings.Contains(s, "http://") || !strings.Contains(s, "\nsanboot ") {
		t.Fatalf("idle sanboot: %s", s)
	}
	if s := string(Autoexec("a:1", proto.ExitFirmware, true)); strings.Contains(s, "dhcp") || !strings.HasSuffix(s, "\nexit\n") {
		t.Fatalf("idle exit: %s", s)
	}
	if s := string(Autoexec("a:1", proto.ExitRefind, true)); !strings.Contains(s, "dhcp ||") {
		t.Fatalf("idle refind: %s", s)
	}
}
