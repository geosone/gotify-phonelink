package main

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Config is the configuration of an administrator's instance. The plugin reads
// it from the enabled administrator instance with the lowest user ID that has
// public_url set (the "control" instance); all others are ignored.
type Config struct {
	// Public base URL of Gotify, e.g. https://gotify.example.org
	PublicURL string         `yaml:"public_url"`
	OIDC      OIDCConfig     `yaml:"oidc"`
	SMS       SMSConfig      `yaml:"sms"`
	Senders   []SenderConfig `yaml:"senders"`
	// Gotify users who approve messages to unknown recipients (senders with
	// approval: true); empty = all administrators
	Moderators []string `yaml:"moderators"`
	// SMTP gate for Synapse mail (see mail.go)
	Mail MailConfig `yaml:"mail"`
}

// OIDCConfig is the OpenID Connect provider the phone numbers come from. It
// must be the provider Gotify itself logs in with: the username claim has to
// match the Gotify user name.
type OIDCConfig struct {
	Issuer       string   `yaml:"issuer"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Scopes       []string `yaml:"scopes"`
	// Claim holding the Gotify user name (default preferred_username)
	UsernameClaim string `yaml:"username_claim"`
	// Accept phone numbers the provider does not mark as phone_number_verified
	// (e.g. a directory only administrators maintain). Not needed with
	// sms.verify_on_link.
	TrustUnverifiedPhone bool `yaml:"trust_unverified_phone"`
	// How often linked numbers are checked again with the refresh token
	// (e.g. 24h; empty or 0 = never)
	RecheckInterval string `yaml:"recheck_interval"`
}

// SMSConfig is smstools-http-api (https://github.com/geosone/smstools-http-api),
// used for the verification code and for failover.
type SMSConfig struct {
	URL      string `yaml:"url"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	Queue    string `yaml:"queue"`
	Timeout  string `yaml:"timeout"`
	// Send a code to the number when linking; only a confirmed number is linked
	VerifyOnLink bool `yaml:"verify_on_link"`
	// SMS only to numbers starting with one of these (e.g. ["+43"]); empty = all
	AllowedPrefixes []string `yaml:"allowed_prefixes"`
}

// SenderConfig is a client allowed to POST to the send endpoint.
type SenderConfig struct {
	Name  string `yaml:"name"`
	Token string `yaml:"token"`
	// Time until an unopened message is also sent by SMS (e.g. 2m; empty = no failover)
	Failover string `yaml:"failover"`
	// Upper limit for a failover requested per message (default: failover)
	FailoverMax string `yaml:"failover_max"`
	// May use SMS (failover and unlinked recipients)
	SMS bool `yaml:"sms"`
	// Recipients without a linked, enabled Gotify user get the message by SMS at once
	SMSUnlinked bool `yaml:"sms_unlinked"`
	// SMS only for messages with at least this priority
	SMSMinPriority int `yaml:"sms_min_priority"`
	// Push only "tap to view"; the text is shown on the plugin page (for codes)
	HideContent bool `yaml:"hide_content"`
	// SMS to unlinked numbers (sms_unlinked) wait until a moderator approves them
	Approval bool `yaml:"approval"`
	// How long a request waits for a moderator (default 24h)
	ApprovalTimeout string `yaml:"approval_timeout"`
}

// UserConfig is the (empty) configuration of non-administrators.
type UserConfig struct{}

const (
	defaultUsernameClaim   = "preferred_username"
	defaultSMSTimeout      = 30 * time.Second
	minTokenLength         = 16
	defaultApprovalTimeout = 24 * time.Hour
)

func defaultConfig() *Config {
	return &Config{
		OIDC: OIDCConfig{
			Scopes:          []string{"openid", "profile", "phone", "offline_access"},
			UsernameClaim:   defaultUsernameClaim,
			RecheckInterval: "24h",
		},
		SMS:     SMSConfig{Timeout: "30s"},
		Senders: []SenderConfig{},
	}
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, errors.New("must not be negative")
	}
	return d, nil
}

func (c *Config) oidcConfigured() bool {
	return c.OIDC.Issuer != "" && c.OIDC.ClientID != "" && c.OIDC.ClientSecret != ""
}

func (c *Config) smsConfigured() bool {
	return c.SMS.URL != ""
}

func (c *Config) usernameClaim() string {
	if c.OIDC.UsernameClaim == "" {
		return defaultUsernameClaim
	}
	return c.OIDC.UsernameClaim
}

func (c *Config) smsTimeout() time.Duration {
	d, err := parseDuration(c.SMS.Timeout)
	if err != nil || d == 0 {
		return defaultSMSTimeout
	}
	return d
}

func (c *Config) recheckInterval() time.Duration {
	d, _ := parseDuration(c.OIDC.RecheckInterval)
	return d
}

// Validate checks the configuration of an administrator.
func (c *Config) Validate() error {
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("public_url %q is not an http(s) URL", c.PublicURL)
		}
	}
	o := c.OIDC
	if (o.Issuer != "" || o.ClientID != "" || o.ClientSecret != "") && !c.oidcConfigured() {
		return errors.New("oidc needs issuer, client_id and client_secret")
	}
	if o.Issuer != "" {
		if u, err := url.Parse(o.Issuer); err != nil || u.Scheme != "https" && u.Scheme != "http" {
			return fmt.Errorf("oidc.issuer %q is not an http(s) URL", o.Issuer)
		}
	}
	if _, err := parseDuration(o.RecheckInterval); err != nil {
		return fmt.Errorf("oidc.recheck_interval: %w", err)
	}
	if c.SMS.URL != "" {
		if u, err := url.Parse(c.SMS.URL); err != nil || u.Scheme != "https" && u.Scheme != "http" {
			return fmt.Errorf("sms.url %q is not an http(s) URL", c.SMS.URL)
		}
	}
	if c.SMS.VerifyOnLink && !c.smsConfigured() {
		return errors.New("sms.verify_on_link needs sms.url")
	}
	if _, err := parseDuration(c.SMS.Timeout); err != nil {
		return fmt.Errorf("sms.timeout: %w", err)
	}
	for _, p := range c.SMS.AllowedPrefixes {
		if len(p) < 2 || p[0] != '+' || strings.Trim(p[1:], "0123456789") != "" {
			return fmt.Errorf("sms.allowed_prefixes: %q is not \"+\" followed by digits", p)
		}
	}
	if err := c.Mail.validate(); err != nil {
		return err
	}
	names := map[string]bool{}
	tokens := map[string]bool{}
	for i, s := range c.Senders {
		if s.Name == "" {
			return fmt.Errorf("senders[%d]: name is required", i)
		}
		if names[s.Name] {
			return fmt.Errorf("senders: duplicate name %q", s.Name)
		}
		names[s.Name] = true
		if len(s.Token) < minTokenLength {
			return fmt.Errorf("sender %q: token must have at least %d characters", s.Name, minTokenLength)
		}
		if tokens[s.Token] {
			return fmt.Errorf("sender %q: token is used by another sender", s.Name)
		}
		tokens[s.Token] = true
		f, err := parseDuration(s.Failover)
		if err != nil {
			return fmt.Errorf("sender %q: failover: %w", s.Name, err)
		}
		m, err := parseDuration(s.FailoverMax)
		if err != nil {
			return fmt.Errorf("sender %q: failover_max: %w", s.Name, err)
		}
		if m != 0 && m < f {
			return fmt.Errorf("sender %q: failover_max is lower than failover", s.Name)
		}
		if (s.SMSUnlinked || f > 0) && !s.SMS {
			return fmt.Errorf("sender %q: failover and sms_unlinked need sms: true", s.Name)
		}
		if s.SMS && !c.smsConfigured() {
			return fmt.Errorf("sender %q: sms needs sms.url", s.Name)
		}
		if s.Approval && !s.SMSUnlinked {
			return fmt.Errorf("sender %q: approval needs sms_unlinked: true", s.Name)
		}
		if _, err := parseDuration(s.ApprovalTimeout); err != nil {
			return fmt.Errorf("sender %q: approval_timeout: %w", s.Name, err)
		}
	}
	return nil
}

// smsPermitted tells whether SMS may go to phone (sms.allowed_prefixes).
func (c *Config) smsPermitted(phone string) bool {
	if len(c.SMS.AllowedPrefixes) == 0 {
		return true
	}
	for _, p := range c.SMS.AllowedPrefixes {
		if strings.HasPrefix(phone, p) {
			return true
		}
	}
	return false
}

func (s *SenderConfig) approvalTimeout() time.Duration {
	d, _ := parseDuration(s.ApprovalTimeout)
	if d == 0 {
		return defaultApprovalTimeout
	}
	return d
}

// failoverFor returns the failover delay for a message: the sender's default,
// or the requested one capped at failover_max (0 = no failover).
func (s *SenderConfig) failoverFor(requested *time.Duration) time.Duration {
	def, _ := parseDuration(s.Failover)
	if requested == nil {
		return def
	}
	max, _ := parseDuration(s.FailoverMax)
	if max == 0 {
		max = def
	}
	d := *requested
	if d > max {
		d = max
	}
	return d
}

// normalizePhone returns the number in E.164 form (+<digits>). Numbers without
// "+" or "00" are taken as international (as ma1sd sends them).
func normalizePhone(s string) (string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "00") {
		s = "+" + s[2:]
	}
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
		case r == ' ' || r == '-' || r == '(' || r == ')' || r == '/' || r == '.':
		default:
			return "", fmt.Errorf("invalid character %q in phone number", r)
		}
	}
	d := b.String()
	if len(d) < 6 || len(d) > 15 {
		return "", fmt.Errorf("phone number %q has %d digits", s, len(d))
	}
	if d[0] == '0' {
		return "", fmt.Errorf("phone number %q is not in international format", s)
	}
	return "+" + d, nil
}

// maskPhone hides the middle of a number: +43676*****496
func maskPhone(p string) string {
	if len(p) <= 8 {
		return p
	}
	return p[:6] + strings.Repeat("*", len(p)-9) + p[len(p)-3:]
}
