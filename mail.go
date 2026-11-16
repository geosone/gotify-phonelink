package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"time"

	gosmtp "github.com/emersion/go-smtp"
)

// Mail gate: Synapse (or any trusted client) sends its mail here instead of
// to the mail server. Mails that contain one of mail.hold_patterns (by
// default the link of a registration e-mail) wait for a moderator; all other
// mail is relayed at once.

// MailConfig is the SMTP gate.
type MailConfig struct {
	// Address to listen on, e.g. ":2525" (empty = no gate)
	Listen string `yaml:"listen"`
	// Client IPs allowed to send (required)
	AllowedClients []string `yaml:"allowed_clients"`
	// Certificate files for STARTTLS (read on every handshake, so renewals work)
	TLSCert string `yaml:"tls_cert"`
	TLSKey  string `yaml:"tls_key"`
	// Mail server everything is relayed to, host:port
	Relay         string `yaml:"relay"`
	RelayStartTLS bool   `yaml:"relay_starttls"`
	RelayUsername string `yaml:"relay_username"`
	RelayPassword string `yaml:"relay_password"`
	// Mails whose text contains one of these wait for approval
	HoldPatterns []string `yaml:"hold_patterns"`
	// Name shown to moderators
	Sender          string `yaml:"sender"`
	ApprovalTimeout string `yaml:"approval_timeout"`
}

const maxMailSize = 10 << 20

var defaultHoldPatterns = []string{"/registration/email/submit_token"}

func (m *MailConfig) enabled() bool { return m.Listen != "" }

func (m *MailConfig) holdPatterns() []string {
	if len(m.HoldPatterns) == 0 {
		return defaultHoldPatterns
	}
	return m.HoldPatterns
}

func (m *MailConfig) senderName() string {
	if m.Sender == "" {
		return "mail"
	}
	return m.Sender
}

func (m *MailConfig) approvalTimeout() time.Duration {
	d, _ := parseDuration(m.ApprovalTimeout)
	if d == 0 {
		return defaultApprovalTimeout
	}
	return d
}

func (m *MailConfig) validate() error {
	if !m.enabled() {
		return nil
	}
	if _, _, err := net.SplitHostPort(m.Listen); err != nil {
		return fmt.Errorf("mail.listen: %w", err)
	}
	if len(m.AllowedClients) == 0 {
		return errors.New("mail.allowed_clients is required")
	}
	for _, c := range m.AllowedClients {
		if net.ParseIP(c) == nil {
			return fmt.Errorf("mail.allowed_clients: %q is not an IP address", c)
		}
	}
	if _, _, err := net.SplitHostPort(m.Relay); err != nil {
		return fmt.Errorf("mail.relay: %w", err)
	}
	if (m.TLSCert == "") != (m.TLSKey == "") {
		return errors.New("mail.tls_cert and mail.tls_key go together")
	}
	if _, err := parseDuration(m.ApprovalTimeout); err != nil {
		return fmt.Errorf("mail.approval_timeout: %w", err)
	}
	return nil
}

// mailGate is the running SMTP listener.
type mailGate struct {
	h      *hub
	cfg    MailConfig
	server *gosmtp.Server
	done   chan struct{}
}

var mailMu sync.Mutex

// reconcileMail starts, restarts or stops the gate for the control config.
func (h *hub) reconcileMail(cfg *Config) {
	mailMu.Lock()
	defer mailMu.Unlock()
	var want *MailConfig
	if cfg != nil && cfg.Mail.enabled() {
		want = &cfg.Mail
	}
	if h.mail != nil && want != nil && fmt.Sprint(h.mail.cfg) == fmt.Sprint(*want) {
		return
	}
	if h.mail != nil {
		_ = h.mail.server.Close()
		<-h.mail.done
		h.mail = nil
		log.Printf("gotify-phonelink: mail gate stopped")
	}
	if want == nil {
		return
	}
	g := &mailGate{h: h, cfg: *want, done: make(chan struct{})}
	s := gosmtp.NewServer(g)
	s.Addr = want.Listen
	s.Domain = "phonelink"
	s.MaxMessageBytes = maxMailSize
	s.MaxRecipients = 50
	s.ReadTimeout = 2 * time.Minute
	s.WriteTimeout = 2 * time.Minute
	if want.TLSCert != "" {
		certFile, keyFile := want.TLSCert, want.TLSKey
		s.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				c, err := tls.LoadX509KeyPair(certFile, keyFile)
				return &c, err
			},
		}
	}
	ln, err := net.Listen("tcp", want.Listen)
	if err != nil {
		log.Printf("gotify-phonelink: mail gate: %v", err)
		return
	}
	g.server = s
	h.mail = g
	go func() {
		defer close(g.done)
		if err := s.Serve(ln); err != nil && !errors.Is(err, gosmtp.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Printf("gotify-phonelink: mail gate: %v", err)
		}
	}()
	log.Printf("gotify-phonelink: mail gate listening on %s, relaying to %s", want.Listen, want.Relay)
}

// NewSession implements gosmtp.Backend.
func (g *mailGate) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	host, _, _ := net.SplitHostPort(c.Conn().RemoteAddr().String())
	ip := net.ParseIP(host)
	for _, a := range g.cfg.AllowedClients {
		if ip != nil && ip.Equal(net.ParseIP(a)) {
			return &mailSession{g: g}, nil
		}
	}
	log.Printf("gotify-phonelink: mail gate refused client %s", host)
	return nil, &gosmtp.SMTPError{Code: 554, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "client not allowed"}
}

type mailSession struct {
	g    *mailGate
	from string
	to   []string
}

func (s *mailSession) Mail(from string, _ *gosmtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *mailSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *mailSession) Reset() { s.from, s.to = "", nil }

func (s *mailSession) Logout() error { return nil }

func (s *mailSession) Data(r io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(r, maxMailSize+1))
	if err != nil {
		return err
	}
	if len(raw) > maxMailSize {
		return &gosmtp.SMTPError{Code: 552, EnhancedCode: gosmtp.EnhancedCode{5, 3, 4}, Message: "message too big"}
	}
	return s.g.h.acceptMail(context.Background(), &s.g.cfg, s.from, append([]string(nil), s.to...), raw)
}

// acceptMail relays a mail or holds it for approval.
func (h *hub) acceptMail(ctx context.Context, mc *MailConfig, from string, to []string, raw []byte) error {
	held := mailNeedsApproval(raw, mc.holdPatterns())
	if !held {
		if err := relayMail(ctx, mc, from, to, raw); err != nil {
			log.Printf("gotify-phonelink: relaying mail failed: %v", err)
			return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 4, 0}, Message: "relay failed, try again later"}
		}
		return nil
	}
	ctrl, cfg := h.control()
	if cfg == nil {
		return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "not configured"}
	}
	relayCfg := *mc
	for _, rcpt := range to {
		rcpt := rcpt
		_, err := h.hold(cfg, ctrl, "mail", mc.senderName(), rcpt, mc.approvalTimeout(), func(ctx context.Context) error {
			c, cancel := context.WithTimeout(ctx, time.Minute)
			defer cancel()
			return relayMail(c, &relayCfg, from, []string{rcpt}, raw)
		})
		if err != nil {
			log.Printf("gotify-phonelink: holding mail to %s failed: %v", maskAddr("mail", rcpt), err)
			return &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "cannot hold the message"}
		}
	}
	return nil
}

// mailNeedsApproval tells whether the decoded text of a mail contains a
// hold pattern.
func mailNeedsApproval(raw []byte, patterns []string) bool {
	text := mailText(raw)
	for _, p := range patterns {
		if p != "" && strings.Contains(text, p) {
			return true
		}
	}
	return false
}

// mailText returns the decoded text parts of a mail (and the raw mail, so
// nothing is missed when it cannot be parsed).
func mailText(raw []byte) string {
	var b strings.Builder
	b.Write(raw)
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return b.String()
	}
	collectText(&b, m.Header.Get("Content-Type"), m.Header.Get("Content-Transfer-Encoding"), m.Body, 0)
	return b.String()
}

func collectText(b *strings.Builder, ctype, cte string, body io.Reader, depth int) {
	if depth > 5 {
		return
	}
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt = "text/plain"
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			p, err := mr.NextRawPart()
			if err != nil {
				return
			}
			collectText(b, p.Header.Get("Content-Type"), p.Header.Get("Content-Transfer-Encoding"), p, depth+1)
		}
	}
	if !strings.HasPrefix(mt, "text/") {
		return
	}
	var r io.Reader = body
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		r = base64.NewDecoder(base64.StdEncoding, &stripNewlines{r: body})
	case "quoted-printable":
		r = quotedprintable.NewReader(body)
	}
	data, _ := io.ReadAll(io.LimitReader(r, maxMailSize))
	b.WriteByte('\n')
	b.Write(data)
}

// stripNewlines drops CR/LF so base64 lines decode as one stream.
type stripNewlines struct{ r io.Reader }

func (s *stripNewlines) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	j := 0
	for i := 0; i < n; i++ {
		if p[i] != '\r' && p[i] != '\n' {
			p[j] = p[i]
			j++
		}
	}
	return j, err
}

// relayMail passes a mail on to the mail server unchanged.
func relayMail(ctx context.Context, mc *MailConfig, from string, to []string, raw []byte) error {
	host, _, _ := net.SplitHostPort(mc.Relay)
	d := net.Dialer{Timeout: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", mc.Relay)
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Hello("phonelink"); err != nil {
		return err
	}
	if mc.RelayStartTLS {
		if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if mc.RelayUsername != "" {
		if err := c.Auth(smtp.PlainAuth("", mc.RelayUsername, mc.RelayPassword, host)); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
