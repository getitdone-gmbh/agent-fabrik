// Package netguard provides dialers and HTTP transports that refuse to
// connect to loopback, private, link-local and other internal addresses.
// Tenant-controlled connections go through it so users cannot reach the
// cluster network (SSRF).
package netguard

import (
	"context"
	"fmt"
	"io"
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

// TenantMaxResponseBytes caps one response body on TenantTransport. Tenants
// share one process and its memory limit: without a cap a tenant's own
// model, MCP or API server could send 32 MB per call, times parallel tools
// times runs, and take every tenant on the replica down with an OOM kill.
// 1 MiB is far above any real answer: a model reply is bounded by max_tokens
// (8192), and tool results are cut to max_tool_output (50 KB) anyway.
const TenantMaxResponseBytes = 1 << 20

// TenantTransport is Transport with each response body capped at
// TenantMaxResponseBytes. Reading past the cap fails instead of truncating,
// so a cut-off JSON body is an error, never a silently partial result.
var TenantTransport http.RoundTripper = capped{Transport, TenantMaxResponseBytes}

type capped struct {
	rt  http.RoundTripper
	max int64
}

func (c capped) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &cappedBody{rc: resp.Body, left: c.max, max: c.max}
	return resp, nil
}

type cappedBody struct {
	rc        io.ReadCloser
	left, max int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		// One byte more tells a body of exactly max bytes from a longer one.
		var one [1]byte
		if n, _ := b.rc.Read(one[:]); n > 0 {
			return 0, fmt.Errorf("response larger than %d bytes", b.max)
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }
