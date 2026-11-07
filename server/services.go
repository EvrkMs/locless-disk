package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"

	"fogs/recovery"
	"fogs/server/web"
)

// recheckEvery is how often the client services look again whether they can run:
// a network card may come up later than the server.
const recheckEvery = 30 * time.Second

// service is one thing clients talk to. run binds its port, calls up once it
// listens and serves until ctx ends.
type service struct {
	name string
	run  func(ctx context.Context, up func()) error
}

// setup is what the client services run with, or why they cannot run yet.
type setup struct {
	addr    netip.Addr // where clients reach this server
	waiting string     // what the operator still has to do; "" when ready
}

// supervisor runs the client services: they start once the panel has what they
// need, start over with a new address, and come back after any failure. The panel
// works all the time, so a fresh server is set up from it.
type supervisor struct {
	log      *slog.Logger
	resolve  func(context.Context) setup
	services func(netip.Addr) []service
	changed  chan struct{}

	mu     sync.Mutex
	setup  setup
	states []web.Service
}

func newSupervisor(log *slog.Logger, resolve func(context.Context) setup, services func(netip.Addr) []service) *supervisor {
	return &supervisor{log: log, resolve: resolve, services: services, changed: make(chan struct{}, 1)}
}

// Changed tells the supervisor that the settings may say something new. It never
// blocks: one pending notice is enough.
func (s *supervisor) Changed() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

// Status is what the panel shows about the client services.
func (s *supervisor) Status() web.Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := web.Status{Waiting: s.setup.waiting, Services: slices.Clone(s.states)}
	if s.setup.addr.IsValid() {
		st.Addr = s.setup.addr.String()
	}
	return st
}

// run keeps the services in step with the settings until ctx ends. They start over
// only when the outcome changes, so saving the same thing again breaks no deploy.
func (s *supervisor) run(ctx context.Context) {
	tick := time.NewTicker(recheckEvery)
	defer tick.Stop()
	for {
		want := s.resolve(ctx)
		runCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		s.start(runCtx, &wg, want)
		for same := true; same; {
			select {
			case <-ctx.Done():
				cancel()
				wg.Wait()
				return
			case <-s.changed:
			case <-tick.C:
			}
			same = s.resolve(ctx) == want
		}
		cancel()
		wg.Wait()
	}
}

func (s *supervisor) start(ctx context.Context, wg *sync.WaitGroup, want setup) {
	var services []service
	if want.waiting == "" {
		services = s.services(want.addr)
		s.log.Info("client services starting", "addr", want.addr)
	} else {
		s.log.Info("client services waiting", "reason", want.waiting)
	}
	s.mu.Lock()
	s.setup = want
	s.states = make([]web.Service, len(services))
	for i, svc := range services {
		s.states[i] = web.Service{Name: svc.name}
	}
	s.mu.Unlock()

	for i, svc := range services {
		wg.Go(func() {
			recovery.Keep(ctx, s.log, svc.name, func() error {
				// The panel hears of a panic as well as of an error.
				err := recovery.Run(func() error { return svc.run(ctx, func() { s.set(i, true, "") }) })
				if ctx.Err() == nil {
					if err == nil {
						err = errors.New("служба остановилась")
					}
					s.set(i, false, err.Error())
				}
				return err
			})
		})
	}
}

func (s *supervisor) set(i int, running bool, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < len(s.states) {
		s.states[i].Running, s.states[i].Error = running, err
	}
}

// localAddrs lists the IPv4 addresses of this machine clients could use.
func localAddrs() []netip.Addr {
	var addrs []netip.Addr
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		list, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range list {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if ip = ip.Unmap(); ok && ip.Is4() && !ip.IsLinkLocalUnicast() {
				addrs = append(addrs, ip)
			}
		}
	}
	slices.SortFunc(addrs, netip.Addr.Compare)
	return slices.Compact(addrs)
}

// chooseAddr picks the client address: the one given at start, the one chosen in
// the panel, or the only one the machine has. It returns why there is none.
func chooseAddr(bind netip.Addr, chosen string, local []netip.Addr) (netip.Addr, string) {
	switch {
	case bind.IsValid():
		return bind, ""
	case chosen != "":
		addr, err := netip.ParseAddr(chosen)
		if err != nil || !slices.Contains(local, addr) {
			return netip.Addr{}, "Адрес " + chosen + " больше не принадлежит этой машине: выберите адрес для клиентов в настройках"
		}
		return addr, ""
	case len(local) == 1:
		return local[0], ""
	case len(local) == 0:
		return netip.Addr{}, "У машины нет сетевого адреса IPv4"
	}
	return netip.Addr{}, "Выберите в настройках адрес, по которому компьютеры клуба видят сервер"
}
