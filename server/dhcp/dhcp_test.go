package dhcp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

var (
	allowedMAC = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x01}
	otherMAC   = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x02}
)

func testProxy() *Proxy {
	return &Proxy{
		ServerIP: netip.MustParseAddr("192.168.88.105"),
		BootFile: "ipxe.efi",
		ScriptURL: func(mac net.HardwareAddr) string {
			return "http://192.168.88.105:8080/boot.ipxe?mac=" + mac.String()
		},
		Allow: func(mac net.HardwareAddr) bool { return bytes.Equal(mac, allowedMAC) },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func pxeRequest(msg byte, mac net.HardwareAddr) *Packet {
	p := &Packet{
		Op:    OpRequest,
		HType: 1,
		HLen:  6,
		XID:   0x12345678,
		Options: map[byte][]byte{
			OptMessageType: {msg},
			OptVendorClass: []byte("PXEClient:Arch:00007:UNDI:003016"),
			OptClientArch:  {0, 7},
			OptClientUUID:  {0, 1, 2, 3},
		},
	}
	copy(p.CHAddr[:], mac)
	return p
}

func TestPacketRoundTrip(t *testing.T) {
	p := pxeRequest(MsgDiscover, allowedMAC)
	p.SName = "server"
	p.File = "ipxe.efi"
	b, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(b) < minPacket {
		t.Fatalf("packet is %d bytes, want at least %d", len(b), minPacket)
	}
	if b[optionsAt] != OptMessageType {
		t.Fatalf("first option is %d, want the message type", b[optionsAt])
	}

	got, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.XID != p.XID || got.SName != p.SName || got.File != p.File {
		t.Fatalf("fields lost: %+v", got)
	}
	if !bytes.Equal(got.MAC(), allowedMAC) {
		t.Fatalf("MAC: got %s", got.MAC())
	}
	for code, want := range p.Options {
		if !bytes.Equal(got.Options[code], want) {
			t.Fatalf("option %d: got %v, want %v", code, got.Options[code], want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	valid := func() []byte {
		b := make([]byte, optionsAt)
		copy(b[headerSize:], magicCookie)
		return b
	}
	tests := []struct {
		name string
		b    []byte
	}{
		{"empty", nil},
		{"short", valid()[:optionsAt-1]},
		{"bad cookie", make([]byte, optionsAt)},
		{"option without length", append(valid(), 60)},
		{"option overrun", append(valid(), 60, 10, 'P')},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.b); err == nil {
				t.Fatal("Parse accepted a bad packet")
			}
		})
	}
}

func TestParseConcatenatesRepeatedOptions(t *testing.T) {
	b := make([]byte, optionsAt)
	copy(b[headerSize:], magicCookie)
	b = append(b, 60, 3, 'P', 'X', 'E', 0, 60, 2, 'C', 'l', 255, 60, 1, 'x')
	p, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := string(p.Options[60]); got != "PXECl" {
		t.Fatalf("option 60: got %q", got)
	}
}

func TestMarshalRejects(t *testing.T) {
	long := strings.Repeat("x", 200)
	tests := []struct {
		name string
		p    Packet
	}{
		{"long file", Packet{File: long}},
		{"long server name", Packet{SName: long}},
		{"long option", Packet{Options: map[byte][]byte{60: make([]byte, 256)}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.p.Marshal(); err == nil {
				t.Fatal("Marshal accepted an oversized field")
			}
		})
	}
}

func TestOfferToFirmware(t *testing.T) {
	r := testProxy().Offer(pxeRequest(MsgDiscover, allowedMAC))
	if r == nil {
		t.Fatal("no offer for an allowed client")
	}
	if r.Op != OpReply || r.messageType() != MsgOffer || r.XID != 0x12345678 {
		t.Fatalf("bad offer: %+v", r)
	}
	if r.File != "ipxe.efi" || r.SName != "192.168.88.105" {
		t.Fatalf("boot target: file %q server %q", r.File, r.SName)
	}
	if r.SIAddr != [4]byte{192, 168, 88, 105} || !bytes.Equal(r.Options[OptServerID], []byte{192, 168, 88, 105}) {
		t.Fatalf("server address: %v %v", r.SIAddr, r.Options[OptServerID])
	}
	if r.YIAddr != [4]byte{} {
		t.Fatalf("proxy offered an address: %v", r.YIAddr)
	}
	if r.Flags&flagBroadcast == 0 {
		t.Fatal("offer does not ask for broadcast")
	}
	if string(r.Options[OptVendorClass]) != "PXEClient" {
		t.Fatalf("vendor class: %q", r.Options[OptVendorClass])
	}
	if !bytes.Equal(r.Options[OptClientUUID], []byte{0, 1, 2, 3}) {
		t.Fatalf("uuid not echoed: %v", r.Options[OptClientUUID])
	}
	if _, ok := r.Options[OptVendorSpecific]; ok {
		t.Fatal("firmware must not get option 43")
	}
}

func TestOfferToIPXE(t *testing.T) {
	req := pxeRequest(MsgDiscover, allowedMAC)
	req.Options[OptUserClass] = []byte("iPXE")
	r := testProxy().Offer(req)
	if r == nil {
		t.Fatal("no offer for iPXE")
	}
	want := "http://192.168.88.105:8080/boot.ipxe?mac=aa:bb:cc:00:00:01"
	if r.File != want {
		t.Fatalf("file: got %q, want %q", r.File, want)
	}
	if !bytes.Equal(r.Options[OptVendorSpecific], pxeBypassDiscovery) {
		t.Fatalf("option 43: %v", r.Options[OptVendorSpecific])
	}
}

// TestLogsReadableMAC guards the way to find a new PC's MAC: the log. slog
// prints a net.HardwareAddr as raw bytes unless it is converted first.
func TestLogsReadableMAC(t *testing.T) {
	var out bytes.Buffer
	p := testProxy()
	p.Log = slog.New(slog.NewTextHandler(&out, nil))
	p.Offer(pxeRequest(MsgDiscover, otherMAC))
	if !strings.Contains(out.String(), "mac=aa:bb:cc:00:00:02") {
		t.Fatalf("log: %s", out.String())
	}
}

func TestAck(t *testing.T) {
	r := testProxy().Ack(pxeRequest(MsgRequest, allowedMAC))
	if r == nil {
		t.Fatal("no ack for an allowed client")
	}
	if r.messageType() != MsgAck || r.File != "ipxe.efi" {
		t.Fatalf("bad ack: %+v", r)
	}
}

// TestStaysSilent is the safety property: anything but an allowed UEFI PXE
// client must get no answer, or we would hijack other PCs on the network.
func TestStaysSilent(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(p *Proxy, req *Packet)
	}{
		{"mac not allowed", func(_ *Proxy, req *Packet) { copy(req.CHAddr[:], otherMAC) }},
		{"no allow list", func(p *Proxy, _ *Packet) { p.Allow = nil }},
		{"not PXE", func(_ *Proxy, req *Packet) { delete(req.Options, OptVendorClass) }},
		{"other vendor class", func(_ *Proxy, req *Packet) { req.Options[OptVendorClass] = []byte("MSFT 5.0") }},
		{"legacy BIOS", func(_ *Proxy, req *Packet) { req.Options[OptClientArch] = []byte{0, 0} }},
		{"no architecture", func(_ *Proxy, req *Packet) { delete(req.Options, OptClientArch) }},
		{"a reply, not a request", func(_ *Proxy, req *Packet) { req.Op = OpReply }},
		{"not ethernet", func(_ *Proxy, req *Packet) { req.HType = 6 }},
		{"wrong message type", func(_ *Proxy, req *Packet) { req.Options[OptMessageType] = []byte{8} }},
		{"plain BOOTP", func(_ *Proxy, req *Packet) { delete(req.Options, OptMessageType) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := testProxy()
			offer := pxeRequest(MsgDiscover, allowedMAC)
			tc.mutate(p, offer)
			if r := p.Offer(offer); r != nil {
				t.Fatalf("offer sent: %+v", r)
			}
			ack := pxeRequest(MsgRequest, allowedMAC)
			tc.mutate(p, ack)
			if r := p.Ack(ack); r != nil {
				t.Fatalf("ack sent: %+v", r)
			}
		})
	}
}

func TestOfferIgnoresRequestAndAckIgnoresDiscover(t *testing.T) {
	p := testProxy()
	if r := p.Offer(pxeRequest(MsgRequest, allowedMAC)); r != nil {
		t.Fatal("offer sent for a request meant for the real DHCP server")
	}
	if r := p.Ack(pxeRequest(MsgDiscover, allowedMAC)); r != nil {
		t.Fatal("ack sent for a discover")
	}
}

func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func send(t *testing.T, from, to *net.UDPConn, p *Packet) {
	t.Helper()
	b, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if _, err := from.WriteToUDPAddrPort(b, to.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func receive(t *testing.T, c *net.UDPConn) *Packet {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	p, err := Parse(buf[:n])
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return p
}

func TestServeDHCPSendsToBroadcastAddress(t *testing.T) {
	server, client := listen(t), listen(t)
	p := testProxy()
	p.Broadcast = client.LocalAddr().(*net.UDPAddr).AddrPort()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.ServeDHCP(ctx, server) }()

	// Garbage and foreign clients must not stop the loop or get an answer.
	if _, err := client.WriteToUDPAddrPort([]byte("junk"), server.LocalAddr().(*net.UDPAddr).AddrPort()); err != nil {
		t.Fatalf("send: %v", err)
	}
	send(t, client, server, pxeRequest(MsgDiscover, otherMAC))
	send(t, client, server, pxeRequest(MsgDiscover, allowedMAC))

	r := receive(t, client)
	if r.messageType() != MsgOffer || !bytes.Equal(r.MAC(), allowedMAC) {
		t.Fatalf("bad reply: %+v", r)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ServeDHCP: %v", err)
	}
}

func TestServeBootRepliesToSender(t *testing.T) {
	server, client := listen(t), listen(t)
	p := testProxy()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.ServeBoot(ctx, server) }()

	send(t, client, server, pxeRequest(MsgRequest, allowedMAC))
	if r := receive(t, client); r.messageType() != MsgAck {
		t.Fatalf("bad reply: %+v", r)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("ServeBoot: %v", err)
	}
}
