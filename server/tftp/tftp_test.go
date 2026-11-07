package tftp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startServer serves dir on a loopback port and returns the server address.
func startServer(t *testing.T, dir string) *net.UDPAddr {
	t.Helper()
	return startServerWith(t, dir, nil)
}

// startServerWith also serves the given generated files.
func startServerWith(t *testing.T, dir string, generated map[string][]byte) *net.UDPAddr {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	s := &Server{
		Root:      root,
		Generated: func(name string, _ netip.Addr) []byte { return generated[name] },
		Timeout:   50 * time.Millisecond,
		Retries:   3,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, conn) }()
	addr := conn.LocalAddr().(*net.UDPAddr)
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
		root.Close()
	})
	return addr
}

func writeFile(t *testing.T, dir, name string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
	}
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return data
}

func rrq(name string, opts ...string) []byte {
	b := []byte{0, opRRQ}
	for _, s := range append([]string{name, "octet"}, opts...) {
		b = append(b, s...)
		b = append(b, 0)
	}
	return b
}

// client is a minimal TFTP client for the tests.
type client struct {
	t      *testing.T
	conn   *net.UDPConn
	server *net.UDPAddr // becomes the transfer socket after the first reply
	bound  bool
}

func newClient(t *testing.T, server *net.UDPAddr) *client {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return &client{t: t, conn: conn, server: server}
}

func (c *client) send(b []byte) {
	c.t.Helper()
	if _, err := c.conn.WriteToUDP(b, c.server); err != nil {
		c.t.Fatalf("send: %v", err)
	}
}

func (c *client) ack(block uint16) {
	c.t.Helper()
	b := []byte{0, opAck, 0, 0}
	binary.BigEndian.PutUint16(b[2:], block)
	c.send(b)
}

func (c *client) read() []byte {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 70000)
	n, from, err := c.conn.ReadFromUDP(buf)
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	if !c.bound {
		c.server, c.bound = from, true
	}
	return buf[:n]
}

// wantError reads a reply and checks it is an ERROR with the given code.
func (c *client) wantError(code uint16) {
	c.t.Helper()
	p := c.read()
	if binary.BigEndian.Uint16(p) != opError {
		c.t.Fatalf("got opcode %d, want ERROR", binary.BigEndian.Uint16(p))
	}
	if got := binary.BigEndian.Uint16(p[2:]); got != code {
		c.t.Fatalf("error code: got %d, want %d (%q)", got, code, p[4:])
	}
}

// download receives a file with one ACK per window. first is the packet already
// read, if any.
func (c *client) download(blockSize, window int, first []byte) []byte {
	c.t.Helper()
	var got []byte
	next := uint16(1)
	inWindow := 0
	for p := first; ; p = nil {
		if p == nil {
			p = c.read()
		}
		if op := binary.BigEndian.Uint16(p); op != opData {
			c.t.Fatalf("got opcode %d, want DATA", op)
		}
		block := binary.BigEndian.Uint16(p[2:])
		if block != next {
			c.t.Fatalf("got block %d, want %d", block, next)
		}
		got = append(got, p[4:]...)
		inWindow++
		final := len(p)-4 < blockSize
		if final || inWindow == window {
			c.ack(block)
			inWindow = 0
		}
		if final {
			return got
		}
		next++
	}
}

func TestDownload(t *testing.T) {
	for _, size := range []int{0, 1, 511, 512, 513, 1024, 5000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			dir := t.TempDir()
			want := writeFile(t, dir, "ipxe.efi", size)
			c := newClient(t, startServer(t, dir))
			c.send(rrq("ipxe.efi"))
			if got := c.download(512, 1, nil); !bytes.Equal(got, want) {
				t.Fatalf("got %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

// oack reads an OACK and returns its options.
func (c *client) oack() map[string]string {
	c.t.Helper()
	p := c.read()
	if op := binary.BigEndian.Uint16(p); op != opOACK {
		c.t.Fatalf("got opcode %d, want OACK", op)
	}
	fields := strings.Split(strings.TrimSuffix(string(p[2:]), "\x00"), "\x00")
	opts := make(map[string]string)
	for i := 0; i+1 < len(fields); i += 2 {
		opts[fields[i]] = fields[i+1]
	}
	return opts
}

func TestOptions(t *testing.T) {
	dir := t.TempDir()
	want := writeFile(t, dir, "boot.wim", 100000)
	c := newClient(t, startServer(t, dir))

	c.send(rrq("boot.wim", "BLKSIZE", "1432", "tsize", "0", "windowsize", "8", "unknown", "1"))
	opts := c.oack()
	wantOpts := map[string]string{"blksize": "1432", "tsize": "100000", "windowsize": "8"}
	if fmt.Sprint(opts) != fmt.Sprint(wantOpts) {
		t.Fatalf("OACK: got %v, want %v", opts, wantOpts)
	}
	c.ack(0)
	if got := c.download(1432, 8, nil); !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d", len(got), len(want))
	}
}

func TestOptionLimits(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f", 10)
	c := newClient(t, startServer(t, dir))

	c.send(rrq("f", "blksize", "99999", "windowsize", "1000", "timeout", "0"))
	opts := c.oack()
	if opts["blksize"] != "65464" || opts["windowsize"] != "64" {
		t.Fatalf("limits not applied: %v", opts)
	}
	if _, ok := opts["timeout"]; ok {
		t.Fatalf("invalid timeout was accepted: %v", opts)
	}
}

// TestBlockNumberWraps sends more than 65536 blocks, the size of a boot.wim
// transferred with small blocks.
func TestBlockNumberWraps(t *testing.T) {
	dir := t.TempDir()
	want := writeFile(t, dir, "big", 70000*8+3)
	c := newClient(t, startServer(t, dir))

	c.send(rrq("big", "blksize", "8", "windowsize", "64"))
	c.oack()
	c.ack(0)
	if got := c.download(8, 64, nil); !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d", len(got), len(want))
	}
}

func TestResendsAfterLostAck(t *testing.T) {
	dir := t.TempDir()
	want := writeFile(t, dir, "f", 1000)
	c := newClient(t, startServer(t, dir))

	c.send(rrq("f"))
	first := c.read()
	again := c.read() // no ACK was sent, so the block must come again
	if !bytes.Equal(first, again) {
		t.Fatal("resent block differs")
	}
	if got := c.download(512, 1, again); !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d", len(got), len(want))
	}
}

func TestDuplicateAckDoesNotResend(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f", 2000)
	c := newClient(t, startServer(t, dir))

	c.send(rrq("f"))
	c.read()
	c.ack(1)
	c.ack(1)
	c.read() // block 2
	c.ack(2)
	p := c.read()
	if got := binary.BigEndian.Uint16(p[2:]); got != 3 {
		t.Fatalf("got block %d after ACK 2, want 3", got)
	}
}

func TestGivesUpOnSilentClient(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f", 1000)
	c := newClient(t, startServer(t, dir))

	c.send(rrq("f"))
	for range 4 { // the first send and 3 retries
		c.read()
	}
	c.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	var nerr net.Error
	if _, err := c.conn.Read(make([]byte, 1024)); !errors.As(err, &nerr) || !nerr.Timeout() {
		t.Fatalf("server kept sending: %v", err)
	}
}

// TestClientAbortAfterOACK is what UEFI firmware does to learn a file size.
func TestClientAbortAfterOACK(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "f", 1000)
	c := newClient(t, startServer(t, dir))

	c.send(rrq("f", "tsize", "0"))
	if got := c.oack()["tsize"]; got != "1000" {
		t.Fatalf("tsize: %q", got)
	}
	c.send(errorPacket(8, "aborted"))
	c.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := c.conn.Read(make([]byte, 1024)); err == nil {
		t.Fatal("server kept sending after the client aborted")
	}
}

func TestPaths(t *testing.T) {
	outer := t.TempDir()
	dir := filepath.Join(outer, "root")
	want := writeFile(t, dir, "efi/boot/bootx64.efi", 100)
	writeFile(t, outer, "secret", 10)
	addr := startServer(t, dir)

	for _, name := range []string{"efi/boot/bootx64.efi", `efi\boot\bootx64.efi`, `\efi\boot\bootx64.efi`, "/efi/boot/bootx64.efi"} {
		t.Run("found "+name, func(t *testing.T) {
			c := newClient(t, addr)
			c.send(rrq(name))
			if got := c.download(512, 1, nil); !bytes.Equal(got, want) {
				t.Fatalf("got %d bytes", len(got))
			}
		})
	}

	// Windows refuses a drive path outright. Elsewhere the leading slash goes and
	// the rest is looked up inside the root, where there is no such file.
	absolute := uint16(errAccess)
	if runtime.GOOS != "windows" {
		absolute = errNotFound
	}
	refused := []struct {
		name string
		code uint16
	}{
		{"missing", errNotFound},
		{"efi", errNotFound}, // a directory
		{"", errAccess},
		{"../secret", errAccess},
		{`..\secret`, errAccess},
		{"efi/../../secret", errAccess},
		{filepath.Join(outer, "secret"), absolute},
	}
	for _, tc := range refused {
		t.Run("refused "+tc.name, func(t *testing.T) {
			c := newClient(t, addr)
			c.send(rrq(tc.name))
			c.wantError(tc.code)
		})
	}
}

func TestGeneratedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "autoexec.ipxe", 10) // a stale copy on disk must lose
	want := []byte("#!ipxe\nexit\n")
	addr := startServerWith(t, dir, map[string][]byte{"autoexec.ipxe": want})

	for _, name := range []string{"autoexec.ipxe", `\autoexec.ipxe`, "/autoexec.ipxe"} {
		t.Run(name, func(t *testing.T) {
			c := newClient(t, addr)
			c.send(rrq(name))
			if got := c.download(512, 1, nil); !bytes.Equal(got, want) {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// TestGeneratedFileSize is what the UEFI firmware does for autoexec.ipxe: it
// asks for the size, aborts, then fetches the file in a second request.
func TestGeneratedFileSize(t *testing.T) {
	want := []byte("#!ipxe\nexit\n")
	addr := startServerWith(t, t.TempDir(), map[string][]byte{"autoexec.ipxe": want})

	c := newClient(t, addr)
	c.send(rrq("autoexec.ipxe", "tsize", "0"))
	if got := c.oack()["tsize"]; got != strconv.Itoa(len(want)) {
		t.Fatalf("tsize: %q", got)
	}
	c.send(errorPacket(8, "aborted"))

	c = newClient(t, addr)
	c.send(rrq("autoexec.ipxe", "blksize", "1468"))
	c.oack()
	c.ack(0)
	if got := c.download(1468, 1, nil); !bytes.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRefusesWrites(t *testing.T) {
	dir := t.TempDir()
	c := newClient(t, startServer(t, dir))
	wrq := rrq("evil")
	wrq[1] = opWRQ
	c.send(wrq)
	c.wantError(errAccess)
	if _, err := os.Stat(filepath.Join(dir, "evil")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file was created: %v", err)
	}
}

func TestRefusesBadRequests(t *testing.T) {
	addr := startServer(t, t.TempDir())
	tests := []struct {
		name string
		b    []byte
	}{
		{"netascii", []byte("\x00\x01f\x00netascii\x00")},
		{"no mode", []byte("\x00\x01f\x00")},
		{"no terminator", []byte("\x00\x01f\x00octet")},
		{"option without value", []byte("\x00\x01f\x00octet\x00blksize\x00")},
		{"data on the main port", []byte{0, opData, 0, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, addr)
			c.send(tc.b)
			c.wantError(errIllegal)
		})
	}
}

// TestGeneratedFileKnowsTheClient: the script can depend on which PC asks.
func TestGeneratedFileKnowsTheClient(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	s := &Server{Root: root, Generated: func(name string, client netip.Addr) []byte {
		return []byte(client.String())
	}}
	f, _, _, err := s.open("autoexec.ipxe", netip.MustParseAddr("192.168.88.40"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := make([]byte, 13)
	if n, _ := f.ReadAt(got, 0); string(got[:n]) != "192.168.88.40" {
		t.Fatalf("got %q", got[:n])
	}
}
