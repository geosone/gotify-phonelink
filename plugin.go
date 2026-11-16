// gotify-phonelink delivers messages addressed to a phone number to the Gotify
// user that number belongs to. Users link their number once through the
// OpenID Connect provider Gotify logs in with (scope "phone"), optionally
// confirmed by an SMS code; senders (ma1sd, Zabbix, ...) POST to one endpoint.
// Unopened messages can fall back to SMS after a per-sender delay.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gotify/plugin-api"
)

// Version is set at build time.
var Version = "dev"

// GetGotifyPluginInfo returns gotify plugin info.
func GetGotifyPluginInfo() plugin.Info {
	return plugin.Info{
		ModulePath:  "github.com/geosone/gotify-phonelink",
		Version:     Version,
		Author:      "Mario Fetka",
		Website:     "https://github.com/geosone/gotify-phonelink",
		Description: "Deliver messages addressed to a phone number to the user who linked it (via OIDC), with SMS failover",
		License:     "MIT",
		Name:        "phonelink",
	}
}

// NewGotifyPluginInstance creates a plugin instance for a user context.
func NewGotifyPluginInstance(ctx plugin.UserContext) plugin.Plugin {
	return theHub.newInstance(ctx)
}

func (h *hub) newInstance(ctx plugin.UserContext) *Instance {
	i := &Instance{h: h, user: ctx}
	h.mu.Lock()
	h.instances[ctx.ID] = i
	h.mu.Unlock()
	return i
}

// Enable enables the plugin for this user.
func (i *Instance) Enable() error {
	i.h.mu.Lock()
	i.enabled = true
	i.h.mu.Unlock()
	i.h.reconcile()
	return nil
}

// Disable disables the plugin for this user: no more pushes to this account.
func (i *Instance) Disable() error {
	i.h.mu.Lock()
	i.enabled = false
	i.h.mu.Unlock()
	i.h.reconcile()
	return nil
}

// SetMessageHandler implements plugin.Messenger.
func (i *Instance) SetMessageHandler(m plugin.MessageHandler) {
	i.h.mu.Lock()
	i.msg = m
	i.h.mu.Unlock()
}

// SetStorageHandler implements plugin.Storager and loads the stored link.
func (i *Instance) SetStorageHandler(s plugin.StorageHandler) {
	i.h.mu.Lock()
	defer i.h.mu.Unlock()
	i.store = s
	raw, err := s.Load()
	if err != nil || len(raw) == 0 {
		return
	}
	var l linkData
	if err := json.Unmarshal(raw, &l); err != nil || l.Phone == "" {
		log.Printf("gotify-phonelink: stored link of %s is unreadable, ignored", i.user.Name)
		return
	}
	i.link = &l
}

// DefaultConfig implements plugin.Configurer: administrators configure the
// plugin, other users have nothing to set.
func (i *Instance) DefaultConfig() interface{} {
	if i.user.Admin {
		return defaultConfig()
	}
	return &UserConfig{}
}

// ValidateAndSetConfig implements plugin.Configurer.
func (i *Instance) ValidateAndSetConfig(c interface{}) error {
	switch cfg := c.(type) {
	case *Config:
		if !i.user.Admin {
			return fmt.Errorf("only administrators configure phonelink")
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		i.h.mu.Lock()
		i.cfg = cfg
		i.h.mu.Unlock()
		i.h.reconcile()
		return nil
	case *UserConfig:
		return nil
	}
	return fmt.Errorf("unexpected configuration type %T", c)
}

// RegisterWebhook implements plugin.Webhooker.
func (i *Instance) RegisterWebhook(basePath string, g *gin.RouterGroup) {
	if !strings.HasPrefix(basePath, "/") {
		basePath = "/" + basePath
	}
	if !strings.HasSuffix(basePath, "/") {
		basePath += "/"
	}
	i.h.mu.Lock()
	i.basePath = basePath
	i.h.mu.Unlock()
	i.routes(g)
}

// GetDisplay implements plugin.Displayer (markdown on the plugin page).
func (i *Instance) GetDisplay(location *url.URL) string {
	h := i.h
	ctrl, cfg := h.control()
	h.mu.Lock()
	link := i.link
	enabled := i.enabled
	pending := h.verifies[i.user.ID] != nil
	h.mu.Unlock()

	var b strings.Builder
	b.WriteString("Messages sent to your phone number arrive in this Gotify account.\n\n")
	switch {
	case cfg == nil || !cfg.oidcConfigured():
		b.WriteString("**Not configured yet** - an administrator has to set it up.\n")
	case !enabled:
		b.WriteString("**Enable the plugin** (switch above) to link your phone number.\n")
	case link != nil:
		fmt.Fprintf(&b, "**Linked:** `%s` (since %s)\n\n", link.Phone, link.LinkedAt.Format("2006-01-02"))
		fmt.Fprintf(&b, "[Link again](%s) · [Unlink](%s)\n", h.publicURL(cfg, i, "link"), h.publicURL(cfg, i, "unlink"))
	default:
		if pending {
			fmt.Fprintf(&b, "A code was sent by SMS: [enter it](%s)\n\n", h.publicURL(cfg, i, "verify"))
		}
		b.WriteString("**No phone number linked.** The number comes from your account at the identity provider")
		if cfg.SMS.VerifyOnLink {
			b.WriteString(" and is confirmed with a code sent by SMS")
		}
		fmt.Fprintf(&b, ".\n\n[Link phone number](%s)\n", h.publicURL(cfg, i, "link"))
	}

	if !i.user.Admin {
		return b.String()
	}
	b.WriteString("\n---\n### Administrator\n")
	if ctrl == nil {
		b.WriteString("Set `public_url`, `oidc` and `sms` in the configuration of an enabled administrator.\n")
		return b.String()
	}
	if ctrl != i {
		fmt.Fprintf(&b, "The configuration of **%s** is in use.\n", ctrl.user.Name)
		return b.String()
	}
	h.mu.Lock()
	linked := 0
	for _, o := range h.instances {
		if o.link != nil {
			linked++
		}
	}
	h.mu.Unlock()
	fmt.Fprintf(&b, "- Redirect URI for the OIDC client: `%s`\n", h.publicURL(cfg, i, "oidc/callback"))
	fmt.Fprintf(&b, "- Send endpoint: `POST %s` (`Authorization: Bearer <sender token>`)\n", h.publicURL(cfg, i, "send"))
	names := make([]string, 0, len(cfg.Senders))
	for _, s := range cfg.Senders {
		names = append(names, s.Name)
	}
	fmt.Fprintf(&b, "- Senders: %s\n", strings.Join(names, ", "))
	fmt.Fprintf(&b, "- Linked users: %d\n", linked)
	h.mu.Lock()
	waiting := 0
	for _, a := range h.approvals {
		if a.decided == "" {
			waiting++
		}
	}
	mods := h.moderatorsLocked(cfg)
	h.mu.Unlock()
	modNames := make([]string, 0, len(mods))
	for _, m := range mods {
		modNames = append(modNames, m.user.Name)
	}
	fmt.Fprintf(&b, "- Moderators: %s; waiting for approval: %d\n", strings.Join(modNames, ", "), waiting)
	mailMu.Lock()
	gate := h.mail
	mailMu.Unlock()
	if gate != nil {
		fmt.Fprintf(&b, "- Mail gate: listening on `%s`, relaying to `%s`\n", gate.cfg.Listen, gate.cfg.Relay)
	} else if cfg.Mail.enabled() {
		fmt.Fprintf(&b, "- Mail gate: **not running** (see the Gotify log)\n")
	}
	return b.String()
}

func main() {
	panic("this should be built as go plugin")
}
