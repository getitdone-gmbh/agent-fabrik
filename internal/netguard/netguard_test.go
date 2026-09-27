package netguard

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.1.1", "::1", "fd00::1", "fe80::1", "0.0.0.0", "::ffff:10.0.0.1"} {
		if Allowed(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "142.250.185.78", "2a00:1450:4001:82b::200e"} {
		if !Allowed(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestDialBlocked(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	_, err := DialContext(context.Background(), "tcp", ln.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Fatalf("expected block, got %v", err)
	}
}
