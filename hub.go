package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gotify/plugin-api"
)

// hub is the state shared by all instances (one per Gotify user) of the plugin.
// All fields are guarded by mu; network calls happen without holding it.
type hub struct {
	mu         sync.Mutex
	instances  map[uint]*Instance
	states     map[string]*linkState
	verifies   map[uint]*pendingVerify
	deliveries map[string]*delivery
	approvals  map[string]*approval

	http *http.Client
	oidc *oidcClient
	now  func() time.Time

	recheckOwner *Instance
	recheckStop  chan struct{}

	mail *mailGate // guarded by mailMu
}

type linkData struct {
	Phone        string    `json:"phone"`
	Subject      string    `json:"subject"`
	Username     string    `json:"username"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	LinkedAt     time.Time `json:"linked_at"`
	CheckedAt    time.Time `json:"checked_at"`
}

type linkState struct {
	userID   uint
	verifier string
	created  time.Time
}

type pendingVerify struct {
	identity     identity
	refreshToken string
	code         string
	tries        int
	expires      time.Time
}

type delivery struct {
	id, key string
	userID  uint
	sender  string
	title   string
	message string
	phone   string
	opened  bool
	smsSent bool
	created time.Time
	timer   *time.Timer
}

const (
	linkStateTTL      = 15 * time.Minute
	verifyTTL         = 10 * time.Minute
	verifyMaxTries    = 5
	deliveryTTL       = 24 * time.Hour
	hiddenPlaceholder = "New message - tap to view"
)

func newHub() *hub {
	c := &http.Client{Timeout: 30 * time.Second}
	return &hub{
		instances:  map[uint]*Instance{},
		states:     map[string]*linkState{},
		verifies:   map[uint]*pendingVerify{},
		deliveries: map[string]*delivery{},
		approvals:  map[string]*approval{},
		http:       c,
		oidc:       newOIDCClient(c),
		now:        time.Now,
	}
}

var theHub = newHub()

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func randomCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

func equalSecret(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// controlLocked returns the instance whose configuration is used: the enabled
// administrator instance with the lowest user ID that has public_url set.
func (h *hub) controlLocked() *Instance {
	var best *Instance
	for _, i := range h.instances {
		if !i.user.Admin || !i.enabled || i.cfg == nil || i.cfg.PublicURL == "" {
			continue
		}
		if best == nil || i.user.ID < best.user.ID {
			best = i
		}
	}
	return best
}

func (h *hub) control() (*Instance, *Config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.controlLocked()
	if c == nil {
		return nil, nil
	}
	cfg := *c.cfg
	return c, &cfg
}

// usersForPhoneLocked returns the users a number is linked to.
func (h *hub) usersForPhoneLocked(phone string) []*Instance {
	var r []*Instance
	for _, i := range h.instances {
		if i.link != nil && i.link.Phone == phone {
			r = append(r, i)
		}
	}
	sort.Slice(r, func(a, b int) bool { return r[a].user.ID < r[b].user.ID })
	return r
}

func (h *hub) publicURL(cfg *Config, inst *Instance, rel string) string {
	return strings.TrimSuffix(cfg.PublicURL, "/") + inst.basePath + rel
}

// reconcile follows a change of the control instance or its configuration.
func (h *hub) reconcile() {
	h.reconcileRecheck()
	_, cfg := h.control()
	h.reconcileMail(cfg)
}

// reconcileRecheck restarts the recheck loop when the control instance or its
// interval changed.
func (h *hub) reconcileRecheck() {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.controlLocked()
	if c == h.recheckOwner && h.recheckStop != nil {
		return
	}
	if h.recheckStop != nil {
		close(h.recheckStop)
		h.recheckStop = nil
	}
	h.recheckOwner = c
	if c == nil || !c.cfg.oidcConfigured() || c.cfg.recheckInterval() == 0 {
		return
	}
	stop := make(chan struct{})
	h.recheckStop = stop
	interval := c.cfg.recheckInterval()
	go h.recheckLoop(interval, stop)
}

func (h *hub) recheckLoop(interval time.Duration, stop chan struct{}) {
	tick := interval / 24
	if tick < time.Minute {
		tick = time.Minute
	}
	if tick > time.Hour {
		tick = time.Hour
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			h.recheckDue(interval)
		}
	}
}

// recheckDue checks every link older than interval again with its refresh
// token: a changed number, user name or a rejected token removes the link.
func (h *hub) recheckDue(interval time.Duration) {
	_, cfg := h.control()
	if cfg == nil || !cfg.oidcConfigured() {
		return
	}
	h.mu.Lock()
	var due []*Instance
	for _, i := range h.instances {
		if i.link != nil && i.link.RefreshToken != "" && h.now().Sub(i.link.CheckedAt) >= interval {
			due = append(due, i)
		}
	}
	h.mu.Unlock()
	for _, i := range due {
		h.recheck(cfg, i)
	}
}

func (h *hub) recheck(cfg *Config, inst *Instance) {
	h.mu.Lock()
	if inst.link == nil {
		h.mu.Unlock()
		return
	}
	link := *inst.link
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, err := h.oidc.refresh(ctx, &cfg.OIDC, link.RefreshToken)
	if errors.Is(err, errInvalidGrant) {
		h.unlink(inst, "the identity provider no longer accepts the link (account changed, disabled or consent revoked)")
		return
	}
	if err != nil {
		log.Printf("gotify-phonelink: recheck of %s skipped: %v", inst.user.Name, err)
		return
	}
	claims, err := h.oidc.userinfo(ctx, &cfg.OIDC, tok.AccessToken)
	if err != nil {
		log.Printf("gotify-phonelink: recheck of %s skipped: %v", inst.user.Name, err)
		return
	}
	id, err := identityFromClaims(claims, cfg.usernameClaim())
	if err != nil || id.Subject != link.Subject || id.Username != inst.user.Name || id.Phone != link.Phone {
		h.unlink(inst, "the phone number or account at the identity provider changed - link it again")
		return
	}
	h.mu.Lock()
	if inst.link != nil {
		inst.link.CheckedAt = h.now()
		if tok.RefreshToken != "" {
			inst.link.RefreshToken = tok.RefreshToken
		}
		inst.saveLocked()
	}
	h.mu.Unlock()
}

// startLink begins linking for inst and returns the URL to send the user to.
func (h *hub) startLink(ctx context.Context, inst *Instance) (string, error) {
	ctrl, cfg := h.control()
	if cfg == nil || !cfg.oidcConfigured() {
		return "", errors.New("the administrator has not configured gotify-phonelink yet")
	}
	state, verifier := randomHex(24), randomHex(32)
	h.mu.Lock()
	for k, s := range h.states {
		if h.now().Sub(s.created) > linkStateTTL {
			delete(h.states, k)
		}
	}
	h.states[state] = &linkState{userID: inst.user.ID, verifier: verifier, created: h.now()}
	h.mu.Unlock()
	return h.oidc.authURL(ctx, &cfg.OIDC, h.publicURL(cfg, ctrl, "oidc/callback"), state, verifier)
}

// linkResult tells the callback what to show.
type linkResult struct {
	inst      *Instance
	needsCode bool
	phone     string
}

// finishLink handles the provider's answer for a link started by startLink.
func (h *hub) finishLink(ctx context.Context, state, code string) (*linkResult, error) {
	ctrl, cfg := h.control()
	if cfg == nil || !cfg.oidcConfigured() {
		return nil, errors.New("gotify-phonelink is not configured")
	}
	h.mu.Lock()
	st := h.states[state]
	delete(h.states, state)
	var inst *Instance
	if st != nil {
		inst = h.instances[st.userID]
	}
	h.mu.Unlock()
	if st == nil || h.now().Sub(st.created) > linkStateTTL || inst == nil {
		return nil, errors.New("the link request is unknown or expired - start again")
	}

	tok, err := h.oidc.exchange(ctx, &cfg.OIDC, code, h.publicURL(cfg, ctrl, "oidc/callback"), st.verifier)
	if err != nil {
		return nil, err
	}
	claims, err := h.oidc.userinfo(ctx, &cfg.OIDC, tok.AccessToken)
	if err != nil {
		return nil, err
	}
	id, err := identityFromClaims(claims, cfg.usernameClaim())
	if err != nil {
		return nil, err
	}
	if id.Username != inst.user.Name {
		log.Printf("gotify-phonelink: link of %s refused: provider account is %q", inst.user.Name, id.Username)
		return nil, fmt.Errorf("you are logged in at the identity provider as %q, but this is the Gotify account %q", id.Username, inst.user.Name)
	}
	if !id.PhoneVerified && !cfg.OIDC.TrustUnverifiedPhone && !cfg.SMS.VerifyOnLink {
		return nil, errors.New("the identity provider does not mark the phone number as verified")
	}
	if err := h.checkFree(inst, id.Phone); err != nil {
		return nil, err
	}

	if !cfg.SMS.VerifyOnLink {
		if err := h.setLink(inst, id, tok.RefreshToken); err != nil {
			return nil, err
		}
		return &linkResult{inst: inst, phone: id.Phone}, nil
	}

	v := &pendingVerify{identity: *id, refreshToken: tok.RefreshToken, code: randomCode(), expires: h.now().Add(verifyTTL)}
	smsCtx, cancel := context.WithTimeout(ctx, cfg.smsTimeout())
	defer cancel()
	if err := sendSMS(smsCtx, h.http, &cfg.SMS, id.Phone, "Gotify verification code: "+v.code); err != nil {
		log.Printf("gotify-phonelink: verification SMS for %s failed: %v", inst.user.Name, err)
		return nil, errors.New("the verification SMS could not be sent")
	}
	h.mu.Lock()
	h.verifies[inst.user.ID] = v
	h.mu.Unlock()
	return &linkResult{inst: inst, needsCode: true, phone: id.Phone}, nil
}

func (h *hub) checkFree(inst *Instance, phone string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, o := range h.usersForPhoneLocked(phone) {
		if o != inst {
			log.Printf("gotify-phonelink: %s tried to link %s, already linked to %s", inst.user.Name, maskPhone(phone), o.user.Name)
			return errors.New("this phone number is already linked to another Gotify account")
		}
	}
	return nil
}

// verifyCode completes a link that waits for the SMS code.
func (h *hub) verifyCode(inst *Instance, code string) error {
	h.mu.Lock()
	v := h.verifies[inst.user.ID]
	if v == nil || h.now().After(v.expires) {
		delete(h.verifies, inst.user.ID)
		h.mu.Unlock()
		return errors.New("no code is pending or it expired - link again")
	}
	v.tries++
	if v.tries > verifyMaxTries {
		delete(h.verifies, inst.user.ID)
		h.mu.Unlock()
		return errors.New("too many wrong codes - link again")
	}
	if !equalSecret(strings.TrimSpace(code), v.code) {
		h.mu.Unlock()
		return errors.New("wrong code")
	}
	delete(h.verifies, inst.user.ID)
	h.mu.Unlock()
	if err := h.checkFree(inst, v.identity.Phone); err != nil {
		return err
	}
	return h.setLink(inst, &v.identity, v.refreshToken)
}

func (h *hub) setLink(inst *Instance, id *identity, refreshToken string) error {
	h.mu.Lock()
	now := h.now()
	inst.link = &linkData{
		Phone: id.Phone, Subject: id.Subject, Username: id.Username,
		RefreshToken: refreshToken, LinkedAt: now, CheckedAt: now,
	}
	err := inst.saveLocked()
	msg := inst.msg
	h.mu.Unlock()
	if err != nil {
		return err
	}
	log.Printf("gotify-phonelink: %s linked %s", inst.user.Name, maskPhone(id.Phone))
	if msg != nil {
		_ = msg.SendMessage(plugin.Message{Title: "Phone number linked",
			Message: "Messages to " + id.Phone + " are delivered to this Gotify account now.", Priority: 4})
	}
	return nil
}

func (h *hub) unlink(inst *Instance, reason string) {
	h.mu.Lock()
	if inst.link == nil {
		h.mu.Unlock()
		return
	}
	phone := inst.link.Phone
	inst.link = nil
	_ = inst.saveLocked()
	msg := inst.msg
	h.mu.Unlock()
	log.Printf("gotify-phonelink: %s unlinked %s: %s", inst.user.Name, maskPhone(phone), reason)
	if msg != nil {
		_ = msg.SendMessage(plugin.Message{Title: "Phone number unlinked",
			Message: phone + " is no longer linked: " + reason + ".", Priority: 4})
	}
}

// sendRequest is the body of POST .../send.
type sendRequest struct {
	To          string      `json:"to"`
	User        string      `json:"user"`
	Title       string      `json:"title"`
	Message     string      `json:"message"`
	Priority    *int        `json:"priority"`
	Failover    interface{} `json:"failover"`
	HideContent bool        `json:"hide_content"`
	// Who asked the sender for this message (e.g. the logged-in Matrix user
	// ma1sd got the request from); set = no moderator approval needed
	Requester string `json:"requester"`
}

type sendResult struct {
	Status     string `json:"status"` // push or sms
	ID         string `json:"id,omitempty"`
	FailoverIn string `json:"failover_in,omitempty"`
}

// sendError carries the HTTP status for the send endpoint.
type sendError struct {
	code int
	msg  string
}

func (e *sendError) Error() string { return e.msg }

func parseFailover(v interface{}) (*time.Duration, error) {
	switch f := v.(type) {
	case nil:
		return nil, nil
	case float64:
		if f < 0 {
			return nil, errors.New("failover must not be negative")
		}
		d := time.Duration(f * float64(time.Second))
		return &d, nil
	case string:
		d, err := parseDuration(f)
		if err != nil {
			return nil, err
		}
		return &d, nil
	}
	return nil, errors.New("failover must be seconds or a duration like \"2m\"")
}

func (h *hub) senderByToken(cfg *Config, token string) *SenderConfig {
	var found *SenderConfig
	for i := range cfg.Senders {
		if equalSecret(cfg.Senders[i].Token, token) {
			found = &cfg.Senders[i]
		}
	}
	return found
}

func (h *hub) send(ctx context.Context, cfg *Config, ctrl *Instance, s *SenderConfig, req *sendRequest) (*sendResult, error) {
	if strings.TrimSpace(req.Message) == "" {
		return nil, &sendError{http.StatusBadRequest, "message is required"}
	}
	priority := 5
	if req.Priority != nil {
		priority = *req.Priority
	}
	requested, err := parseFailover(req.Failover)
	if err != nil {
		return nil, &sendError{http.StatusBadRequest, err.Error()}
	}
	var phone string
	if req.To != "" {
		if phone, err = normalizePhone(req.To); err != nil {
			return nil, &sendError{http.StatusBadRequest, err.Error()}
		}
	} else if req.User == "" {
		return nil, &sendError{http.StatusBadRequest, "to or user is required"}
	}

	h.mu.Lock()
	var target *Instance
	if phone != "" {
		users := h.usersForPhoneLocked(phone)
		if len(users) > 1 {
			names := make([]string, 0, len(users))
			for _, u := range users {
				names = append(names, u.user.Name)
			}
			h.mu.Unlock()
			log.Printf("gotify-phonelink: %s is linked to %s - not delivered", maskPhone(phone), strings.Join(names, ", "))
			return nil, &sendError{http.StatusConflict, "the phone number is linked to more than one account"}
		}
		if len(users) == 1 {
			target = users[0]
		}
	} else {
		for _, i := range h.instances {
			if i.user.Name == req.User {
				target = i
			}
		}
		if target != nil && target.link != nil {
			phone = target.link.Phone
		}
	}
	if target != nil && (!target.enabled || target.msg == nil) {
		target = nil
	}
	h.mu.Unlock()

	smsAllowed := s.SMS && cfg.smsConfigured() && phone != "" && priority >= s.SMSMinPriority && cfg.smsPermitted(phone)
	text := req.Message
	if req.Title != "" {
		text = req.Title + "\n" + req.Message
	}

	if target == nil {
		if phone != "" && s.SMS && s.SMSUnlinked && !cfg.smsPermitted(phone) {
			log.Printf("gotify-phonelink: %s -> %s refused: not in sms.allowed_prefixes", s.Name, maskPhone(phone))
			return nil, &sendError{http.StatusForbidden, "SMS to this phone number is not allowed"}
		}
		if smsAllowed && s.SMSUnlinked && s.Approval && req.Requester == "" {
			return h.requestApproval(cfg, ctrl, s, phone, text)
		}
		if smsAllowed && s.SMSUnlinked {
			smsCtx, cancel := context.WithTimeout(ctx, cfg.smsTimeout())
			defer cancel()
			if err := sendSMS(smsCtx, h.http, &cfg.SMS, phone, text); err != nil {
				log.Printf("gotify-phonelink: SMS from %s to %s failed: %v", s.Name, maskPhone(phone), err)
				return nil, &sendError{http.StatusBadGateway, "SMS could not be sent"}
			}
			if req.Requester != "" {
				log.Printf("gotify-phonelink: %s -> %s by SMS (not linked, requested by %s)", s.Name, maskPhone(phone), req.Requester)
			} else {
				log.Printf("gotify-phonelink: %s -> %s by SMS (not linked)", s.Name, maskPhone(phone))
			}
			return &sendResult{Status: "sms"}, nil
		}
		return nil, &sendError{http.StatusNotFound, "the recipient has not linked this phone number to an enabled Gotify account"}
	}

	failover := time.Duration(0)
	if smsAllowed {
		failover = s.failoverFor(requested)
	}
	hide := s.HideContent || req.HideContent
	d := &delivery{
		id: randomHex(12), key: randomHex(16), userID: target.user.ID, sender: s.Name,
		title: req.Title, message: req.Message, phone: phone, created: h.now(),
	}
	msg := plugin.Message{Title: req.Title, Message: req.Message, Priority: priority}
	if hide || failover > 0 {
		show := h.publicURL(cfg, ctrl, "m/"+d.id+"?k="+d.key)
		// The click URL only works on the system notification; the markdown
		// link makes it tappable in the app's message list and the web UI too.
		msg.Extras = map[string]interface{}{
			"client::notification": map[string]interface{}{"click": map[string]interface{}{"url": show}},
			"client::display":      map[string]interface{}{"contentType": "text/markdown"},
		}
		link := "[Open message](" + show + ")"
		if failover > 0 {
			link = "[Confirm](" + show + ") - otherwise it is also sent by SMS in " + failover.String() + "."
		}
		if hide {
			msg.Message = hiddenPlaceholder + "\n\n" + link
		} else {
			msg.Message = markdownEscape(req.Message) + "\n\n" + link
		}
	}

	h.mu.Lock()
	h.deliveries[d.id] = d
	if failover > 0 {
		id := d.id
		d.timer = time.AfterFunc(failover, func() { h.failover(id, cfg) })
	}
	time.AfterFunc(deliveryTTL, func() { h.forget(d.id) })
	handler := target.msg
	h.mu.Unlock()

	if err := handler.SendMessage(msg); err != nil {
		h.forget(d.id)
		return nil, &sendError{http.StatusInternalServerError, "Gotify did not accept the message"}
	}
	res := &sendResult{Status: "push", ID: d.id}
	if failover > 0 {
		res.FailoverIn = failover.String()
	}
	log.Printf("gotify-phonelink: %s -> %s by push (message %s, failover %s)", s.Name, target.user.Name, d.id, failover)
	return res, nil
}

// markdownEscape keeps a sender's text literal when the message is rendered
// as markdown.
func markdownEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("\\`*_{}[]()#+-.!|<>~", r) {
			b.WriteByte('\\')
		}
		if r == '\n' {
			b.WriteString("  ")
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (h *hub) forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := h.deliveries[id]; d != nil {
		if d.timer != nil {
			d.timer.Stop()
		}
		delete(h.deliveries, id)
	}
}

func (h *hub) failover(id string, cfg *Config) {
	h.mu.Lock()
	d := h.deliveries[id]
	if d == nil || d.opened || d.smsSent {
		h.mu.Unlock()
		return
	}
	d.smsSent = true
	phone, text, sender := d.phone, d.message, d.sender
	if d.title != "" {
		text = d.title + "\n" + d.message
	}
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.smsTimeout())
	defer cancel()
	if err := sendSMS(ctx, h.http, &cfg.SMS, phone, text); err != nil {
		log.Printf("gotify-phonelink: failover SMS from %s to %s failed: %v", sender, maskPhone(phone), err)
		return
	}
	log.Printf("gotify-phonelink: message %s from %s not opened - sent to %s by SMS", id, sender, maskPhone(phone))
}

// open marks a delivery as read (the user tapped the notification) and
// returns it for display.
func (h *hub) open(id, key string) (*delivery, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.deliveries[id]
	if d == nil {
		log.Printf("gotify-phonelink: open of unknown or expired message %s (%d pending)", id, len(h.deliveries))
		return nil, false
	}
	if !equalSecret(d.key, key) {
		log.Printf("gotify-phonelink: open of message %s with a wrong key (len %d)", id, len(key))
		return nil, false
	}
	if !d.opened {
		d.opened = true
		if d.timer != nil {
			d.timer.Stop()
		}
	}
	c := *d
	return &c, true
}

// Instance is the plugin instance of one Gotify user.
type Instance struct {
	h        *hub
	user     plugin.UserContext
	basePath string
	msg      plugin.MessageHandler
	store    plugin.StorageHandler
	cfg      *Config
	enabled  bool
	link     *linkData
}

func (i *Instance) saveLocked() error {
	if i.store == nil {
		return errors.New("no storage")
	}
	if i.link == nil {
		return i.store.Save(nil)
	}
	raw, err := json.Marshal(i.link)
	if err != nil {
		return err
	}
	return i.store.Save(raw)
}
