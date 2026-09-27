package mail_test

import (
	"net"
	"strconv"
)

func splitAddr(addr string) (string, int) {
	h, p, _ := net.SplitHostPort(addr)
	n, _ := strconv.Atoi(p)
	return h, n
}
