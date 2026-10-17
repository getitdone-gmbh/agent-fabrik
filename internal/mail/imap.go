// Package mail contains a minimal IMAP4rev1 client, an SMTP sender and a
// MIME message parser — just enough for agents to read and answer email
// without pulling in heavy dependencies.
package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// IMAPConfig configures an IMAP connection.
type IMAPConfig struct {
	Host               string `yaml:"host"`
	Port               int    `yaml:"port"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	TLS                string `yaml:"tls"` // implicit (default), starttls, none
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	Mailbox            string `yaml:"mailbox"` // default INBOX
	// MaxMessageBytes caps what one command may read from the server, the
	// sum of all its literals (default 50 MB). Tenants get a smaller cap.
	MaxMessageBytes int `yaml:"max_message_bytes"`

	// Dial overrides the TCP dialer (e.g. SSRF-guarded for tenants).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error) `yaml:"-"`
}

func (c IMAPConfig) addr() string {
	port := c.Port
	if port == 0 {
		if strings.EqualFold(c.TLS, "implicit") || c.TLS == "" {
			port = 993
		} else {
			port = 143
		}
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

// IMAPClient is a single, non-concurrent IMAP session.
type IMAPClient struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	tag  int
	// limit is the byte budget of one command, left what remains of it.
	limit, left int64
	// textLeft is what remains of one command's budget for response lines
	// (everything but literals), so a server can't stream untagged lines
	// until the process runs out of memory.
	textLeft int64
}

// response is one server response with literals extracted.
type response struct {
	text     string
	literals [][]byte
}

const imapTimeout = 60 * time.Second

// DialIMAP connects and logs in.
func DialIMAP(ctx context.Context, cfg IMAPConfig) (*IMAPClient, error) {
	dial := cfg.Dial
	if dial == nil {
		d := net.Dialer{Timeout: 20 * time.Second}
		dial = d.DialContext
	}
	conn, err := dial(ctx, "tcp", cfg.addr())
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: cfg.InsecureSkipVerify, MinVersion: tls.VersionTLS12}
	mode := strings.ToLower(cfg.TLS)
	if mode == "" || mode == "implicit" {
		conn = tls.Client(conn, tlsCfg)
	}
	limit := int64(cfg.MaxMessageBytes)
	if limit <= 0 {
		limit = maxLiteral
	}
	c := &IMAPClient{conn: conn, r: bufio.NewReader(conn), w: bufio.NewWriter(conn), limit: limit, left: limit, textLeft: maxResponseText}
	c.deadline()
	greeting, err := c.readLine()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("imap greeting: %w", err)
	}
	if !strings.HasPrefix(greeting, "* OK") && !strings.HasPrefix(greeting, "* PREAUTH") {
		conn.Close()
		return nil, fmt.Errorf("imap greeting: %s", greeting)
	}
	if mode == "starttls" {
		if _, err := c.exec("STARTTLS"); err != nil {
			conn.Close()
			return nil, err
		}
		c.conn = tls.Client(conn, tlsCfg)
		c.r = bufio.NewReader(c.conn)
		c.w = bufio.NewWriter(c.conn)
	}
	if !strings.HasPrefix(greeting, "* PREAUTH") {
		if _, err := c.exec("LOGIN ", istr(cfg.Username), " ", istr(cfg.Password)); err != nil {
			c.conn.Close()
			return nil, fmt.Errorf("imap login: %w", err)
		}
	}
	return c, nil
}

// istr is an IMAP string argument, sent quoted or as a literal.
type istr string

func (c *IMAPClient) deadline() { _ = c.conn.SetDeadline(time.Now().Add(imapTimeout)) }

// maxLine caps one response line; ReadString alone would buffer a line
// without end until the process runs out of memory.
const maxLine = 1 << 20

func (c *IMAPClient) readLine() (string, error) {
	var line []byte
	for {
		chunk, err := c.r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLine {
			return "", fmt.Errorf("imap line longer than %d bytes", maxLine)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(line), "\r\n"), nil
	}
}

var literalRe = regexp.MustCompile(`\{(\d+)\+?\}$`)

const maxLiteral = 50 << 20

// maxResponseText caps the response lines of one command outside literals.
const maxResponseText = 8 << 20

func (c *IMAPClient) readResponse() (response, error) {
	var resp response
	var text strings.Builder
	for {
		line, err := c.readLine()
		if err != nil {
			return resp, err
		}
		c.textLeft -= int64(len(line))
		if c.textLeft < 0 {
			return resp, fmt.Errorf("imap response text larger than %d bytes", maxResponseText)
		}
		text.WriteString(line)
		m := literalRe.FindStringSubmatch(line)
		if m == nil {
			break
		}
		n, _ := strconv.Atoi(m[1])
		if int64(n) > c.left {
			return resp, fmt.Errorf("imap response larger than %d bytes", c.limit)
		}
		c.left -= int64(n)
		buf := make([]byte, n)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return resp, err
		}
		resp.literals = append(resp.literals, buf)
	}
	resp.text = text.String()
	return resp, nil
}

func needsLiteral(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return true
		}
	}
	return false
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// exec sends one tagged command made of raw string parts and istr arguments,
// and returns the untagged responses once the command completes with OK.
func (c *IMAPClient) exec(parts ...any) ([]response, error) {
	c.tag++
	tag := fmt.Sprintf("A%04d", c.tag)
	c.deadline()
	c.left = c.limit
	c.textLeft = maxResponseText
	var untagged []response
	c.w.WriteString(tag + " ")
	for _, p := range parts {
		switch v := p.(type) {
		case string:
			c.w.WriteString(v)
		case istr:
			s := string(v)
			if !needsLiteral(s) {
				c.w.WriteString(quote(s))
				continue
			}
			// Synchronizing literal: announce size, wait for continuation.
			fmt.Fprintf(c.w, "{%d}\r\n", len(s))
			if err := c.w.Flush(); err != nil {
				return nil, err
			}
			for {
				r, err := c.readResponse()
				if err != nil {
					return nil, err
				}
				if strings.HasPrefix(r.text, "+") {
					break
				}
				if strings.HasPrefix(r.text, tag+" ") {
					return nil, errors.New("imap: " + strings.TrimPrefix(r.text, tag+" "))
				}
				untagged = append(untagged, r)
			}
			c.w.WriteString(s)
		default:
			return nil, fmt.Errorf("imap: bad command part %T", p)
		}
	}
	c.w.WriteString("\r\n")
	if err := c.w.Flush(); err != nil {
		return nil, err
	}
	for {
		r, err := c.readResponse()
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(r.text, tag+" ") {
			status := strings.TrimPrefix(r.text, tag+" ")
			if strings.HasPrefix(status, "OK") {
				return untagged, nil
			}
			return untagged, errors.New("imap: " + status)
		}
		if strings.HasPrefix(r.text, "+") {
			continue // unexpected continuation; ignore
		}
		untagged = append(untagged, r)
	}
}

// Select opens a mailbox read-write and returns the message count.
func (c *IMAPClient) Select(mailbox string) (int, error) {
	if mailbox == "" {
		mailbox = "INBOX"
	}
	resps, err := c.exec("SELECT ", istr(mailbox))
	if err != nil {
		return 0, err
	}
	exists := 0
	for _, r := range resps {
		f := strings.Fields(r.text)
		if len(f) == 3 && f[0] == "*" && f[2] == "EXISTS" {
			exists, _ = strconv.Atoi(f[1])
		}
	}
	return exists, nil
}

// SearchCriteria filters messages. Empty fields are ignored.
type SearchCriteria struct {
	Unseen  bool
	From    string
	Subject string
	Text    string
	Since   time.Time
}

// Search returns UIDs matching the criteria (ascending).
func (c *IMAPClient) Search(sc SearchCriteria) ([]uint32, error) {
	parts := []any{"UID SEARCH"}
	if needsLiteral(sc.From + sc.Subject + sc.Text) {
		parts = append(parts, " CHARSET UTF-8")
	}
	n := 0
	add := func(key string, val string) {
		parts = append(parts, " "+key+" ", istr(val))
		n++
	}
	if sc.Unseen {
		parts = append(parts, " UNSEEN")
		n++
	}
	if sc.From != "" {
		add("FROM", sc.From)
	}
	if sc.Subject != "" {
		add("SUBJECT", sc.Subject)
	}
	if sc.Text != "" {
		add("TEXT", sc.Text)
	}
	if !sc.Since.IsZero() {
		parts = append(parts, " SINCE "+sc.Since.Format("2-Jan-2006"))
		n++
	}
	if n == 0 {
		parts = append(parts, " ALL")
	}
	resps, err := c.exec(parts...)
	if err != nil {
		return nil, err
	}
	var uids []uint32
	for _, r := range resps {
		if !strings.HasPrefix(r.text, "* SEARCH") {
			continue
		}
		for _, f := range strings.Fields(strings.TrimPrefix(r.text, "* SEARCH")) {
			if u, err := strconv.ParseUint(f, 10, 32); err == nil {
				uids = append(uids, uint32(u))
			}
		}
	}
	return uids, nil
}

// Summary is a lightweight view of a message.
type Summary struct {
	UID       uint32   `json:"uid"`
	Flags     []string `json:"flags,omitempty"`
	From      string   `json:"from"`
	Subject   string   `json:"subject"`
	Date      string   `json:"date,omitempty"`
	MessageID string   `json:"message_id,omitempty"`
}

var (
	uidRe   = regexp.MustCompile(`UID (\d+)`)
	flagsRe = regexp.MustCompile(`FLAGS \(([^)]*)\)`)
)

func uidSet(uids []uint32) string {
	s := make([]string, len(uids))
	for i, u := range uids {
		s[i] = strconv.FormatUint(uint64(u), 10)
	}
	return strings.Join(s, ",")
}

// FetchSummaries returns header summaries for the given UIDs.
func (c *IMAPClient) FetchSummaries(uids []uint32) ([]Summary, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	resps, err := c.exec("UID FETCH " + uidSet(uids) + " (UID FLAGS BODY.PEEK[HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)])")
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, r := range resps {
		if !strings.Contains(r.text, " FETCH ") {
			continue
		}
		m := uidRe.FindStringSubmatch(r.text)
		if m == nil {
			continue
		}
		u, _ := strconv.ParseUint(m[1], 10, 32)
		s := Summary{UID: uint32(u)}
		if fm := flagsRe.FindStringSubmatch(r.text); fm != nil {
			s.Flags = strings.Fields(fm[1])
		}
		if len(r.literals) > 0 {
			h := parseHeaderBlock(r.literals[0])
			s.From, s.Subject, s.Date, s.MessageID = h["From"], h["Subject"], h["Date"], h["Message-Id"]
		}
		out = append(out, s)
	}
	return out, nil
}

// fetchSlots bounds how many full messages are held and decoded at once
// per process. A raw message and its decoded parts are the largest buffers
// agentd holds, and every tenant's triggers and tools may fetch in parallel.
var fetchSlots = make(chan struct{}, 4)

// FetchEmail fetches and parses one message without setting \Seen. A
// message the server returned but that does not parse is reported as
// ErrUnparseable, so callers can skip it instead of failing the batch.
func (c *IMAPClient) FetchEmail(uid uint32) (*Email, error) {
	fetchSlots <- struct{}{}
	defer func() { <-fetchSlots }()
	raw, err := c.FetchRaw(uid)
	if err != nil {
		return nil, err
	}
	e, err := ParseEmail(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnparseable, err)
	}
	e.UID = uid
	return e, nil
}

// ErrUnparseable marks a fetched message that is not valid RFC 822.
var ErrUnparseable = errors.New("unparseable email")

// FetchRaw returns the full RFC 822 message without setting \Seen.
func (c *IMAPClient) FetchRaw(uid uint32) ([]byte, error) {
	resps, err := c.exec("UID FETCH " + strconv.FormatUint(uint64(uid), 10) + " (UID BODY.PEEK[])")
	if err != nil {
		return nil, err
	}
	for _, r := range resps {
		if strings.Contains(r.text, " FETCH ") && len(r.literals) > 0 {
			return r.literals[0], nil
		}
	}
	return nil, fmt.Errorf("imap: message uid %d not found", uid)
}

// AddFlags adds flags (e.g. `\Seen`) to messages.
func (c *IMAPClient) AddFlags(uids []uint32, flags ...string) error {
	if len(uids) == 0 {
		return nil
	}
	_, err := c.exec("UID STORE " + uidSet(uids) + " +FLAGS.SILENT (" + strings.Join(flags, " ") + ")")
	return err
}

// Close logs out and closes the connection.
func (c *IMAPClient) Close() error {
	_, _ = c.exec("LOGOUT")
	return c.conn.Close()
}
