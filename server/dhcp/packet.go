// Package dhcp implements the proxyDHCP half of PXE: it never leases addresses,
// it only tells allowed clients where to find their boot file.
package dhcp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"slices"
)

// Wire layout of a BOOTP/DHCP message.
const (
	headerSize = 236
	optionsAt  = headerSize + 4
	minPacket  = 300 // BOOTP minimum; some firmware drops anything shorter
	maxSName   = 63  // 64-byte field including the terminating NUL
	maxFile    = 127 // 128-byte field including the terminating NUL
)

var magicCookie = []byte{99, 130, 83, 99}

// BOOTP operation codes.
const (
	OpRequest byte = 1
	OpReply   byte = 2
)

// DHCP option codes used by PXE.
const (
	OptVendorSpecific byte = 43
	OptMessageType    byte = 53
	OptServerID       byte = 54
	OptVendorClass    byte = 60
	OptUserClass      byte = 77
	OptClientArch     byte = 93
	OptClientUUID     byte = 97
)

// DHCP message types.
const (
	MsgDiscover byte = 1
	MsgOffer    byte = 2
	MsgRequest  byte = 3
	MsgAck      byte = 5
)

// flagBroadcast asks for the reply to be broadcast: the client has no address yet.
const flagBroadcast uint16 = 0x8000

// Packet is a decoded BOOTP/DHCP message.
type Packet struct {
	Op      byte
	HType   byte
	HLen    byte
	XID     uint32
	Flags   uint16
	CIAddr  [4]byte
	YIAddr  [4]byte
	SIAddr  [4]byte
	GIAddr  [4]byte
	CHAddr  [16]byte
	SName   string
	File    string
	Options map[byte][]byte
}

// Parse decodes a message. Repeated options are concatenated (RFC 3396).
func Parse(b []byte) (*Packet, error) {
	if len(b) < optionsAt {
		return nil, fmt.Errorf("dhcp: packet of %d bytes is too short", len(b))
	}
	if !bytes.Equal(b[headerSize:optionsAt], magicCookie) {
		return nil, errors.New("dhcp: bad magic cookie")
	}
	p := &Packet{
		Op:      b[0],
		HType:   b[1],
		HLen:    b[2],
		XID:     binary.BigEndian.Uint32(b[4:]),
		Flags:   binary.BigEndian.Uint16(b[10:]),
		SName:   cString(b[44:108]),
		File:    cString(b[108:236]),
		Options: make(map[byte][]byte),
	}
	copy(p.CIAddr[:], b[12:])
	copy(p.YIAddr[:], b[16:])
	copy(p.SIAddr[:], b[20:])
	copy(p.GIAddr[:], b[24:])
	copy(p.CHAddr[:], b[28:])

	for i := optionsAt; i < len(b); {
		code := b[i]
		i++
		if code == 0 {
			continue
		}
		if code == 255 {
			break
		}
		if i >= len(b) {
			return nil, fmt.Errorf("dhcp: option %d has no length", code)
		}
		n := int(b[i])
		i++
		if i+n > len(b) {
			return nil, fmt.Errorf("dhcp: option %d overruns the packet", code)
		}
		p.Options[code] = append(p.Options[code], b[i:i+n]...)
		i += n
	}
	return p, nil
}

// Marshal encodes the message, padded to the BOOTP minimum size.
func (p *Packet) Marshal() ([]byte, error) {
	if len(p.SName) > maxSName {
		return nil, fmt.Errorf("dhcp: server name of %d bytes is too long", len(p.SName))
	}
	if len(p.File) > maxFile {
		return nil, fmt.Errorf("dhcp: boot file name of %d bytes is too long", len(p.File))
	}

	b := make([]byte, optionsAt, minPacket)
	b[0], b[1], b[2] = p.Op, p.HType, p.HLen
	binary.BigEndian.PutUint32(b[4:], p.XID)
	binary.BigEndian.PutUint16(b[10:], p.Flags)
	copy(b[12:], p.CIAddr[:])
	copy(b[16:], p.YIAddr[:])
	copy(b[20:], p.SIAddr[:])
	copy(b[24:], p.GIAddr[:])
	copy(b[28:], p.CHAddr[:])
	copy(b[44:], p.SName)
	copy(b[108:], p.File)
	copy(b[headerSize:], magicCookie)

	codes := make([]byte, 0, len(p.Options))
	for code := range p.Options {
		codes = append(codes, code)
	}
	slices.Sort(codes)
	// The message type goes first: some firmware only looks for it there.
	if v, ok := p.Options[OptMessageType]; ok {
		b = append(b, OptMessageType, byte(len(v)))
		b = append(b, v...)
	}
	for _, code := range codes {
		v := p.Options[code]
		if len(v) > 255 {
			return nil, fmt.Errorf("dhcp: option %d of %d bytes is too long", code, len(v))
		}
		if code == OptMessageType {
			continue
		}
		b = append(b, code, byte(len(v)))
		b = append(b, v...)
	}
	b = append(b, 255)
	if len(b) < minPacket {
		b = b[:minPacket]
	}
	return b, nil
}

// MAC returns the client hardware address, or nil if it is not Ethernet.
func (p *Packet) MAC() net.HardwareAddr {
	if p.HType != 1 || p.HLen != 6 {
		return nil
	}
	return slices.Clone(net.HardwareAddr(p.CHAddr[:6]))
}

// messageType returns the DHCP message type, or 0 for plain BOOTP.
func (p *Packet) messageType() byte {
	if v := p.Options[OptMessageType]; len(v) == 1 {
		return v[0]
	}
	return 0
}

// cString returns the part of a fixed-size field before the first NUL.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
