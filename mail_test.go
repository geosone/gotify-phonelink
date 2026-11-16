package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"
)

// fakeRelay is the mail server the gate relays to.
type fakeRelay struct {
	mu   sync.Mutex
	got  []string // "to|body"
	addr string
}

func (f *fakeRelay) NewSession(*gosmtp.Conn) (gosmtp.Session, error) { return &relaySess{f: f}, nil }

type relaySess struct {
	f  *fakeRelay
	to []string
}

func (s *relaySess) Mail(string, *gosmtp.MailOptions) error     { return nil }
func (s *relaySess) Rcpt(t string, _ *gosmtp.RcptOptions) error { s.to = append(s.to, t); return nil }
func (s *relaySess) Reset()                                     {}
func (s *relaySess) Logout() error                              { return nil }
func (s *relaySess) Data(r io.Reader) error {
	b, _ := io.ReadAll(r)
	s.f.mu.Lock()
	s.f.got = append(s.f.got, strings.Join(s.to, ",")+"|"+string(b))
	s.f.mu.Unlock()
	return nil
}

func (f *fakeRelay) mails() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func startRelay(t *testing.T) *fakeRelay {
	f := &fakeRelay{}
	s := gosmtp.NewServer(f)
	s.AllowInsecureAuth = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	go s.Serve(ln)
	t.Cleanup(func() { s.Close() })
	return f
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := ln.Addr().String()
	ln.Close()
	return a
}

func synapseMail(to, text string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	var lines []string
	for len(b64) > 76 {
		lines = append(lines, b64[:76])
		b64 = b64[76:]
	}
	lines = append(lines, b64)
	return fmt.Sprintf("From: Chat <noreply@example.org>\r\nTo: %s\r\nSubject: Validate\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: multipart/alternative; boundary=\"b1\"\r\n\r\n--b1\r\nContent-Type: text/plain; charset=\"utf-8\"\r\n"+
		"Content-Transfer-Encoding: base64\r\n\r\n%s\r\n--b1--\r\n", to, strings.Join(lines, "\r\n"))
}

func TestMailGate(t *testing.T) {
	e := newEnv(t, false)
	relay := startRelay(t)
	listen := freePort(t)
	e.cfg.Mail = MailConfig{Listen: listen, AllowedClients: []string{"127.0.0.1"}, Relay: relay.addr, Sender: "synapse"}
	if err := e.admin.ValidateAndSetConfig(e.cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.h.reconcileMail(nil) })
	time.Sleep(100 * time.Millisecond)

	send := func(to, text string) error {
		return smtp.SendMail(listen, nil, "noreply@example.org", []string{to}, []byte(synapseMail(to, text)))
	}
	// a notification passes at once
	if err := send("alice@example.org", "You have 1 unread message"); err != nil {
		t.Fatal(err)
	}
	if m := relay.mails(); len(m) != 1 || !strings.HasPrefix(m[0], "alice@example.org|") {
		t.Fatalf("relayed: %v", m)
	}
	// password reset passes too
	if err := send("alice@example.org", "https://matrix.example.org/_synapse/client/password_reset/email/submit_token?token=x"); err != nil {
		t.Fatal(err)
	}
	before := len(e.msgs[1].all())
	// a registration mail waits
	if err := send("eve@example.org", "Click https://matrix.example.org/_synapse/client/registration/email/submit_token?token=abc&sid=1 to validate"); err != nil {
		t.Fatal(err)
	}
	if n := len(relay.mails()); n != 2 {
		t.Fatalf("registration mail relayed before approval (%d mails)", n)
	}
	m := e.msgs[1].all()
	if len(m) != before+1 || !strings.Contains(m[len(m)-1].Message, "eve@example.org") {
		t.Fatalf("moderator not notified: %+v", m)
	}
	u, _ := url.Parse(m[len(m)-1].Extras["client::notification"].(map[string]interface{})["click"].(map[string]interface{})["url"].(string))
	id := strings.TrimPrefix(u.Path, "/plugin/admin/custom/tok/approve/")
	v, err := e.h.decide(context.Background(), id, "admin", u.Query().Get("k"), true)
	if err != nil || v.Decided != "approved" || v.Error != "" {
		t.Fatalf("decide: %+v %v", v, err)
	}
	got := relay.mails()
	if len(got) != 3 || !strings.HasPrefix(got[2], "eve@example.org|") || !strings.Contains(got[2], "Content-Transfer-Encoding: base64") {
		t.Fatalf("approved mail not relayed unchanged: %v", got)
	}
}

func TestMailGateRefusesClients(t *testing.T) {
	e := newEnv(t, false)
	relay := startRelay(t)
	listen := freePort(t)
	e.cfg.Mail = MailConfig{Listen: listen, AllowedClients: []string{"192.0.2.1"}, Relay: relay.addr}
	if err := e.admin.ValidateAndSetConfig(e.cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.h.reconcileMail(nil) })
	time.Sleep(100 * time.Millisecond)
	if err := smtp.SendMail(listen, nil, "a@example.org", []string{"b@example.org"}, []byte("Subject: x\r\n\r\nhi\r\n")); err == nil {
		t.Fatal("mail from a client that is not allowed accepted")
	}
	if len(relay.mails()) != 0 {
		t.Fatal("relayed")
	}
}

func TestMailText(t *testing.T) {
	raw := synapseMail("x@example.org", "see /registration/email/submit_token here")
	if !mailNeedsApproval([]byte(raw), defaultHoldPatterns) {
		t.Fatal("base64 registration mail not detected")
	}
	qp := "Content-Type: text/plain\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nhttps://x/_synapse/client/registr=\r\nation/email/submit_token?t=3D1\r\n"
	if !mailNeedsApproval([]byte(qp), defaultHoldPatterns) {
		t.Fatal("quoted-printable registration mail not detected")
	}
}
