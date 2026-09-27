package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// SMTPConfig configures outgoing mail.
type SMTPConfig struct {
	Host               string `yaml:"host"`
	Port               int    `yaml:"port"`
	Username           string `yaml:"username"`
	Password           string `yaml:"password"`
	From               string `yaml:"from"`
	TLS                string `yaml:"tls"` // starttls (default), implicit, none
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	HeloName           string `yaml:"helo_name"`

	// Dial overrides the TCP dialer (e.g. SSRF-guarded for tenants).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error) `yaml:"-"`
}

// Outgoing is a message to send.
type Outgoing struct {
	To         []string
	Cc         []string
	Bcc        []string
	Subject    string
	Text       string
	InReplyTo  string
	References string
	// AutoSubmitted is set as the Auto-Submitted header (RFC 3834), e.g.
	// "auto-replied" or "auto-generated", so other robots don't answer back.
	AutoSubmitted string
}

func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func parseAddrs(list []string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, raw := range list {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		addrs, err := mail.ParseAddressList(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid address %q: %w", raw, err)
		}
		out = append(out, addrs...)
	}
	return out, nil
}

func joinAddrs(a []*mail.Address) string {
	s := make([]string, len(a))
	for i, x := range a {
		s[i] = x.String()
	}
	return strings.Join(s, ", ")
}

// Send delivers m and returns the generated Message-ID.
func Send(ctx context.Context, cfg SMTPConfig, m Outgoing) (string, error) {
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return "", fmt.Errorf("smtp: invalid from address %q: %w", cfg.From, err)
	}
	to, err := parseAddrs(m.To)
	if err != nil {
		return "", err
	}
	cc, err := parseAddrs(m.Cc)
	if err != nil {
		return "", err
	}
	bcc, err := parseAddrs(m.Bcc)
	if err != nil {
		return "", err
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return "", errors.New("smtp: no recipients")
	}

	domain := "localhost"
	if i := strings.LastIndex(from.Address, "@"); i >= 0 {
		domain = from.Address[i+1:]
	}
	rnd := make([]byte, 12)
	_, _ = rand.Read(rnd)
	msgID := "<" + hex.EncodeToString(rnd) + "@" + domain + ">"

	var buf bytes.Buffer
	hdr := func(k, v string) {
		if v != "" {
			buf.WriteString(k + ": " + stripCRLF(v) + "\r\n")
		}
	}
	hdr("From", from.String())
	hdr("To", joinAddrs(to))
	hdr("Cc", joinAddrs(cc))
	hdr("Subject", mime.QEncoding.Encode("utf-8", stripCRLF(m.Subject)))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", msgID)
	hdr("In-Reply-To", m.InReplyTo)
	hdr("References", m.References)
	hdr("Auto-Submitted", m.AutoSubmitted)
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	buf.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&buf)
	_, _ = qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(m.Text, "\r\n", "\n"), "\n", "\r\n")))
	_ = qp.Close()

	var rcpts []string
	for _, lists := range [][]*mail.Address{to, cc, bcc} {
		for _, a := range lists {
			rcpts = append(rcpts, a.Address)
		}
	}
	return msgID, deliver(ctx, cfg, from.Address, rcpts, buf.Bytes())
}

func deliver(ctx context.Context, cfg SMTPConfig, from string, rcpts []string, data []byte) error {
	mode := strings.ToLower(cfg.TLS)
	port := cfg.Port
	if port == 0 {
		switch mode {
		case "implicit":
			port = 465
		case "none":
			port = 25
		default:
			port = 587
		}
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	dial := cfg.Dial
	if dial == nil {
		d := net.Dialer{Timeout: 20 * time.Second}
		dial = d.DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	tlsCfg := &tls.Config{ServerName: cfg.Host, InsecureSkipVerify: cfg.InsecureSkipVerify, MinVersion: tls.VersionTLS12}
	if mode == "implicit" {
		conn = tls.Client(conn, tlsCfg)
	}
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if cfg.HeloName != "" {
		if err := c.Hello(cfg.HeloName); err != nil {
			return err
		}
	}
	if mode == "" || mode == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not support STARTTLS (set tls: none to allow plaintext)")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("smtp rcpt %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
