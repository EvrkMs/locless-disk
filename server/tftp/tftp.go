// Package tftp is a read-only TFTP server (RFC 1350) for network boot, with the
// blksize, timeout, tsize and windowsize options (RFC 2347-2349, RFC 7440).
package tftp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port is the well-known TFTP port.
const Port = 69

// Opcodes.
const (
	opRRQ   = 1
	opWRQ   = 2
	opData  = 3
	opAck   = 4
	opError = 5
	opOACK  = 6
)

// Error codes.
const (
	errNotDefined = 0
	errNotFound   = 1
	errAccess     = 2
	errIllegal    = 4
)

// Negotiation limits.
const (
	defaultBlockSize = 512
	minBlockSize     = 8
	maxBlockSize     = 65464
	maxWindow        = 64 // far below 65535, so block numbers stay unambiguous
)

// Server serves the files under Root.
type Server struct {
	Root         *os.Root      // files are confined to this directory
	Timeout      time.Duration // wait for an ACK; zero means 1s
	Retries      int           // resends before giving up; zero means 5
	MaxTransfers int           // concurrent transfers; zero means 64
	Log          *slog.Logger
}

// request is a decoded RRQ.
type request struct {
	file    string
	options map[string]string
}

// parseRequest decodes the body of an RRQ: NUL-terminated file name, mode and
// option name/value pairs.
func parseRequest(b []byte) (request, error) {
	fields := bytes.Split(b, []byte{0})
	n := len(fields)
	if n < 3 || len(fields[n-1]) != 0 {
		return request{}, errors.New("malformed request")
	}
	fields = fields[:n-1] // the empty string after the final NUL
	if !strings.EqualFold(string(fields[1]), "octet") {
		return request{}, fmt.Errorf("unsupported mode %q", fields[1])
	}
	opts := fields[2:]
	if len(opts)%2 != 0 {
		return request{}, errors.New("option without a value")
	}
	r := request{file: string(fields[0]), options: make(map[string]string, len(opts)/2)}
	for i := 0; i < len(opts); i += 2 {
		r.options[strings.ToLower(string(opts[i]))] = string(opts[i+1])
	}
	return r, nil
}

// Serve answers read requests on conn until ctx ends, then waits for running
// transfers to stop.
func (s *Server) Serve(ctx context.Context, conn *net.UDPConn) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	limit := s.MaxTransfers
	if limit == 0 {
		limit = 64
	}
	slots := make(chan struct{}, limit)
	var running sync.WaitGroup
	defer running.Wait()

	local := conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap()
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
			s.Log.Debug("read failed", "err", err)
			continue
		}
		src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		if n < 2 {
			continue
		}
		reject := func(code uint16, msg string) {
			if _, err := conn.WriteToUDPAddrPort(errorPacket(code, msg), src); err != nil {
				s.Log.Debug("cannot send error", "err", err)
			}
		}
		switch binary.BigEndian.Uint16(buf) {
		case opRRQ:
		case opWRQ:
			reject(errAccess, "server is read-only")
			continue
		default:
			reject(errIllegal, "expected a read request")
			continue
		}
		req, err := parseRequest(buf[2:n])
		if err != nil {
			reject(errIllegal, err.Error())
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			reject(errNotDefined, "server busy")
			continue
		}
		running.Add(1)
		go func() {
			defer running.Done()
			defer func() { <-slots }()
			s.transfer(ctx, local, src, req)
		}()
	}
}

// open resolves a client path inside Root. Windows boot loaders send
// backslashes and often a leading separator.
func (s *Server) open(name string) (*os.File, int64, uint16, error) {
	name = strings.TrimLeft(strings.ReplaceAll(name, `\`, "/"), "/")
	if name == "" || !filepath.IsLocal(filepath.FromSlash(name)) {
		return nil, 0, errAccess, errors.New("access denied")
	}
	f, err := s.Root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, errNotFound, errors.New("file not found")
	}
	if err != nil {
		return nil, 0, errAccess, errors.New("access denied")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, 0, errNotFound, errors.New("file not found")
	}
	return f, info.Size(), 0, nil
}

// session is one running transfer.
type session struct {
	conn      *net.UDPConn
	file      io.ReaderAt
	size      int64
	blockSize int
	window    int
	timeout   time.Duration
	retries   int
	in        []byte
	out       []byte
}

// transfer sends one file from its own socket, whose port is the transfer ID.
func (s *Server) transfer(ctx context.Context, local netip.Addr, client netip.AddrPort, req request) {
	log := s.Log.With("client", client, "file", req.file)
	conn, err := net.DialUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(local, 0)), net.UDPAddrFromAddrPort(client))
	if err != nil {
		log.Warn("cannot open transfer socket", "err", err)
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	f, size, code, err := s.open(req.file)
	if err != nil {
		log.Info("request refused", "err", err)
		conn.Write(errorPacket(code, err.Error()))
		return
	}
	defer f.Close()

	t := &session{
		conn:      conn,
		file:      f,
		size:      size,
		blockSize: defaultBlockSize,
		window:    1,
		timeout:   s.Timeout,
		retries:   s.Retries,
		in:        make([]byte, 2048),
	}
	if t.timeout == 0 {
		t.timeout = time.Second
	}
	if t.retries == 0 {
		t.retries = 5
	}

	start := time.Now()
	oack := t.negotiate(req.options)
	t.out = make([]byte, 4+t.blockSize)
	if len(oack) > 0 {
		err = t.sendOACK(oack)
	}
	if err == nil {
		err = t.sendFile()
	}
	switch {
	case errors.Is(err, errClientStopped):
		// Firmware routinely asks for tsize and then aborts on purpose.
		log.Debug("client stopped the transfer")
	case err != nil:
		log.Warn("transfer failed", "err", err)
	default:
		log.Info("file sent", "bytes", size, "elapsed", time.Since(start).Round(time.Millisecond))
	}
}

// negotiate applies the options we support and returns the OACK body.
func (t *session) negotiate(opts map[string]string) []byte {
	var oack []byte
	accept := func(name string, value int64) {
		oack = append(oack, name...)
		oack = append(oack, 0)
		oack = strconv.AppendInt(oack, value, 10)
		oack = append(oack, 0)
	}
	if n, err := strconv.Atoi(opts["blksize"]); err == nil && n >= minBlockSize {
		t.blockSize = min(n, maxBlockSize)
		accept("blksize", int64(t.blockSize))
	}
	if n, err := strconv.Atoi(opts["timeout"]); err == nil && n >= 1 && n <= 255 {
		t.timeout = time.Duration(n) * time.Second
		accept("timeout", int64(n))
	}
	if _, ok := opts["tsize"]; ok {
		accept("tsize", t.size)
	}
	if n, err := strconv.Atoi(opts["windowsize"]); err == nil && n >= 1 {
		t.window = min(n, maxWindow)
		accept("windowsize", int64(t.window))
	}
	return oack
}

// errClientStopped means the client sent an ERROR packet.
var errClientStopped = errors.New("tftp: client stopped the transfer")

// errTimeout means the client did not answer in time.
var errTimeout = errors.New("tftp: timed out waiting for the client")

// sendOACK sends the negotiated options until the client acknowledges them.
func (t *session) sendOACK(body []byte) error {
	pkt := append([]byte{0, opOACK}, body...)
	for try := 0; try <= t.retries; try++ {
		if _, err := t.conn.Write(pkt); err != nil {
			return err
		}
		_, err := t.awaitAck(func(block uint16) (uint64, bool) { return 0, block == 0 })
		if !errors.Is(err, errTimeout) {
			return err
		}
	}
	return errTimeout
}

// sendFile streams the file in windows of blocks. A file always ends with a
// block shorter than blockSize, empty if the size is an exact multiple.
func (t *session) sendFile() error {
	last := uint64(t.size)/uint64(t.blockSize) + 1
	base := uint64(1) // first block not yet acknowledged
	tries := 0
	for base <= last {
		end := min(base+uint64(t.window)-1, last)
		for n := base; n <= end; n++ {
			if err := t.sendBlock(n); err != nil {
				return err
			}
		}
		acked, err := t.awaitAck(func(block uint16) (uint64, bool) {
			// Block numbers wrap at 16 bits, so compare by distance from the
			// last acknowledged block. Duplicates are ignored, not answered,
			// to avoid the Sorcerer's Apprentice bug.
			delta := uint64(block - uint16(base-1))
			return base - 1 + delta, delta != 0 && delta <= end-base+1
		})
		if errors.Is(err, errTimeout) {
			if tries++; tries > t.retries {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		base = acked + 1
		tries = 0
	}
	return nil
}

// sendBlock sends block n, counted from 1.
func (t *session) sendBlock(n uint64) error {
	binary.BigEndian.PutUint16(t.out[0:], opData)
	binary.BigEndian.PutUint16(t.out[2:], uint16(n))
	read, err := t.file.ReadAt(t.out[4:], int64(n-1)*int64(t.blockSize))
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	_, err = t.conn.Write(t.out[:4+read])
	return err
}

// awaitAck reads until accept takes an ACK, an ERROR arrives or the timeout runs out.
func (t *session) awaitAck(accept func(block uint16) (uint64, bool)) (uint64, error) {
	if err := t.conn.SetReadDeadline(time.Now().Add(t.timeout)); err != nil {
		return 0, err
	}
	for {
		n, err := t.conn.Read(t.in)
		if err != nil {
			var nerr net.Error
			if errors.As(err, &nerr) && nerr.Timeout() {
				return 0, errTimeout
			}
			return 0, err
		}
		if n < 4 {
			continue
		}
		switch binary.BigEndian.Uint16(t.in) {
		case opError:
			return 0, errClientStopped
		case opAck:
			if acked, ok := accept(binary.BigEndian.Uint16(t.in[2:])); ok {
				return acked, nil
			}
		}
	}
}

// errorPacket encodes an ERROR message.
func errorPacket(code uint16, msg string) []byte {
	b := make([]byte, 4, 5+len(msg))
	binary.BigEndian.PutUint16(b[0:], opError)
	binary.BigEndian.PutUint16(b[2:], code)
	b = append(b, msg...)
	return append(b, 0)
}
