package dhcp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"net"
	"net/netip"
)

// Ports used by proxyDHCP.
const (
	ServerPort = 67
	ClientPort = 68
	BootPort   = 4011
)

// Client architectures from option 93 (RFC 4578) that we can boot.
const (
	archEFIBC  uint16 = 7
	archEFIX64 uint16 = 9
)

// pxeBypassDiscovery is option 43 telling a PXE client to skip boot server
// discovery and use the file name in the reply: suboption 6 with bit 3 set.
var pxeBypassDiscovery = []byte{6, 1, 8, 255}

// Proxy answers PXE clients without leasing addresses.
//
// Other PCs share the network segment, and a reply steers a client's boot, so
// the default is silence: only a MAC accepted by Allow ever gets an answer.
type Proxy struct {
	ServerIP  netip.Addr                    // IPv4 address of this server
	BootFile  string                        // first-stage file served over TFTP
	ScriptURL func(net.HardwareAddr) string // boot script for a client already in iPXE
	Allow     func(net.HardwareAddr) bool   // nil refuses everyone
	Seen      func(net.HardwareAddr)        // called for every UEFI PXE client; may be nil
	Broadcast netip.AddrPort                // zero value means 255.255.255.255:68
	Log       *slog.Logger
}

// Offer returns the reply to a broadcast received on port 67, or nil to stay
// silent. UEFI firmware follows it up with a request to port 4011.
func (p *Proxy) Offer(req *Packet) *Packet {
	if req.messageType() != MsgDiscover {
		return nil
	}
	r := p.reply(req, MsgOffer)
	if r != nil {
		r.Flags |= flagBroadcast
	}
	return r
}

// Ack returns the reply to a request received on port 4011, or nil to stay silent.
func (p *Proxy) Ack(req *Packet) *Packet {
	if req.messageType() != MsgRequest {
		return nil
	}
	return p.reply(req, MsgAck)
}

// reply builds an answer for an eligible client.
func (p *Proxy) reply(req *Packet, msg byte) *Packet {
	mac := req.MAC()
	if mac == nil || req.Op != OpRequest {
		return nil
	}
	if !bytes.HasPrefix(req.Options[OptVendorClass], []byte("PXEClient")) {
		return nil
	}
	if !isUEFI(req.Options[OptClientArch]) {
		p.Log.Debug("ignoring non-UEFI client", "mac", mac.String())
		return nil
	}
	if p.Seen != nil {
		p.Seen(mac)
	}
	if p.Allow == nil || !p.Allow(mac) {
		p.Log.Info("PXE client seen, not registered", "mac", mac.String())
		return nil
	}

	server := p.ServerIP.As4()
	r := &Packet{
		Op:     OpReply,
		HType:  req.HType,
		HLen:   req.HLen,
		XID:    req.XID,
		Flags:  req.Flags,
		SIAddr: server,
		CHAddr: req.CHAddr,
		SName:  p.ServerIP.String(),
		File:   p.BootFile,
		Options: map[byte][]byte{
			OptMessageType: {msg},
			OptServerID:    server[:],
			OptVendorClass: []byte("PXEClient"),
		},
	}
	if uuid, ok := req.Options[OptClientUUID]; ok {
		r.Options[OptClientUUID] = uuid
	}
	// Firmware gets no option 43: some UEFI implementations ignore a proxy
	// reply that bypasses discovery, while all of them go on to port 4011.
	if bytes.Contains(req.Options[OptUserClass], []byte("iPXE")) {
		r.File = p.ScriptURL(mac)
		r.Options[OptVendorSpecific] = pxeBypassDiscovery
	}
	p.Log.Info("answering PXE client", "mac", mac.String(), "file", r.File, "msg", msg)
	return r
}

// isUEFI reports whether option 93 lists a supported first architecture.
func isUEFI(arch []byte) bool {
	if len(arch) < 2 {
		return false
	}
	a := binary.BigEndian.Uint16(arch)
	return a == archEFIBC || a == archEFIX64
}

// ServeDHCP answers broadcasts on a socket bound to port 67 until ctx ends.
func (p *Proxy) ServeDHCP(ctx context.Context, conn *net.UDPConn) error {
	dst := p.Broadcast
	if !dst.IsValid() {
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte{255, 255, 255, 255}), ClientPort)
	}
	return p.serve(ctx, conn, p.Offer, func(netip.AddrPort) netip.AddrPort { return dst })
}

// ServeBoot answers requests on a socket bound to port 4011 until ctx ends.
func (p *Proxy) ServeBoot(ctx context.Context, conn *net.UDPConn) error {
	return p.serve(ctx, conn, p.Ack, func(src netip.AddrPort) netip.AddrPort { return src })
}

// serve is the receive loop shared by both ports.
func (p *Proxy) serve(ctx context.Context, conn *net.UDPConn, answer func(*Packet) *Packet, dest func(netip.AddrPort) netip.AddrPort) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	buf := make([]byte, 2048)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Windows reports ICMP errors from earlier sends here; the socket is fine.
			p.Log.Debug("read failed", "err", err)
			continue
		}
		req, err := Parse(buf[:n])
		if err != nil {
			p.Log.Debug("dropping bad packet", "src", src, "err", err)
			continue
		}
		r := answer(req)
		if r == nil {
			continue
		}
		b, err := r.Marshal()
		if err != nil {
			p.Log.Error("cannot encode reply", "err", err)
			continue
		}
		src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		if _, err := conn.WriteToUDPAddrPort(b, dest(src)); err != nil {
			p.Log.Warn("cannot send reply", "err", err)
		}
	}
}
