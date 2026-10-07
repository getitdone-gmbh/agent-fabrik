package mail

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
)

// A server-declared literal is checked against the command's byte budget
// before anything is allocated, and the budget covers all literals of one
// command together, not each on its own.
func TestIMAPCommandByteBudget(t *testing.T) {
	run := func(limit int64, sizes ...int) error {
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			defer server.Close()
			r := bufio.NewReader(server)
			r.ReadString('\n') // the command
			for i, n := range sizes {
				fmt.Fprintf(server, "* %d FETCH (BODY[] {%d}\r\n%s)\r\n", i+1, n, strings.Repeat("x", n))
			}
			fmt.Fprint(server, "A0001 OK done\r\n")
		}()
		c := &IMAPClient{conn: client, r: bufio.NewReader(client), w: bufio.NewWriter(client), limit: limit, left: limit}
		_, err := c.exec("UID FETCH 1:* (BODY.PEEK[])")
		return err
	}
	if err := run(100, 60, 40); err != nil {
		t.Errorf("within budget: %v", err)
	}
	if err := run(100, 60, 41); err == nil || !strings.Contains(err.Error(), "larger than 100") {
		t.Errorf("two literals over budget together: %v", err)
	}
	if err := run(100, 1_000_000); err == nil {
		t.Error("one oversized literal accepted")
	}
}
