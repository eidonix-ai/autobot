// Package netguard keeps Autobot's outgoing requests to the public internet. Any address a person
// or a model can choose (a provider's endpoint, a search API, a page an agent fetches) could
// otherwise point Autobot at its own network: the LAN behind it, the host itself, or, through the
// router's hairpin, the DMZ proxy's sites that are limited to the local network. The check is made
// on each address as it is dialled, after its name is resolved, so a name pointed at a private
// address, or a redirect to one, gets no further. Hosts an admin has listed as trusted (a local
// inference server, a gateway on Autobot's own public address) are exempt, by name.
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PublicOnly refuses a connection to anything but an ordinary public address that is not
// Autobot's own. It is a net.Dialer Control function.
func PublicOnly(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ownAddress(ip) {
		return fmt.Errorf("%s is not a public address", ip)
	}
	for _, p := range notPublic {
		if p.Contains(ip) {
			return fmt.Errorf("%s is not a public address", ip)
		}
	}
	return nil
}

// notPublic are ranges that are not ordinary public addresses, beyond what netip names: shared
// (carrier NAT), "this network", protocol and benchmark ranges, the old reserved block, and IPv6
// forms that carry an IPv4 address (NAT64, 6to4, IPv4-compatible) or are site-local.
var notPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{"100.64.0.0/10", "0.0.0.0/8", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "2002::/16", "::/96", "fec0::/10", "100::/64"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Autobot's own public addresses, learned from the names it is reached by.
var own struct {
	sync.Mutex
	names map[string]bool
	addrs map[netip.Addr]bool
}

func ownAddress(ip netip.Addr) bool {
	own.Lock()
	defer own.Unlock()
	return own.addrs[ip]
}

// NoteOwnHost records a name Autobot is reached by (a request's Host) and refuses its addresses
// from then on. Names are resolved once each, at most 16 of them.
func NoteOwnHost(host string) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil {
		return
	}
	own.Lock()
	if own.names[host] || len(own.names) >= 16 {
		own.Unlock()
		return
	}
	if own.names == nil {
		own.names, own.addrs = map[string]bool{}, map[netip.Addr]bool{}
	}
	own.names[host] = true
	own.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return
		}
		own.Lock()
		defer own.Unlock()
		for _, ip := range ips {
			own.addrs[ip.Unmap()] = true
		}
	}()
}

// trusted are the hosts an admin has allowed to be on Autobot's own network, lower case.
var trusted struct {
	sync.RWMutex
	hosts map[string]bool
}

// SetTrusted replaces the trusted hosts: names or addresses, each perhaps with a port.
func SetTrusted(hosts []string) {
	m := map[string]bool{}
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			m[h] = true
		}
	}
	trusted.Lock()
	trusted.hosts = m
	trusted.Unlock()
}

// Trusted reports whether host (host or host:port, as a URL has it) is one an admin trusts.
func Trusted(hostport string) bool {
	hostport = strings.ToLower(hostport)
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = strings.Trim(h, "[]")
	}
	trusted.RLock()
	defer trusted.RUnlock()
	return trusted.hosts[hostport] || trusted.hosts[host]
}

var (
	guarded = &net.Dialer{Timeout: 10 * time.Second, Control: PublicOnly}
	open    = &net.Dialer{Timeout: 10 * time.Second}
)

// DialContext dials a trusted host as it is, and any other only at a public address.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if Trusted(address) {
		return open.DialContext(ctx, network, address)
	}
	return guarded.DialContext(ctx, network, address)
}

// Transport is an HTTP transport whose connections go through DialContext, with no proxy.
func Transport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	}
}
