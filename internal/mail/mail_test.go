package mail_test

import (
	"context"
	"strings"
	"testing"

	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/mail"
	"github.com/getitdone-gmbh/itdone-agent-runtime/internal/testutil"
)

const multipartMsg = `From: =?utf-8?q?J=C3=BCrgen?= <juergen@example.com>
To: support@example.com
Subject: =?utf-8?b?SGlsZmUgYmVpIGRlciBBbm1lbGR1bmc=?=
Message-ID: <abc@example.com>
Date: Mon, 21 Sep 2026 10:00:00 +0200
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary="b1"

--b1
Content-Type: multipart/alternative; boundary="b2"

--b2
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Hallo, ich kann mich nicht anmelden. Gr=C3=BC=C3=9Fe
--b2
Content-Type: text/html; charset=utf-8

<p>Hallo</p>
--b2--
--b1
Content-Type: application/pdf; name="invoice.pdf"
Content-Disposition: attachment; filename="invoice.pdf"
Content-Transfer-Encoding: base64

JVBERi0xLjQK
--b1--
`

func TestParseEmail(t *testing.T) {
	e, err := mail.ParseEmail([]byte(strings.ReplaceAll(multipartMsg, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if e.Subject != "Hilfe bei der Anmeldung" || !strings.Contains(e.From, "Jürgen") {
		t.Errorf("headers: %q %q", e.Subject, e.From)
	}
	if e.Text != "Hallo, ich kann mich nicht anmelden. Grüße" {
		t.Errorf("text: %q", e.Text)
	}
	if len(e.Attachments) != 1 || !strings.HasPrefix(e.Attachments[0], "invoice.pdf") {
		t.Errorf("attachments: %v", e.Attachments)
	}
	if e.IsAutomated() {
		t.Error("should not be automated")
	}
}

func TestParseHTMLOnlyAndLatin1(t *testing.T) {
	raw := "From: a@b.c\r\nSubject: x\r\nContent-Type: text/html; charset=iso-8859-1\r\n\r\n<html><head><style>p{}</style></head><body><p>Gr\xfc\xdfe</p><br>Zeile&nbsp;2</body></html>"
	e, err := mail.ParseEmail([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.Text, "Grüße") || !strings.Contains(e.Text, "Zeile") || strings.Contains(e.Text, "<p>") {
		t.Errorf("text: %q", e.Text)
	}
}

func TestAutomatedDetection(t *testing.T) {
	e, _ := mail.ParseEmail([]byte("From: MAILER-DAEMON@x.org\r\nSubject: fail\r\n\r\nbounce"))
	if !e.IsAutomated() {
		t.Error("bounce not detected")
	}
	e, _ = mail.ParseEmail([]byte("From: bot@x.org\r\nAuto-Submitted: auto-replied\r\nSubject: ooo\r\n\r\naway"))
	if !e.IsAutomated() {
		t.Error("auto-reply not detected")
	}
}

func TestIMAPClient(t *testing.T) {
	srv := testutil.NewFakeIMAP(t, "bot@example.com", `p"ä\ss`) // forces literal + escaping
	uid1 := srv.Add(multipartMsg)
	uid2 := srv.Add("From: other@example.com\nSubject: Second\nMessage-ID: <2@x>\n\nbody two\n")

	host, port := splitAddr(srv.Addr)
	c, err := mail.DialIMAP(context.Background(), mail.IMAPConfig{Host: host, Port: port, TLS: "none", Username: "bot@example.com", Password: `p"ä\ss`})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	n, err := c.Select("INBOX")
	if err != nil || n != 2 {
		t.Fatalf("select: %d %v", n, err)
	}
	uids, err := c.Search(mail.SearchCriteria{Unseen: true})
	if err != nil || len(uids) != 2 {
		t.Fatalf("search: %v %v", uids, err)
	}
	sums, err := c.FetchSummaries(uids)
	if err != nil || len(sums) != 2 || sums[1].Subject != "Second" || sums[0].Subject != "Hilfe bei der Anmeldung" {
		t.Fatalf("summaries: %+v %v", sums, err)
	}
	raw, err := c.FetchRaw(uid2)
	if err != nil || !strings.Contains(string(raw), "body two") {
		t.Fatalf("fetch: %q %v", raw, err)
	}
	if err := c.AddFlags([]uint32{uid1}, `\Seen`); err != nil {
		t.Fatal(err)
	}
	if !srv.Seen(uid1) || srv.Seen(uid2) {
		t.Error("flags not applied")
	}
	uids, _ = c.Search(mail.SearchCriteria{Unseen: true})
	if len(uids) != 1 || uids[0] != uid2 {
		t.Errorf("unseen after store: %v", uids)
	}
}

func TestIMAPLoginFailure(t *testing.T) {
	srv := testutil.NewFakeIMAP(t, "u", "p")
	host, port := splitAddr(srv.Addr)
	_, err := mail.DialIMAP(context.Background(), mail.IMAPConfig{Host: host, Port: port, TLS: "none", Username: "u", Password: "wrong"})
	if err == nil || !strings.Contains(err.Error(), "login") {
		t.Fatalf("expected login error, got %v", err)
	}
}

func TestSMTPSend(t *testing.T) {
	srv := testutil.NewFakeSMTP(t)
	host, port := splitAddr(srv.Addr)
	id, err := mail.Send(context.Background(), mail.SMTPConfig{Host: host, Port: port, TLS: "none", From: "Bot <bot@example.com>"}, mail.Outgoing{
		To: []string{"a@example.com, B <b@example.com>"}, Bcc: []string{"hidden@example.com"},
		Subject: "Grüße\r\nBcc: evil@x.org", Text: "Hallo\nWelt – ✓", InReplyTo: "<abc@example.com>", AutoSubmitted: "auto-replied",
	})
	if err != nil {
		t.Fatal(err)
	}
	sent := srv.Sent()
	if len(sent) != 1 || len(sent[0].To) != 3 || sent[0].From != "bot@example.com" {
		t.Fatalf("sent: %+v", sent)
	}
	d := sent[0].Data
	for _, want := range []string{"Message-ID: " + id, "In-Reply-To: <abc@example.com>", "Auto-Submitted: auto-replied", "Subject: =?utf-8?q?Gr=C3=BC=C3=9Fe"} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in\n%s", want, d)
		}
	}
	if strings.Contains(d, "\r\nBcc:") || strings.Contains(d, "hidden@example.com") {
		t.Errorf("header injection or bcc leak: %q", d)
	}
	parsed, err := mail.ParseEmail([]byte(d))
	if err != nil || parsed.Text != "Hallo\r\nWelt – ✓" && parsed.Text != "Hallo\nWelt – ✓" {
		t.Errorf("round trip: %q %v", parsed.Text, err)
	}
}

func TestSMTPRejectsBadAddress(t *testing.T) {
	_, err := mail.Send(context.Background(), mail.SMTPConfig{Host: "127.0.0.1", Port: 1, TLS: "none", From: "bot@example.com"}, mail.Outgoing{To: []string{"not an address"}})
	if err == nil {
		t.Fatal("expected error")
	}
}
