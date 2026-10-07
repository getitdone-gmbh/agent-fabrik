package mail

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Email is a parsed message.
type Email struct {
	UID         uint32   `json:"uid,omitempty"`
	MessageID   string   `json:"message_id,omitempty"`
	InReplyTo   string   `json:"in_reply_to,omitempty"`
	References  string   `json:"references,omitempty"`
	From        string   `json:"from"`
	ReplyTo     string   `json:"reply_to,omitempty"`
	To          string   `json:"to,omitempty"`
	Cc          string   `json:"cc,omitempty"`
	Subject     string   `json:"subject"`
	Date        string   `json:"date,omitempty"`
	AutoSubmit  string   `json:"auto_submitted,omitempty"`
	Precedence  string   `json:"precedence,omitempty"`
	Text        string   `json:"text"`
	Attachments []string `json:"attachments,omitempty"`
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// charsetReader converts common single-byte charsets to UTF-8.
func charsetReader(charset string, r io.Reader) (io.Reader, error) {
	switch strings.ToLower(charset) {
	case "utf-8", "us-ascii", "ascii", "":
		return r, nil
	case "iso-8859-1", "latin1", "iso-8859-15", "windows-1252", "cp1252":
		b, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		return strings.NewReader(latin1ToUTF8(b)), nil
	}
	return nil, fmt.Errorf("unsupported charset %q", charset)
}

func latin1ToUTF8(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

func decodeHeader(s string) string {
	d, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return s
	}
	return d
}

func parseHeaderBlock(b []byte) map[string]string {
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(append(b, '\r', '\n'))))
	h, _ := tp.ReadMIMEHeader()
	out := map[string]string{}
	for k, v := range h {
		if len(v) > 0 {
			out[k] = decodeHeader(v[0])
		}
	}
	return out
}

// maxText caps the decoded body text kept per message.
const maxText = 256 << 10

// ParseEmail parses a raw RFC 822 message and extracts readable text.
func ParseEmail(raw []byte) (*Email, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	h := msg.Header
	e := &Email{
		MessageID:  strings.TrimSpace(h.Get("Message-Id")),
		InReplyTo:  strings.TrimSpace(h.Get("In-Reply-To")),
		References: strings.TrimSpace(h.Get("References")),
		From:       decodeHeader(h.Get("From")),
		ReplyTo:    decodeHeader(h.Get("Reply-To")),
		To:         decodeHeader(h.Get("To")),
		Cc:         decodeHeader(h.Get("Cc")),
		Subject:    decodeHeader(h.Get("Subject")),
		Date:       h.Get("Date"),
		AutoSubmit: h.Get("Auto-Submitted"),
		Precedence: h.Get("Precedence"),
	}
	var plain, htmlText string
	walkPart(textproto.MIMEHeader(h), msg.Body, &plain, &htmlText, &e.Attachments, 0)
	if strings.TrimSpace(plain) != "" {
		e.Text = plain
	} else {
		e.Text = htmlToText(htmlText)
	}
	e.Text = strings.TrimSpace(e.Text)
	if len(e.Text) > maxText {
		// Batches of parsed mail wait in memory while their runs go; no
		// model reads megabytes of body text anyway.
		e.Text = e.Text[:maxText] + "\n…(truncated)"
	}
	return e, nil
}

func walkPart(h textproto.MIMEHeader, body io.Reader, plain, htmlText *string, atts *[]string, depth int) {
	if depth > 10 {
		return
	}
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = "text/plain; charset=us-ascii"
	}
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		mediaType = "text/plain"
	}
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))

	if strings.HasPrefix(mediaType, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				return
			}
			walkPart(p.Header, p, plain, htmlText, atts, depth+1)
		}
	}

	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	if disp == "attachment" || (name != "" && !strings.HasPrefix(mediaType, "text/")) {
		*atts = append(*atts, decodeHeader(name)+" ("+mediaType+")")
		return
	}
	if mediaType == "message/rfc822" {
		*atts = append(*atts, "forwarded message (message/rfc822)")
		return
	}
	if !strings.HasPrefix(mediaType, "text/") {
		return
	}
	data, err := io.ReadAll(io.LimitReader(decodeTransfer(h.Get("Content-Transfer-Encoding"), body), 5<<20))
	if err != nil && len(data) == 0 {
		return
	}
	text := toUTF8(data, params["charset"])
	switch mediaType {
	case "text/plain":
		*plain += text
	case "text/html":
		*htmlText += text
	}
}

func decodeTransfer(enc string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "quoted-printable":
		return quotedprintable.NewReader(r)
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, &newlineStripper{r: r})
	}
	return r
}

// newlineStripper removes CR/LF so base64 decoding tolerates line breaks.
type newlineStripper struct{ r io.Reader }

func (n *newlineStripper) Read(p []byte) (int, error) {
	for {
		c, err := n.r.Read(p)
		j := 0
		for _, b := range p[:c] {
			if b != '\r' && b != '\n' && b != ' ' && b != '\t' {
				p[j] = b
				j++
			}
		}
		if j > 0 || err != nil {
			return j, err
		}
	}
}

func toUTF8(b []byte, charset string) string {
	cs := strings.ToLower(charset)
	if cs == "" || cs == "utf-8" || cs == "us-ascii" || utf8.Valid(b) && cs != "iso-8859-1" && cs != "windows-1252" {
		return string(b)
	}
	if r, err := charsetReader(cs, bytes.NewReader(b)); err == nil {
		out, _ := io.ReadAll(r)
		return string(out)
	}
	return strings.ToValidUTF8(string(b), "�")
}

var (
	reScript = regexp.MustCompile(`(?is)<(script|style|head)[^>]*>.*?</(script|style|head)>`)
	reBreak  = regexp.MustCompile(`(?i)<(br|/p|/div|/tr|/li|/h[1-6])[^>]*>`)
	reTag    = regexp.MustCompile(`<[^>]+>`)
	reBlank  = regexp.MustCompile(`\n[ \t]*\n[\s]*`)
	reSpaces = regexp.MustCompile(`[ \t]+`)
)

func htmlToText(s string) string {
	s = reScript.ReplaceAllString(s, "")
	s = reBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	s = reBlank.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// IsAutomated reports whether the message looks machine-generated (bounces,
// auto-replies, mailing lists) so agents don't reply and create mail loops.
func (e *Email) IsAutomated() bool {
	if e.AutoSubmit != "" && !strings.EqualFold(e.AutoSubmit, "no") {
		return true
	}
	switch strings.ToLower(e.Precedence) {
	case "bulk", "junk", "list", "auto_reply":
		return true
	}
	from := strings.ToLower(e.From)
	for _, p := range []string{"mailer-daemon", "postmaster@", "no-reply", "noreply", "do-not-reply", "donotreply"} {
		if strings.Contains(from, p) {
			return true
		}
	}
	return false
}
