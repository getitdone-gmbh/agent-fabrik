// Package testutil provides in-process fake IMAP and SMTP servers for tests.
package testutil

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Message is a mailbox entry in the fake IMAP server.
type Message struct {
	UID  uint32
	Raw  string
	Seen bool
}

// FakeIMAP is a tiny IMAP server supporting the commands the client uses.
type FakeIMAP struct {
	Addr     string
	User     string
	Pass     string
	mu       sync.Mutex
	messages []*Message
	nextUID  uint32
	ln       net.Listener
	Commands []string
}

func NewFakeIMAP(t *testing.T, user, pass string) *FakeIMAP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &FakeIMAP{Addr: ln.Addr().String(), User: user, Pass: pass, ln: ln, nextUID: 100}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *FakeIMAP) Add(raw string) uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUID++
	f.messages = append(f.messages, &Message{UID: f.nextUID, Raw: strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\n", "\r\n")})
	return f.nextUID
}

func (f *FakeIMAP) Seen(uid uint32) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if m.UID == uid {
			return m.Seen
		}
	}
	return false
}

var litRe = regexp.MustCompile(`\{(\d+)\}$`)

func (f *FakeIMAP) readCommand(r *bufio.Reader, w io.Writer) (string, []string, error) {
	var sb strings.Builder
	var lits []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		m := litRe.FindStringSubmatch(line)
		if m == nil {
			sb.WriteString(line)
			return sb.String(), lits, nil
		}
		n, _ := strconv.Atoi(m[1])
		sb.WriteString(line[:len(line)-len(m[0])] + "\x00")
		io.WriteString(w, "+ go ahead\r\n")
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", nil, err
		}
		lits = append(lits, string(buf))
	}
}

// unquote extracts quoted/literal arguments from a command tail.
func unquote(s string, lits []string) []string {
	var out []string
	li := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			var b strings.Builder
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
				b.WriteByte(s[i])
			}
			out = append(out, b.String())
		case 0:
			out = append(out, lits[li])
			li++
		}
	}
	return out
}

func (f *FakeIMAP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	w := c
	io.WriteString(w, "* OK fake IMAP ready\r\n")
	authed := false
	for {
		line, lits, err := f.readCommand(r, w)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.Commands = append(f.Commands, line)
		f.mu.Unlock()
		tag, rest, _ := strings.Cut(line, " ")
		upper := strings.ToUpper(rest)
		switch {
		case strings.HasPrefix(upper, "LOGIN"):
			args := unquote(rest, lits)
			if len(args) == 2 && args[0] == f.User && args[1] == f.Pass {
				authed = true
				fmt.Fprintf(w, "%s OK LOGIN completed\r\n", tag)
			} else {
				fmt.Fprintf(w, "%s NO LOGIN failed\r\n", tag)
			}
		case !authed:
			fmt.Fprintf(w, "%s NO not authenticated\r\n", tag)
		case strings.HasPrefix(upper, "SELECT"):
			f.mu.Lock()
			n := len(f.messages)
			f.mu.Unlock()
			fmt.Fprintf(w, "* %d EXISTS\r\n* 0 RECENT\r\n%s OK [READ-WRITE] SELECT completed\r\n", n, tag)
		case strings.HasPrefix(upper, "UID SEARCH"):
			f.mu.Lock()
			var ids []string
			for _, m := range f.messages {
				if strings.Contains(upper, "UNSEEN") && m.Seen {
					continue
				}
				if strings.Contains(upper, "FROM") {
					args := unquote(rest, lits)
					if len(args) > 0 && !strings.Contains(strings.ToLower(m.Raw), strings.ToLower(args[0])) {
						continue
					}
				}
				ids = append(ids, strconv.Itoa(int(m.UID)))
			}
			f.mu.Unlock()
			fmt.Fprintf(w, "* SEARCH %s\r\n%s OK SEARCH completed\r\n", strings.Join(ids, " "), tag)
		case strings.HasPrefix(upper, "UID FETCH"):
			fields := strings.Fields(rest)
			set := parseSet(fields[2])
			f.mu.Lock()
			seq := 0
			for _, m := range f.messages {
				seq++
				if !set[m.UID] {
					continue
				}
				flags := ""
				if m.Seen {
					flags = `\Seen`
				}
				if strings.Contains(upper, "HEADER.FIELDS") {
					hdr, _, _ := strings.Cut(m.Raw, "\r\n\r\n")
					hdr += "\r\n\r\n"
					fmt.Fprintf(w, "* %d FETCH (UID %d FLAGS (%s) BODY[HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)] {%d}\r\n%s)\r\n", seq, m.UID, flags, len(hdr), hdr)
				} else {
					// Put UID after the literal to exercise the client parser.
					fmt.Fprintf(w, "* %d FETCH (BODY[] {%d}\r\n%s UID %d)\r\n", seq, len(m.Raw), m.Raw, m.UID)
				}
			}
			f.mu.Unlock()
			fmt.Fprintf(w, "%s OK FETCH completed\r\n", tag)
		case strings.HasPrefix(upper, "UID STORE"):
			fields := strings.Fields(rest)
			set := parseSet(fields[2])
			f.mu.Lock()
			for _, m := range f.messages {
				if set[m.UID] && strings.Contains(upper, `\SEEN`) {
					m.Seen = true
				}
			}
			f.mu.Unlock()
			fmt.Fprintf(w, "%s OK STORE completed\r\n", tag)
		case strings.HasPrefix(upper, "LOGOUT"):
			fmt.Fprintf(w, "* BYE\r\n%s OK LOGOUT completed\r\n", tag)
			return
		default:
			fmt.Fprintf(w, "%s BAD unknown command\r\n", tag)
		}
	}
}

func parseSet(s string) map[uint32]bool {
	out := map[uint32]bool{}
	for _, p := range strings.Split(s, ",") {
		n, _ := strconv.Atoi(p)
		out[uint32(n)] = true
	}
	return out
}

// Sent is a message received by the fake SMTP server.
type Sent struct {
	From string
	To   []string
	Data string
}

// FakeSMTP accepts mail without TLS or auth.
type FakeSMTP struct {
	Addr string
	mu   sync.Mutex
	sent []Sent
}

func NewFakeSMTP(t *testing.T) *FakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &FakeSMTP{Addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *FakeSMTP) Sent() []Sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Sent(nil), f.sent...)
}

func (f *FakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	io.WriteString(c, "220 fake ESMTP\r\n")
	var cur Sent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			io.WriteString(c, "250-fake\r\n250 8BITMIME\r\n")
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			cur = Sent{From: angle(line)}
			io.WriteString(c, "250 OK\r\n")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			cur.To = append(cur.To, angle(line))
			io.WriteString(c, "250 OK\r\n")
		case cmd == "DATA":
			io.WriteString(c, "354 go\r\n")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			cur.Data = b.String()
			f.mu.Lock()
			f.sent = append(f.sent, cur)
			f.mu.Unlock()
			io.WriteString(c, "250 queued\r\n")
		case cmd == "QUIT":
			io.WriteString(c, "221 bye\r\n")
			return
		default:
			io.WriteString(c, "250 OK\r\n")
		}
	}
}

func angle(line string) string {
	_, rest, _ := strings.Cut(line, "<")
	addr, _, _ := strings.Cut(rest, ">")
	return addr
}
