// Command server is the FOGs deployment server: proxyDHCP, TFTP, the iPXE boot
// scripts and the operator panel, with the state in a SQLite file.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"time"

	"fogs/proto"
	"fogs/recovery"
	"fogs/server/boot"
	"fogs/server/dhcp"
	"fogs/server/imaging"
	"fogs/server/store"
	"fogs/server/tftp"
	"fogs/server/web"
)

func main() {
	var cfg config
	flag.StringVar(&cfg.ip, "ip", "", "IPv4 address of this server on the client network (required)")
	flag.StringVar(&cfg.root, "root", "", "directory served over TFTP and HTTP (required)")
	flag.StringVar(&cfg.db, "db", "fogs.db", "SQLite database file")
	flag.StringVar(&cfg.admin, "admin-addr", "127.0.0.1:8081", "address of the operator panel; must be loopback, it has no login")
	flag.StringVar(&cfg.bootFile, "bootfile", "ipxe.efi", "first-stage boot file, relative to -root")
	flag.IntVar(&cfg.httpPort, "http-port", 8080, "port of the HTTP server for clients")
	flag.StringVar(&cfg.log, "log", "locless.log", "log file; a crash no code could catch goes to the same name with .crash")
	verbose := flag.Bool("v", false, "log debug messages")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	// The file comes first: a service has no stderr, and a failed write ends the list.
	var out io.Writer = os.Stderr
	file, fileErr := openLog(cfg.log)
	if fileErr == nil {
		out = io.MultiWriter(file, os.Stderr)
	}
	log := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))
	if fileErr != nil {
		log.Warn("no log file, logging to stderr only", "err", fileErr)
	}
	keepCrashes(cfg.log+".crash", log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := recovery.Run(func() error { return run(ctx, cfg, log) })
	if err != nil {
		log.Error("server stopped", "err", err, "stack", recovery.StackOf(err))
		os.Exit(1)
	}
}

// keepCrashes sends what the runtime prints when it dies to a file. Some failures,
// like running out of memory, cannot be recovered from, and this is all they leave.
func keepCrashes(path string, log *slog.Logger) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Warn("no crash file", "err", err)
		return
	}
	defer f.Close() // the runtime keeps its own copy of the handle
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		log.Warn("no crash file", "err", err)
	}
}

// config holds the command line.
type config struct {
	ip       string
	root     string
	db       string
	admin    string
	bootFile string
	httpPort int
	log      string
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

// checkAdminAddr refuses anything but loopback. A task can take over a PC and
// the panel has no login, so it must not be reachable from the club network.
func checkAdminAddr(s string) error {
	addr, err := netip.ParseAddrPort(s)
	if err != nil {
		return fmt.Errorf("bad -admin-addr %q: %w", s, err)
	}
	if !addr.Addr().IsLoopback() {
		return fmt.Errorf("-admin-addr must be a loopback address until the panel has a login, got %q", s)
	}
	return nil
}

// listenUDP binds one service port on the server address.
func listenUDP(ip netip.Addr, port int, name string) (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(port))))
	if err != nil {
		return nil, fmt.Errorf("%s: cannot bind %s port %d: %w", name, ip, port, err)
	}
	return conn, nil
}

// serveHTTP runs an HTTP server until it is closed.
func serveHTTP(s *http.Server, ln net.Listener) error {
	if err := s.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// run binds every port first, so a conflict stops the server before it answers
// anyone, then serves until ctx ends or a service fails.
func run(ctx context.Context, cfg config, log *slog.Logger) error {
	ip, err := parseIP(cfg.ip)
	if err != nil {
		return err
	}
	if err := checkAdminAddr(cfg.admin); err != nil {
		return err
	}
	if cfg.root == "" {
		return errors.New("-root is required")
	}
	root, err := os.OpenRoot(cfg.root)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range []string{cfg.bootFile, boot.WimbootPath, boot.BootWimPath, boot.AgentPath} {
		if _, err := root.Stat(name); err != nil {
			log.Warn("boot file is missing from -root", "file", name)
		}
	}
	db, err := store.Open(cfg.db)
	if err != nil {
		return err
	}
	defer db.Close()

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
	adminLn, err := net.Listen("tcp4", cfg.admin)
	if err != nil {
		return fmt.Errorf("panel: cannot bind %s: %w", cfg.admin, err)
	}
	defer adminLn.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// A database error must leave the client alone, so it counts as "no".
	check := func(name string, query func(context.Context, net.HardwareAddr) (bool, error)) func(net.HardwareAddr) bool {
		return func(mac net.HardwareAddr) bool {
			ok, err := query(ctx, mac)
			if err != nil {
				log.Error("database query failed", "query", name, "mac", mac.String(), "err", err)
			}
			return ok && err == nil
		}
	}
	record := func(name string, update func(context.Context, net.HardwareAddr) error) func(net.HardwareAddr) {
		return func(mac net.HardwareAddr) {
			if err := update(ctx, mac); err != nil {
				log.Error("database update failed", "update", name, "mac", mac.String(), "err", err)
			}
		}
	}
	known := check("host", db.IsHost)

	// exitMode falls back to the default rather than failing a boot.
	exitMode := func() string {
		mode, err := db.Setting(ctx, store.SettingExitMode)
		if err != nil {
			log.Error("database query failed", "query", "exit mode", "err", err)
		}
		if !proto.ValidExitMode(mode) {
			return proto.ExitSanboot
		}
		return mode
	}

	proxy := &dhcp.Proxy{
		ServerIP: ip,
		BootFile: cfg.bootFile,
		ScriptURL: func(mac net.HardwareAddr) string {
			return fmt.Sprintf("http://%s/boot.ipxe?mac=%s", httpAddr, mac)
		},
		Allow: known,
		Seen:  record("seen", db.Seen),
		Log:   log.With("service", "dhcp"),
	}
	files := &tftp.Server{
		Root: root,
		Generated: func(name string) []byte {
			if name == boot.AutoexecName {
				return boot.Autoexec(httpAddr, exitMode())
			}
			return nil
		},
		Log: log.With("service", "tftp"),
	}
	images := imaging.New(db, log.With("service", "imaging"))
	clients := &http.Server{
		Handler: recovery.Handler(log.With("service", "http"), boot.New(boot.Config{
			HTTPAddr: httpAddr,
			Known:    known,
			HasTask:  check("task", db.HasTask),
			Started:  record("started", db.MarkStarted),
			ExitMode: exitMode,
			Files:    root.FS(),
			Agent:    db,
			Clone:    images,
			Deploy:   images,
			Log:      log.With("service", "http"),
		})),
		ReadHeaderTimeout: 10 * time.Second,
	}
	panel := &http.Server{
		Handler:           recovery.Handler(log.With("service", "panel"), web.New(web.Config{Store: db, Images: images, Addr: cfg.admin, Root: cfg.root, Log: log.With("service", "panel")})),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// A panic in one service must not stop the others: it logs and starts again.
	keep := func(name string, serve func() error) func() error {
		return func() error { return recovery.Keep(ctx, log, name, serve) }
	}
	services := []func() error{
		keep("dhcp", func() error { return proxy.ServeDHCP(ctx, dhcpConn) }),
		keep("boot", func() error { return proxy.ServeBoot(ctx, bootConn) }),
		keep("tftp", func() error { return files.Serve(ctx, tftpConn) }),
		keep("http", func() error { return serveHTTP(clients, httpLn) }),
		keep("panel", func() error { return serveHTTP(panel, adminLn) }),
		keep("imaging", func() error { images.Run(ctx); return nil }),
	}
	errs := make(chan error, len(services))
	for _, serve := range services {
		go func() { errs <- serve() }()
	}
	stopHTTP := context.AfterFunc(ctx, func() {
		clients.Close()
		panel.Close()
	})
	defer stopHTTP()

	log.Info("server started", "ip", ip, "http", httpAddr, "panel", "http://"+cfg.admin, "root", cfg.root, "db", cfg.db)

	var first error
	for range services {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
		cancel()
	}
	return first
}
