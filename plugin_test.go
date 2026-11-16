package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gotify/plugin-api"
)

// --- fakes ---------------------------------------------------------------

type fakeStore struct {
	mu   sync.Mutex
	data []byte
}

func (s *fakeStore) Save(b []byte) error { s.mu.Lock(); s.data = b; s.mu.Unlock(); return nil }
func (s *fakeStore) Load() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data, nil
}

type fakeMessenger struct {
	mu   sync.Mutex
	msgs []plugin.Message
}

func (m *fakeMessenger) SendMessage(msg plugin.Message) error {
	m.mu.Lock()
	m.msgs = append(m.msgs, msg)
	m.mu.Unlock()
	return nil
}

func (m *fakeMessenger) all() []plugin.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]plugin.Message(nil), m.msgs...)
}

type fakeSMS struct {
	mu   sync.Mutex
	sent []struct{ to, text string }
	srv  *httptest.Server
}

func newFakeSMS(t *testing.T) *fakeSMS {
	f := &fakeSMS{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		if u != "ma1sd" || p != "pw" || r.URL.Path != "/api/v1.0/sms/outgoing" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			Mobiles []string `json:"mobiles"`
			Text    string   `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sent = append(f.sent, struct{ to, text string }{body.Mobiles[0], body.Text})
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"mobiles": map[string]interface{}{body.Mobiles[0]: map[string]string{"response": "Ok", "message_id": "x"}},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSMS) messages() []struct{ to, text string } {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]struct{ to, text string }(nil), f.sent...)
}

// fakeIDP is a minimal OIDC provider: the next login returns `claims`.
type fakeIDP struct {
	srv          *httptest.Server
	mu           sync.Mutex
	claims       map[string]interface{}
	refreshValid bool
	lastRedirect string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	f := &fakeIDP{refreshValid: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.srv.URL,
			"authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint":         f.srv.URL + "/token",
			"userinfo_endpoint":      f.srv.URL + "/userinfo",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, _ := r.BasicAuth()
		_ = r.ParseForm()
		if id != "phonelink" || secret != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			f.mu.Lock()
			ok := f.refreshValid
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
		} else if r.Form.Get("code") != "good-code" || r.Form.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		f.mu.Lock()
		f.lastRedirect = r.Form.Get("redirect_uri")
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt-new","token_type":"Bearer"}`))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		c := f.claims
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(c)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIDP) set(claims map[string]interface{}) {
	f.mu.Lock()
	f.claims = claims
	f.mu.Unlock()
}

type env struct {
	h     *hub
	sms   *fakeSMS
	idp   *fakeIDP
	admin *Instance
	cfg   *Config
	msgs  map[uint]*fakeMessenger
}

func newEnv(t *testing.T, verify bool) *env {
	gin.SetMode(gin.TestMode)
	e := &env{h: newHub(), sms: newFakeSMS(t), idp: newFakeIDP(t), msgs: map[uint]*fakeMessenger{}}
	e.cfg = defaultConfig()
	e.cfg.PublicURL = "https://gotify.example.org"
	e.cfg.OIDC.Issuer = e.idp.srv.URL
	e.cfg.OIDC.ClientID = "phonelink"
	e.cfg.OIDC.ClientSecret = "s3cret"
	e.cfg.SMS = SMSConfig{URL: e.sms.srv.URL, Username: "ma1sd", Password: "pw", VerifyOnLink: verify}
	e.cfg.Senders = []SenderConfig{
		{Name: "ma1sd", Token: "ma1sd-token-0123456789", Failover: "100ms", FailoverMax: "1s", SMS: true, SMSUnlinked: true, HideContent: true},
		{Name: "zabbix", Token: "zabbix-token-0123456789", Failover: "100ms", SMS: true, SMSMinPriority: 8},
		{Name: "nosms", Token: "nosms-token-0123456789"},
	}
	if err := e.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	e.admin = e.user(t, 1, "admin", true)
	if err := e.admin.ValidateAndSetConfig(e.cfg); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) user(t *testing.T, id uint, name string, admin bool) *Instance {
	i := e.h.newInstance(plugin.UserContext{ID: id, Name: name, Admin: admin})
	m := &fakeMessenger{}
	e.msgs[id] = m
	i.SetMessageHandler(m)
	i.SetStorageHandler(&fakeStore{})
	r := gin.New()
	i.RegisterWebhook("/plugin/"+name+"/custom/tok/", r.Group("/plugin/"+name+"/custom/tok/"))
	if err := i.Enable(); err != nil {
		t.Fatal(err)
	}
	return i
}

// link runs the OIDC link flow for inst with the given provider claims.
func (e *env) link(t *testing.T, inst *Instance, claims map[string]interface{}) (*linkResult, error) {
	e.idp.set(claims)
	u, err := e.h.startLink(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	if pu.Query().Get("code_challenge_method") != "S256" || !strings.Contains(pu.Query().Get("scope"), "phone") {
		t.Fatalf("bad authorize URL %s", u)
	}
	return e.h.finishLink(context.Background(), pu.Query().Get("state"), "good-code")
}

func claims(user, phone string) map[string]interface{} {
	return map[string]interface{}{"sub": "sub-" + user, "preferred_username": user, "phone_number": phone}
}

func (e *env) send(t *testing.T, token string, req sendRequest) (*sendResult, error) {
	s := e.h.senderByToken(e.cfg, token)
	if s == nil {
		t.Fatal("unknown sender")
	}
	return e.h.send(context.Background(), e.cfg, e.admin, s, &req)
}

// --- tests -----------------------------------------------------------------

func TestNormalizePhone(t *testing.T) {
	ok := map[string]string{
		"+43 676 1234567":  "+436761234567",
		"00436761234567":   "+436761234567",
		"436761234567":     "+436761234567",
		"+43 (676) 123-45": "+4367612345",
	}
	for in, want := range ok {
		if got, err := normalizePhone(in); err != nil || got != want {
			t.Errorf("normalizePhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"06761234567", "+43abc123456", "123", "+1234567890123456"} {
		if _, err := normalizePhone(in); err == nil {
			t.Errorf("normalizePhone(%q) accepted", in)
		}
	}
	if m := maskPhone("+436761234567"); m != "+43676****567" {
		t.Errorf("maskPhone = %q", m)
	}
}

func TestConfigValidate(t *testing.T) {
	bad := []func(c *Config){
		func(c *Config) { c.PublicURL = "ftp://x" },
		func(c *Config) { c.OIDC.ClientSecret = "" },
		func(c *Config) { c.Senders[0].Token = "short" },
		func(c *Config) { c.Senders[1].Name = c.Senders[0].Name },
		func(c *Config) { c.Senders[1].Token = c.Senders[0].Token },
		func(c *Config) { c.Senders[0].Failover = "abc" },
		func(c *Config) { c.Senders[0].FailoverMax = "10ms" },
		func(c *Config) { c.Senders[2].Failover = "1m" },
		func(c *Config) { c.SMS.URL = ""; c.SMS.VerifyOnLink = false },
	}
	for n, mod := range bad {
		e := newEnv(t, true)
		mod(e.cfg)
		if err := e.cfg.Validate(); err == nil {
			t.Errorf("case %d: invalid config accepted", n)
		}
	}
	e := newEnv(t, false)
	u := e.user(t, 2, "bob", false)
	if err := u.ValidateAndSetConfig(defaultConfig()); err == nil {
		t.Error("non-admin may set the admin configuration")
	}
	if _, ok := u.DefaultConfig().(*UserConfig); !ok {
		t.Error("non-admin default config is not empty")
	}
}

func TestLinkWithSMSCode(t *testing.T) {
	e := newEnv(t, true)
	bob := e.user(t, 2, "bob", false)
	res, err := e.link(t, bob, claims("bob", "+43 676 1234567"))
	if err != nil || !res.needsCode {
		t.Fatalf("link: %v %+v", err, res)
	}
	if e.idp.lastRedirect != "https://gotify.example.org/plugin/admin/custom/tok/oidc/callback" {
		t.Fatalf("redirect_uri = %q", e.idp.lastRedirect)
	}
	sms := e.sms.messages()
	if len(sms) != 1 || sms[0].to != "436761234567" {
		t.Fatalf("verification SMS: %+v", sms)
	}
	code := strings.TrimPrefix(sms[0].text, "Gotify verification code: ")
	if bob.link != nil {
		t.Fatal("linked before the code was entered")
	}
	if err := e.h.verifyCode(bob, "000000x"); err == nil {
		t.Fatal("wrong code accepted")
	}
	if err := e.h.verifyCode(bob, code); err != nil {
		t.Fatal(err)
	}
	if bob.link == nil || bob.link.Phone != "+436761234567" || bob.link.RefreshToken != "rt-new" {
		t.Fatalf("link = %+v", bob.link)
	}
	var stored linkData
	if err := json.Unmarshal(bob.store.(*fakeStore).data, &stored); err != nil || stored.Phone != "+436761234567" {
		t.Fatalf("stored = %s", bob.store.(*fakeStore).data)
	}
	if n := len(e.msgs[2].all()); n != 1 {
		t.Fatalf("bob got %d messages, want the link notice", n)
	}

	// reload from storage in a fresh instance
	h2 := newHub()
	i2 := h2.newInstance(plugin.UserContext{ID: 2, Name: "bob"})
	i2.SetStorageHandler(bob.store)
	if i2.link == nil || i2.link.Phone != "+436761234567" {
		t.Fatal("link not restored from storage")
	}
}

func TestLinkTooManyWrongCodes(t *testing.T) {
	e := newEnv(t, true)
	bob := e.user(t, 2, "bob", false)
	if _, err := e.link(t, bob, claims("bob", "+436761234567")); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < verifyMaxTries; n++ {
		_ = e.h.verifyCode(bob, "999999x")
	}
	code := strings.TrimPrefix(e.sms.messages()[0].text, "Gotify verification code: ")
	if err := e.h.verifyCode(bob, code); err == nil {
		t.Fatal("code accepted after too many tries")
	}
}

func TestLinkRefusals(t *testing.T) {
	e := newEnv(t, false)
	bob := e.user(t, 2, "bob", false)
	eve := e.user(t, 3, "eve", false)

	// provider account is someone else
	if _, err := e.link(t, eve, claims("bob", "+436761234567")); err == nil {
		t.Fatal("link with a foreign provider account accepted")
	}
	// unverified number without trust or SMS verification
	if _, err := e.link(t, bob, claims("bob", "+436761234567")); err == nil {
		t.Fatal("unverified number accepted")
	}
	e.cfg.OIDC.TrustUnverifiedPhone = true
	_ = e.admin.ValidateAndSetConfig(e.cfg)
	if _, err := e.link(t, bob, claims("bob", "+436761234567")); err != nil {
		t.Fatal(err)
	}
	// the same number for a second account
	if _, err := e.link(t, eve, claims("eve", "+43 676 1234567")); err == nil {
		t.Fatal("duplicate number accepted")
	}
	// no phone number at the provider
	if _, err := e.link(t, eve, map[string]interface{}{"sub": "x", "preferred_username": "eve"}); err == nil {
		t.Fatal("account without phone number accepted")
	}
	// unknown state
	if _, err := e.h.finishLink(context.Background(), "nope", "good-code"); err == nil {
		t.Fatal("unknown state accepted")
	}
}

func linkDirect(t *testing.T, e *env, inst *Instance, phone string) {
	if err := e.h.setLink(inst, &identity{Subject: "sub-" + inst.user.Name, Username: inst.user.Name, Phone: phone}, "rt"); err != nil {
		t.Fatal(err)
	}
}

func TestSendPushAndOpen(t *testing.T) {
	e := newEnv(t, false)
	bob := e.user(t, 2, "bob", false)
	linkDirect(t, e, bob, "+436761234567")
	before := len(e.msgs[2].all())

	res, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "436761234567", Message: "Your code is 123456"})
	if err != nil || res.Status != "push" || res.FailoverIn != "100ms" {
		t.Fatalf("send: %+v %v", res, err)
	}
	msgs := e.msgs[2].all()
	if len(msgs) != before+1 {
		t.Fatal("no push")
	}
	m := msgs[len(msgs)-1]
	if !strings.HasPrefix(m.Message, hiddenPlaceholder) || !strings.Contains(m.Message, "[Confirm](https://") || strings.Contains(m.Message, "123456") {
		t.Fatalf("content not hidden: %q", m.Message)
	}
	click := m.Extras["client::notification"].(map[string]interface{})["click"].(map[string]interface{})["url"].(string)
	u, _ := url.Parse(click)
	id := strings.TrimPrefix(u.Path, "/plugin/admin/custom/tok/m/")
	if _, ok := e.h.open(id, "wrong"); ok {
		t.Fatal("opened with a wrong key")
	}
	d, ok := e.h.open(id, u.Query().Get("k"))
	if !ok || d.message != "Your code is 123456" {
		t.Fatalf("open: %+v %v", d, ok)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(e.sms.messages()); n != 0 {
		t.Fatalf("%d SMS sent although the message was opened", n)
	}
}

func TestSendFailover(t *testing.T) {
	e := newEnv(t, false)
	bob := e.user(t, 2, "bob", false)
	linkDirect(t, e, bob, "+436761234567")
	if _, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436761234567", Title: "Matrix", Message: "Code 42"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	sms := e.sms.messages()
	if len(sms) != 1 || sms[0].to != "436761234567" || sms[0].text != "Matrix\nCode 42" {
		t.Fatalf("failover SMS: %+v", sms)
	}
}

func TestSendFailoverPerRequest(t *testing.T) {
	e := newEnv(t, false)
	bob := e.user(t, 2, "bob", false)
	linkDirect(t, e, bob, "+436761234567")
	res, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436761234567", Message: "x", Failover: "1h"})
	if err != nil || res.FailoverIn != "1s" {
		t.Fatalf("failover not capped at failover_max: %+v %v", res, err)
	}
	res, err = e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436761234567", Message: "x", Failover: float64(0)})
	if err != nil || res.FailoverIn != "" {
		t.Fatalf("failover 0 not honoured: %+v %v", res, err)
	}
}

func TestSendPriorityAndNoSMS(t *testing.T) {
	e := newEnv(t, false)
	bob := e.user(t, 2, "bob", false)
	linkDirect(t, e, bob, "+436761234567")
	low := 5
	res, err := e.send(t, "zabbix-token-0123456789", sendRequest{To: "+436761234567", Message: "disk 80%", Priority: &low})
	if err != nil || res.FailoverIn != "" {
		t.Fatalf("low priority must not fail over: %+v %v", res, err)
	}
	m := e.msgs[2].all()
	if last := m[len(m)-1]; last.Message != "disk 80%" || last.Extras != nil {
		t.Fatalf("plain push expected: %+v", last)
	}
	high := 9
	res, err = e.send(t, "zabbix-token-0123456789", sendRequest{User: "bob", Message: "host down", Priority: &high})
	if err != nil || res.FailoverIn != "100ms" {
		t.Fatalf("high priority to user: %+v %v", res, err)
	}
	if _, err := e.send(t, "nosms-token-0123456789", sendRequest{To: "+439999999999", Message: "x"}); err == nil {
		t.Fatal("unlinked number accepted by a sender without SMS")
	}
}

func TestSendUnlinkedBySMS(t *testing.T) {
	e := newEnv(t, false)
	res, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "436769999999", Message: "Code 7"})
	if err != nil || res.Status != "sms" {
		t.Fatalf("send: %+v %v", res, err)
	}
	if sms := e.sms.messages(); len(sms) != 1 || sms[0].to != "436769999999" {
		t.Fatalf("SMS: %+v", sms)
	}
	// disabled plugin = opt-out: SMS instead of push
	bob := e.user(t, 2, "bob", false)
	linkDirect(t, e, bob, "+436761234567")
	_ = bob.Disable()
	res, err = e.send(t, "ma1sd-token-0123456789", sendRequest{To: "436761234567", Message: "Code 8"})
	if err != nil || res.Status != "sms" {
		t.Fatalf("disabled user: %+v %v", res, err)
	}
}

func TestSendDuplicateStoredLink(t *testing.T) {
	e := newEnv(t, false)
	bob := e.user(t, 2, "bob", false)
	eve := e.user(t, 3, "eve", false)
	linkDirect(t, e, bob, "+436761234567")
	linkDirect(t, e, eve, "+436761234567") // e.g. restored from storage
	_, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436761234567", Message: "x"})
	var se *sendError
	if err == nil || !errorsAs(err, &se) || se.code != http.StatusConflict {
		t.Fatalf("duplicate link delivered: %v", err)
	}
}

func errorsAs(err error, target **sendError) bool {
	se, ok := err.(*sendError)
	if ok {
		*target = se
	}
	return ok
}

func TestRecheck(t *testing.T) {
	e := newEnv(t, false)
	e.cfg.OIDC.TrustUnverifiedPhone = true
	bob := e.user(t, 2, "bob", false)
	linkDirect(t, e, bob, "+436761234567")

	e.idp.set(claims("bob", "+436761234567"))
	e.h.recheck(e.cfg, bob)
	if bob.link == nil || bob.link.RefreshToken != "rt-new" {
		t.Fatalf("unchanged link removed or token not rotated: %+v", bob.link)
	}
	e.idp.set(claims("bob", "+436760000000"))
	e.h.recheck(e.cfg, bob)
	if bob.link != nil {
		t.Fatal("changed number still linked")
	}

	linkDirect(t, e, bob, "+436761234567")
	e.idp.mu.Lock()
	e.idp.refreshValid = false
	e.idp.mu.Unlock()
	e.h.recheck(e.cfg, bob)
	if bob.link != nil {
		t.Fatal("link kept after invalid_grant")
	}
}

func TestSendHTTP(t *testing.T) {
	e := newEnv(t, false)
	bob := e.user(t, 2, "bob", false)
	linkDirect(t, e, bob, "+436761234567")
	r := gin.New()
	e.admin.routes(r.Group("/p/"))
	post := func(tok, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/p/send", bytes.NewBufferString(body))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		r.ServeHTTP(w, req)
		return w
	}
	if w := post("", `{"to":"436761234567","message":"x"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
	if w := post("wrong-token-0123456789", `{"to":"436761234567","message":"x"}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", w.Code)
	}
	if w := post("nosms-token-0123456789", `nope`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON: %d", w.Code)
	}
	w := post("nosms-token-0123456789", `{"to":"436761234567","title":"T","message":"hello"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"push"`) {
		t.Fatalf("send: %d %s", w.Code, w.Body)
	}
}

func TestMarkdownEscape(t *testing.T) {
	if got := markdownEscape("a *b* [c](d)\nx"); got != `a \*b\* \[c\]\(d\)  `+"\n"+`x` {
		t.Fatalf("markdownEscape = %q", got)
	}
}

func TestApprovalSMS(t *testing.T) {
	e := newEnv(t, false)
	e.cfg.Senders[0].Approval = true
	e.cfg.SMS.AllowedPrefixes = []string{"+43"}
	if err := e.admin.ValidateAndSetConfig(e.cfg); err != nil {
		t.Fatal(err)
	}
	// not a moderator: a normal user
	e.user(t, 2, "bob", false)
	before := len(e.msgs[1].all())

	res, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436769999999", Message: "Code 1"})
	if err != nil || res.Status != "pending" {
		t.Fatalf("send: %+v %v", res, err)
	}
	if n := len(e.sms.messages()); n != 0 {
		t.Fatalf("%d SMS sent before approval", n)
	}
	if len(e.msgs[2].all()) != 0 {
		t.Fatal("a non-moderator got the approval request")
	}
	m := e.msgs[1].all()
	if len(m) != before+1 {
		t.Fatal("moderator not notified")
	}
	click := m[len(m)-1].Extras["client::notification"].(map[string]interface{})["click"].(map[string]interface{})["url"].(string)
	u, _ := url.Parse(click)
	id := strings.TrimPrefix(u.Path, "/plugin/admin/custom/tok/approve/")

	// a newer code replaces the pending one without a second notification
	res2, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436769999999", Message: "Code 2"})
	if err != nil || res2.ID != res.ID || len(e.msgs[1].all()) != before+1 {
		t.Fatalf("replace: %+v %v", res2, err)
	}

	if _, err := e.h.decide(context.Background(), id, "admin", "wrong", true); err == nil {
		t.Fatal("decided with a wrong key")
	}
	if _, ok := e.h.reviewable(id, "bob", u.Query().Get("k")); ok {
		t.Fatal("another user may review with the moderator's key")
	}
	v, err := e.h.decide(context.Background(), id, "admin", u.Query().Get("k"), true)
	if err != nil || v.Decided != "approved" || v.DecidedBy != "admin" {
		t.Fatalf("decide: %+v %v", v, err)
	}
	if sms := e.sms.messages(); len(sms) != 1 || sms[0].text != "Code 2" {
		t.Fatalf("approved SMS: %+v", sms)
	}
	// deciding again does not send again
	_, _ = e.h.decide(context.Background(), id, "admin", u.Query().Get("k"), true)
	if n := len(e.sms.messages()); n != 1 {
		t.Fatalf("%d SMS after a second approval", n)
	}

	// rejected: nothing is sent
	res, _ = e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436768888888", Message: "Code 3"})
	m = e.msgs[1].all()
	u, _ = url.Parse(m[len(m)-1].Extras["client::notification"].(map[string]interface{})["click"].(map[string]interface{})["url"].(string))
	if v, err := e.h.decide(context.Background(), res.ID, "admin", u.Query().Get("k"), false); err != nil || v.Decided != "rejected" {
		t.Fatalf("reject: %+v %v", v, err)
	}
	if n := len(e.sms.messages()); n != 1 {
		t.Fatal("SMS sent after rejection")
	}

	// a logged-in user asked (requester set): no approval needed
	res, err = e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436767777777", Message: "Code 4", Requester: "@bob:example.org"})
	if err != nil || res.Status != "sms" || len(e.sms.messages()) != 2 {
		t.Fatalf("requester: %+v %v", res, err)
	}

	// country not allowed: refused at once, no moderator bothered
	n := len(e.msgs[1].all())
	if _, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+4915112345678", Message: "x"}); err == nil {
		t.Fatal("SMS to a foreign number accepted")
	}
	if len(e.msgs[1].all()) != n {
		t.Fatal("moderator notified for a refused number")
	}
}

func TestApprovalExpires(t *testing.T) {
	e := newEnv(t, false)
	e.cfg.Senders[0].Approval = true
	e.cfg.Senders[0].ApprovalTimeout = "100ms"
	_ = e.admin.ValidateAndSetConfig(e.cfg)
	res, err := e.send(t, "ma1sd-token-0123456789", sendRequest{To: "+436769999999", Message: "x"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	m := e.msgs[1].all()
	u, _ := url.Parse(m[len(m)-1].Extras["client::notification"].(map[string]interface{})["click"].(map[string]interface{})["url"].(string))
	v, err := e.h.decide(context.Background(), res.ID, "admin", u.Query().Get("k"), true)
	if err != nil || v.Decided != "expired" || len(e.sms.messages()) != 0 {
		t.Fatalf("expired request delivered: %+v %v", v, err)
	}
}
