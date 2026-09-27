// Package netguard provides dialers and HTTP transports that refuse to
// connect to loopback, private, link-local and other internal addresses.
// Tenant-controlled connections go through it so users cannot reach the
// cluster network (SSRF).
package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"
)

var blocked = mustCIDRs(
	"0.0.0.0/8",     // "this" network
	"100.64.0.0/10", // carrier-grade NAT (used by some cluster networks)
	"192.0.0.0/24",  // IETF protocol assignments
	"198.18.0.0/15", // benchmarking
	"240.0.0.0/4",   // reserved
	"64:ff9b::/96",  // NAT64 can map to internal IPv4
)

func mustCIDRs(cidrs ...string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

var (
	allowMu   sync.RWMutex
	allowNets []*net.IPNet
)

// Allow permits specific internal networks (e.g. a shared in-cluster
// service) despite the guard.
func Allow(cidrs []string) error {
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return fmt.Errorf("allow_networks: %w", err)
		}
		nets = append(nets, n)
	}
	allowMu.Lock()
	allowNets = nets
	allowMu.Unlock()
	return nil
}

// Allowed reports whether ip is a public unicast address or explicitly
// allowed.
func Allowed(ip net.IP) bool {
	allowMu.RLock()
	for _, n := range allowNets {
		if ip != nil && n.Contains(ip) {
			allowMu.RUnlock()
			return true
		}
	}
	allowMu.RUnlock()
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, n := range blocked {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// Control is a net.Dialer Control hook that enforces Allowed on the address
// actually being connected to (after DNS resolution, so rebinding is covered).
func Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if !Allowed(net.ParseIP(host)) {
		return fmt.Errorf("connection to internal address %s is not allowed", host)
	}
	return nil
}

// Dialer returns a guarded dialer.
func Dialer() *net.Dialer {
	return &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: Control}
}

// DialContext dials through a guarded dialer.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return Dialer().DialContext(ctx, network, addr)
}

// Transport is a shared, guarded HTTP transport. Sharing one transport
// across all tenants keeps idle connections bounded.
var Transport http.RoundTripper = &http.Transport{
	DialContext:           DialContext,
	ForceAttemptHTTP2:     true,
	MaxIdleConns:          512,
	MaxIdleConnsPerHost:   32,
	IdleConnTimeout:       60 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
	ResponseHeaderTimeout: 5 * time.Minute,
}
