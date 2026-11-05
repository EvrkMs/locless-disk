// Command server is the FOGs deployment server. So far it covers network boot:
// proxyDHCP, TFTP and the iPXE boot script.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"fogs/server/boot"
	"fogs/server/dhcp"
	"fogs/server/tftp"
)

func main() {
	var cfg config
	flag.StringVar(&cfg.ip, "ip", "", "IPv4 address of this server on the client network (required)")
	flag.StringVar(&cfg.root, "root", "", "directory served over TFTP (required)")
	flag.StringVar(&cfg.allow, "allow-mac", "", "comma-separated MACs to answer; empty means listen and log only")
	flag.StringVar(&cfg.bootFile, "bootfile", "ipxe.efi", "first-stage boot file, relative to -root")
	flag.IntVar(&cfg.httpPort, "http-port", 8080, "port of the HTTP server")
	verbose := flag.Bool("v", false, "log debug messages")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, cfg, log); err != nil {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// config holds the command line.
type config struct {
	ip       string
	root     string
	allow    string
	bootFile string
	httpPort int
}

// parseIP returns the server address. A wildcard is refused: other services
// may own these ports on other addresses of the same machine.
func parseIP(s string) (netip.Addr, error) {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("bad -ip %q: %w", s, err)
	}
	if !ip.Is4() || ip.IsUnspecified() {
		return netip.Addr{}, fmt.Errorf("-ip must be a specific IPv4 address, got %q", s)
	}
	return ip, nil
}

// parseMACs reads the allow list into a set keyed by canonical MAC.
func parseMACs(s string) (map[string]bool, error) {
	set := make(map[string]bool)
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		mac, err := net.ParseMAC(item)
		if err != nil || len(mac) != 6 {
			return nil, fmt.Errorf("bad MAC %q in -allow-mac", item)
		}
		set[mac.String()] = true
	}
	return set, nil
}

// listenUDP binds one service port on the server address.
func listenUDP(ip netip.Addr, port int, name string) (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(port))))
	if err != nil {
		return nil, fmt.Errorf("%s: cannot bind %s port %d: %w", name, ip, port, err)
	}
	return conn, nil
}

// run binds every port first, so a conflict stops the server before it answers
// anyone, then serves until ctx ends or a service fails.
func run(ctx context.Context, cfg config, log *slog.Logger) error {
	ip, err := parseIP(cfg.ip)
	if err != nil {
		return err
	}
	if cfg.root == "" {
		return errors.New("-root is required")
	}
	allow, err := parseMACs(cfg.allow)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(cfg.root)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := root.Stat(cfg.bootFile); err != nil {
		log.Warn("boot file is missing from -root, clients will fail to load it", "file", cfg.bootFile)
	}

	dhcpConn, err := listenUDP(ip, dhcp.ServerPort, "proxyDHCP")
	if err != nil {
		return err
	}
	defer dhcpConn.Close()
	bootConn, err := listenUDP(ip, dhcp.BootPort, "proxyDHCP")
	if err != nil {
		return err
	}
	defer bootConn.Close()
	tftpConn, err := listenUDP(ip, tftp.Port, "TFTP")
	if err != nil {
		return err
	}
	defer tftpConn.Close()
	httpAddr := net.JoinHostPort(ip.String(), strconv.Itoa(cfg.httpPort))
	httpLn, err := net.Listen("tcp4", httpAddr)
	if err != nil {
		return fmt.Errorf("HTTP: cannot bind %s: %w", httpAddr, err)
	}
	defer httpLn.Close()

	known := func(mac net.HardwareAddr) bool { return allow[mac.String()] }
	proxy := &dhcp.Proxy{
		ServerIP: ip,
		BootFile: cfg.bootFile,
		ScriptURL: func(mac net.HardwareAddr) string {
			return fmt.Sprintf("http://%s/boot.ipxe?mac=%s", httpAddr, mac)
		},
		Allow: known,
		Log:   log.With("service", "dhcp"),
	}
	files := &tftp.Server{Root: root, Log: log.With("service", "tftp")}
	web := &http.Server{
		Handler:           boot.New(known, log.With("service", "http")),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	services := []func() error{
		func() error { return proxy.ServeDHCP(ctx, dhcpConn) },
		func() error { return proxy.ServeBoot(ctx, bootConn) },
		func() error { return files.Serve(ctx, tftpConn) },
		func() error {
			if err := web.Serve(httpLn); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	errs := make(chan error, len(services))
	for _, serve := range services {
		go func() { errs <- serve() }()
	}
	stopWeb := context.AfterFunc(ctx, func() { web.Close() })
	defer stopWeb()

	if len(allow) == 0 {
		log.Warn("allow list is empty: listening and logging only, no client will be answered")
	}
	log.Info("server started", "ip", ip, "http", httpAddr, "root", cfg.root, "allowed", len(allow))

	var first error
	for range services {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
		cancel()
	}
	return first
}
