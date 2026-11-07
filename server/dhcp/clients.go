package dhcp

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// clientTTL is how long an address counts as belonging to a PC. iPXE asks for its
// script seconds after port 4011; later the router may give the address to another.
const clientTTL = 10 * time.Minute

// Clients remembers which registered PC asked on port 4011 from which address.
// TFTP sees only the address, and this is how it learns which PC is asking.
type Clients struct {
	mu     sync.Mutex
	byAddr map[netip.Addr]seenClient
}

type seenClient struct {
	mac net.HardwareAddr
	at  time.Time
}

// NewClients returns an empty list.
func NewClients() *Clients {
	return &Clients{byAddr: map[netip.Addr]seenClient{}}
}

// Lookup returns the PC that last asked from addr, if that was recently.
func (c *Clients) Lookup(addr netip.Addr) (net.HardwareAddr, bool) {
	return c.lookup(addr, time.Now())
}

func (c *Clients) lookup(addr netip.Addr, now time.Time) (net.HardwareAddr, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.byAddr[addr]
	if !ok || now.Sub(s.at) > clientTTL {
		return nil, false
	}
	return s.mac, true
}

func (c *Clients) remember(addr netip.Addr, mac net.HardwareAddr, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for a, s := range c.byAddr { // a club has a few hundred PCs at most
		if now.Sub(s.at) > clientTTL {
			delete(c.byAddr, a)
		}
	}
	c.byAddr[addr] = seenClient{mac: mac, at: now}
}
