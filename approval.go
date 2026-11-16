package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gotify/plugin-api"
)

// Moderator approval: a message to an unknown recipient (an SMS to a number no
// Gotify user linked, or a mail to an address no Matrix user has) is held
// until a moderator approves it on a page linked from a Gotify push.

const maxOpenApprovals = 50

type approval struct {
	id      string
	kind    string // "sms" or "mail"
	sender  string
	to      string // phone number or mail address
	created time.Time
	expires time.Time
	keys    map[string]string // moderator name -> key of the review link
	// decision
	decided   string // "", "approved", "rejected", "expired"
	decidedBy string
	// deliver sends the held message; it is called once, after approval
	deliver func(ctx context.Context) error
	err     error
	timer   *time.Timer
}

// moderatorsLocked returns the enabled instances allowed to approve.
func (h *hub) moderatorsLocked(cfg *Config) []*Instance {
	allowed := map[string]bool{}
	for _, m := range cfg.Moderators {
		allowed[m] = true
	}
	var r []*Instance
	for _, i := range h.instances {
		if !i.enabled || i.msg == nil {
			continue
		}
		if (len(allowed) == 0 && i.user.Admin) || allowed[i.user.Name] {
			r = append(r, i)
		}
	}
	sort.Slice(r, func(a, b int) bool { return r[a].user.ID < r[b].user.ID })
	return r
}

// hold queues a message for approval and notifies the moderators. A newer
// message to the same recipient from the same sender replaces a pending one
// (e.g. a new code) without notifying again.
func (h *hub) hold(cfg *Config, ctrl *Instance, kind, sender, to string, timeout time.Duration, deliver func(ctx context.Context) error) (*approval, error) {
	h.mu.Lock()
	for _, a := range h.approvals {
		if a.decided == "" && a.kind == kind && a.sender == sender && a.to == to {
			a.deliver = deliver
			h.mu.Unlock()
			log.Printf("gotify-phonelink: %s to %s from %s still waits for approval (replaced)", kind, maskAddr(kind, to), sender)
			return a, nil
		}
	}
	open := 0
	for _, a := range h.approvals {
		if a.decided == "" {
			open++
		}
	}
	if open >= maxOpenApprovals {
		h.mu.Unlock()
		return nil, &sendError{http.StatusTooManyRequests, "too many messages wait for approval"}
	}
	mods := h.moderatorsLocked(cfg)
	if len(mods) == 0 {
		h.mu.Unlock()
		return nil, &sendError{http.StatusServiceUnavailable, "no moderator has the plugin enabled"}
	}
	now := h.now()
	a := &approval{
		id: randomHex(12), kind: kind, sender: sender, to: to,
		created: now, expires: now.Add(timeout), keys: map[string]string{}, deliver: deliver,
	}
	type note struct {
		msg plugin.MessageHandler
		url string
	}
	var notes []note
	for _, m := range mods {
		k := randomHex(16)
		a.keys[m.user.Name] = k
		notes = append(notes, note{m.msg, h.publicURL(cfg, ctrl, "approve/"+a.id+"?m="+url.QueryEscape(m.user.Name)+"&k="+k)})
	}
	h.approvals[a.id] = a
	id := a.id
	a.timer = time.AfterFunc(timeout, func() { h.expire(id) })
	h.mu.Unlock()

	what := "an SMS to `" + to + "` (phone number not linked to Gotify)"
	if kind == "mail" {
		what = "an e-mail to `" + to + "` (address of no Matrix user)"
	}
	for _, n := range notes {
		_ = n.msg.SendMessage(plugin.Message{
			Title:    "Approval needed: " + sender,
			Message:  fmt.Sprintf("**%s** wants to send %s.\n\n[Review](%s) - expires in %s.", sender, what, n.url, timeout),
			Priority: 8,
			Extras: map[string]interface{}{
				"client::display":      map[string]interface{}{"contentType": "text/markdown"},
				"client::notification": map[string]interface{}{"click": map[string]interface{}{"url": n.url}},
			},
		})
	}
	log.Printf("gotify-phonelink: %s to %s from %s waits for approval by %d moderator(s)", kind, maskAddr(kind, to), sender, len(notes))
	return a, nil
}

func (h *hub) expire(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.approvals[id]
	if a == nil {
		return
	}
	if a.decided == "" {
		a.decided = "expired"
		a.deliver = nil
		log.Printf("gotify-phonelink: %s to %s from %s expired without approval", a.kind, maskAddr(a.kind, a.to), a.sender)
	}
	// keep the decision visible for a while, then forget it
	time.AfterFunc(time.Hour, func() {
		h.mu.Lock()
		delete(h.approvals, id)
		h.mu.Unlock()
	})
}

// approvalView is what the review page shows.
type approvalView struct {
	ID, Kind, Sender, To, Decided, DecidedBy, Error string
	Created, Expires                                time.Time
}

func (h *hub) reviewable(id, mod, key string) (*approvalView, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.approvals[id]
	if a == nil || mod == "" || !equalSecret(a.keys[mod], key) {
		return nil, false
	}
	v := &approvalView{ID: a.id, Kind: a.kind, Sender: a.sender, To: a.to, Decided: a.decided,
		DecidedBy: a.decidedBy, Created: a.created, Expires: a.expires}
	if a.err != nil {
		v.Error = a.err.Error()
	}
	return v, true
}

// decide approves or rejects a held message; approve delivers it.
func (h *hub) decide(ctx context.Context, id, mod, key string, approve bool) (*approvalView, error) {
	h.mu.Lock()
	a := h.approvals[id]
	if a == nil || mod == "" || !equalSecret(a.keys[mod], key) {
		h.mu.Unlock()
		return nil, fmt.Errorf("this request is unknown or expired")
	}
	if a.decided != "" {
		h.mu.Unlock()
		v, _ := h.reviewable(id, mod, key)
		return v, nil
	}
	a.decidedBy = mod
	deliver := a.deliver
	a.deliver = nil
	if a.timer != nil {
		a.timer.Stop()
	}
	if approve {
		a.decided = "approved"
	} else {
		a.decided = "rejected"
	}
	h.mu.Unlock()
	time.AfterFunc(time.Hour, func() {
		h.mu.Lock()
		delete(h.approvals, id)
		h.mu.Unlock()
	})

	log.Printf("gotify-phonelink: %s to %s from %s %s by %s", a.kind, maskAddr(a.kind, a.to), a.sender, a.decided, mod)
	if approve && deliver != nil {
		if err := deliver(ctx); err != nil {
			log.Printf("gotify-phonelink: approved %s to %s failed: %v", a.kind, maskAddr(a.kind, a.to), err)
			h.mu.Lock()
			a.err = err
			h.mu.Unlock()
		}
	}
	v, _ := h.reviewable(id, mod, key)
	return v, nil
}

// requestApproval holds an SMS to an unlinked number.
func (h *hub) requestApproval(cfg *Config, ctrl *Instance, s *SenderConfig, phone, text string) (*sendResult, error) {
	smsCfg := cfg.SMS
	timeout := cfg.smsTimeout()
	a, err := h.hold(cfg, ctrl, "sms", s.Name, phone, s.approvalTimeout(), func(ctx context.Context) error {
		c, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return sendSMS(c, h.http, &smsCfg, phone, text)
	})
	if err != nil {
		return nil, err
	}
	return &sendResult{Status: "pending", ID: a.id}, nil
}

func maskAddr(kind, to string) string {
	if kind == "sms" {
		return maskPhone(to)
	}
	at := strings.LastIndex(to, "@")
	if at < 2 {
		return "***" + to[max(at, 0):]
	}
	return to[:1] + "***" + to[at:]
}
