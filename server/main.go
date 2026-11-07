// Command server is the FOGs deployment server: proxyDHCP, TFTP, the iPXE boot
// scripts and the operator panel, with the state in a SQLite file.
//
// Everything lives in one folder, -root or the folder it starts in: EFI\ holds what
// clients boot and is the only thing served over the network, data\ holds the
// database and the log. Disk images live where the panel says, outside EFI\.
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
	"path/filepath"
	"runtime/debug"
	"strconv"
	"sync"
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

// Folders inside the root.
const (
	efiDir  = "EFI"
	dataDir = "data"
)

// oldDatabase is where the database lay before data\ existed.
const oldDatabase = "fogs.db"

func main() {
	var cfg config
	flag.StringVar(&cfg.root, "root", "", `folder of the server, with EFI\ and data\ inside; the current folder if empty`)
	flag.StringVar(&cfg.bind, "bind", "", "IPv4 address for the client services; chosen in the panel if empty")
	flag.StringVar(&cfg.db, "db", filepath.Join(dataDir, "fogs.db"), "SQLite database file; a relative path is inside -root")
	flag.StringVar(&cfg.admin, "admin-addr", "127.0.0.1:8081", "address of the operator panel; must be loopback, it has no login")
	flag.StringVar(&cfg.bootFile, "bootfile", boot.ShimPath, `first-stage boot file, relative to EFI\`)
	flag.IntVar(&cfg.httpPort, "http-port", 8080, "port of the HTTP server for clients")
	flag.StringVar(&cfg.log, "log", filepath.Join(dataDir, "locless.log"), "log file, a relative path is inside -root; a crash no code could catch goes to the same name with .crash")
	verbose := flag.Bool("v", false, "log debug messages")
	flag.Parse()

	rootErr := cfg.resolve()
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
	err := rootErr
	if err == nil {
		err = recovery.Run(func() error { return run(ctx, cfg, log) })
	}
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
	root     string
	bind     string
	db       string
	admin    string
	bootFile string
	httpPort int
	log      string
}

// resolve makes the root absolute and the database and the log paths inside it,
// creating the folders they need, so the log works before anything else does.
func (c *config) resolve() error {
	root := c.root
	if root == "" {
		root = "."
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	c.root = root
	for _, p := range []*string{&c.db, &c.log} {
		if !filepath.IsAbs(*p) {
			*p = filepath.Join(root, *p)
		}
		if err := os.MkdirAll(filepath.Dir(*p), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// parseBind returns the address given with -bind, or none. A wildcard is refused:
// CCBoot or another DHCP server may own these ports on other addresses.
func parseBind(s string) (netip.Addr, error) {
	if s == "" {
		return netip.Addr{}, nil
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("bad -bind %q: %w", s, err)
	}
	if !ip.Is4() || ip.IsUnspecified() {
		return netip.Addr{}, fmt.Errorf("-bind must be a specific IPv4 address, got %q", s)
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

// checkOldDatabase stops a server that would start on an empty database while the
// one with every PC and image still lies where it used to.
func checkOldDatabase(root, db string) error {
	old := filepath.Join(root, oldDatabase)
	if db == old {
		return nil
	}
	if _, err := os.Stat(db); err == nil {
		return nil
	}
	if _, err := os.Stat(old); err != nil {
		return nil
	}
	return fmt.Errorf("the database is still at %s: with the server stopped, move fogs.db, fogs.db-wal and fogs.db-shm into %s", old, filepath.Dir(db))
}

// listenUDP binds one service port on the server address.
func listenUDP(ip netip.Addr, port int) (*net.UDPConn, error) {
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(port))))
	if err != nil {
		return nil, fmt.Errorf("cannot bind %s port %d: %w", ip, port, err)
	}
	return conn, nil
}

// udpService binds a port and serves it until ctx ends.
func udpService(ip netip.Addr, port int, serve func(context.Context, *net.UDPConn) error) func(context.Context, func()) error {
	return func(ctx context.Context, up func()) error {
		conn, err := listenUDP(ip, port)
		if err != nil {
			return err
		}
		defer conn.Close()
		up()
		return serve(ctx, conn)
	}
}

// httpService listens on addr and serves until ctx ends. A closed http.Server
// cannot serve again, so every start makes a new one.
func httpService(addr string, newServer func() *http.Server) func(context.Context, func()) error {
	return func(ctx context.Context, up func()) error {
		ln, err := net.Listen("tcp4", addr)
		if err != nil {
			return fmt.Errorf("cannot bind %s: %w", addr, err)
		}
		s := newServer()
		stop := context.AfterFunc(ctx, func() { s.Close() })
		defer stop()
		up()
		if err := s.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// run serves until ctx ends. The panel starts at once; the client services wait
// until the panel has an image directory and an address for them.
func run(ctx context.Context, cfg config, log *slog.Logger) error {
	bind, err := parseBind(cfg.bind)
	if err != nil {
		return err
	}
	if err := checkAdminAddr(cfg.admin); err != nil {
		return err
	}
	efiPath := filepath.Join(cfg.root, efiDir)
	if err := os.MkdirAll(efiPath, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(cfg.root, boot.ShimPath)); err == nil {
		log.Error(`boot files lie in the server folder itself, from the time it was the -root of old: move them into EFI\`, "root", cfg.root)
	}
	efi, err := os.OpenRoot(efiPath)
	if err != nil {
		return err
	}
	defer efi.Close()
	bootFiles := []string{cfg.bootFile, boot.IPXEPath, boot.WimbootPath, boot.BootWimPath, boot.AgentPath}
	missing := func() []string {
		var gone []string
		for _, name := range bootFiles {
			if _, err := efi.Stat(name); err != nil {
				gone = append(gone, name)
			}
		}
		return gone
	}
	for _, name := range missing() {
		log.Warn(`boot file is missing from EFI\`, "file", name)
	}

	if err := checkOldDatabase(cfg.root, cfg.db); err != nil {
		return err
	}
	db, err := store.Open(cfg.db)
	if err != nil {
		return err
	}
	defer db.Close()
	images := imaging.New(db, log.With("service", "imaging"))
	if err := images.CleanIncoming(ctx); err != nil {
		log.Warn("unfinished files not cleaned", "err", err)
	}

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

	seen := dhcp.NewClients() // outlives the services, so a restart keeps what it learned
	clientServices := func(addr netip.Addr) []service {
		httpAddr := net.JoinHostPort(addr.String(), strconv.Itoa(cfg.httpPort))
		proxy := &dhcp.Proxy{
			ServerIP: addr,
			BootFile: cfg.bootFile,
			ScriptURL: func(mac net.HardwareAddr) string {
				return fmt.Sprintf("http://%s/boot.ipxe?mac=%s", httpAddr, mac)
			},
			Allow:   known,
			Seen:    record("seen", db.Seen),
			Clients: seen,
			Log:     log.With("service", "dhcp"),
		}
		files := &tftp.Server{
			Root: efi,
			Generated: func(name string, client netip.Addr) []byte {
				if name != boot.AutoexecName {
					return nil
				}
				// Only a PC known to have no task gets the short script: a database
				// error or an address not seen on port 4011 gets the whole one.
				idle := false
				if mac, ok := seen.Lookup(client); ok {
					task, err := db.HasTask(ctx, mac)
					idle = err == nil && !task
				}
				return boot.Autoexec(httpAddr, exitMode(), idle)
			},
			Log: log.With("service", "tftp"),
		}
		handler := recovery.Handler(log.With("service", "http"), boot.New(boot.Config{
			HTTPAddr: httpAddr,
			Known:    known,
			HasTask:  check("task", db.HasTask),
			Started:  record("started", db.MarkStarted),
			ExitMode: exitMode,
			Files:    efi.FS(),
			Agent:    db,
			Clone:    images,
			Deploy:   images,
			Log:      log.With("service", "http"),
		}))
		return []service{
			{"proxyDHCP", udpService(addr, dhcp.ServerPort, proxy.ServeDHCP)},
			{"proxyDHCP 4011", udpService(addr, dhcp.BootPort, proxy.ServeBoot)},
			{"TFTP", udpService(addr, tftp.Port, files.Serve)},
			{"HTTP", httpService(httpAddr, func() *http.Server {
				// A PC switched off in the middle of a request must not hold a clone
				// for long: reading a request has a limit, a batch takes seconds.
				return &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute}
			})},
		}
	}
	resolve := func(ctx context.Context) setup {
		dir, err := db.Setting(ctx, store.SettingImageDir)
		if err == nil && dir == "" {
			return setup{waiting: "Укажите папку образов в настройках"}
		}
		chosen, err2 := db.Setting(ctx, store.SettingClientAddr)
		if err = errors.Join(err, err2); err != nil {
			log.Error("database query failed", "query", "settings", "err", err)
			return setup{waiting: "Ошибка базы данных, подробности в логе"}
		}
		addr, why := chooseAddr(bind, chosen, localAddrs())
		return setup{addr: addr, waiting: why}
	}
	clients := newSupervisor(log.With("service", "clients"), resolve, clientServices)

	panel := web.New(web.Config{
		Store: db, Images: images, Addr: cfg.admin, Root: efiPath, Log: log.With("service", "panel"),
		Status: func() web.Status {
			st := clients.Status()
			st.MissingFiles = missing()
			return st
		},
		Changed: clients.Changed,
		Addresses: func() []string {
			var list []string
			for _, a := range localAddrs() {
				list = append(list, a.String())
			}
			return list
		},
		Bind: cfg.bind,
	})
	panelService := httpService(cfg.admin, func() *http.Server {
		return &http.Server{Handler: recovery.Handler(log.With("service", "panel"), panel), ReadHeaderTimeout: 10 * time.Second}
	})

	var wg sync.WaitGroup
	wg.Go(func() {
		recovery.Keep(ctx, log, "panel", func() error { return panelService(ctx, func() {}) })
	})
	wg.Go(func() {
		recovery.Keep(ctx, log, "imaging", func() error { images.Run(ctx); return nil })
	})
	wg.Go(func() {
		recovery.Keep(ctx, log, "clients", func() error { clients.run(ctx); return nil })
	})
	log.Info("server started", "root", cfg.root, "panel", "http://"+cfg.admin, "db", cfg.db)
	wg.Wait()
	return nil
}
