package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A small OpenID Connect relying party: authorization code flow with PKCE,
// client_secret_basic, claims from the userinfo endpoint (fetched over TLS
// with the access token, so the ID token is not needed), refresh tokens.

type oidcMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

type oidcTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
}

// errInvalidGrant: the provider rejected the refresh token (revoked, expired,
// user disabled) - the link must be removed.
var errInvalidGrant = errors.New("invalid_grant")

type oidcClient struct {
	http *http.Client

	mu       sync.Mutex
	issuer   string
	meta     *oidcMetadata
	metaTime time.Time
}

func newOIDCClient(c *http.Client) *oidcClient {
	return &oidcClient{http: c}
}

func (o *oidcClient) metadata(ctx context.Context, issuer string) (*oidcMetadata, error) {
	o.mu.Lock()
	if o.meta != nil && o.issuer == issuer && time.Since(o.metaTime) < time.Hour {
		m := o.meta
		o.mu.Unlock()
		return m, nil
	}
	o.mu.Unlock()

	u := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc discovery: HTTP %d", res.StatusCode)
	}
	var m oidcMetadata
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if strings.TrimSuffix(m.Issuer, "/") != strings.TrimSuffix(issuer, "/") {
		return nil, fmt.Errorf("oidc discovery: issuer %q does not match %q", m.Issuer, issuer)
	}
	if m.AuthorizationEndpoint == "" || m.TokenEndpoint == "" || m.UserinfoEndpoint == "" {
		return nil, errors.New("oidc discovery: endpoints missing")
	}
	o.mu.Lock()
	o.issuer, o.meta, o.metaTime = issuer, &m, time.Now()
	o.mu.Unlock()
	return &m, nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (o *oidcClient) authURL(ctx context.Context, cfg *OIDCConfig, redirectURI, state, verifier string) (string, error) {
	m, err := o.metadata(ctx, cfg.Issuer)
	if err != nil {
		return "", err
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = defaultConfig().OIDC.Scopes
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {cfg.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(m.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return m.AuthorizationEndpoint + sep + q.Encode(), nil
}

func (o *oidcClient) token(ctx context.Context, cfg *OIDCConfig, form url.Values) (*oidcTokens, error) {
	m, err := o.metadata(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// RFC 6749 2.3.1: form-encode the credentials before basic auth
	req.SetBasicAuth(url.QueryEscape(cfg.ClientID), url.QueryEscape(cfg.ClientSecret))
	res, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc token: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "invalid_grant" {
			return nil, errInvalidGrant
		}
		return nil, fmt.Errorf("oidc token: HTTP %d %s", res.StatusCode, e.Error)
	}
	var t oidcTokens
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, fmt.Errorf("oidc token: %w", err)
	}
	if t.AccessToken == "" {
		return nil, errors.New("oidc token: no access_token")
	}
	return &t, nil
}

func (o *oidcClient) exchange(ctx context.Context, cfg *OIDCConfig, code, redirectURI, verifier string) (*oidcTokens, error) {
	return o.token(ctx, cfg, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}

func (o *oidcClient) refresh(ctx context.Context, cfg *OIDCConfig, refreshToken string) (*oidcTokens, error) {
	return o.token(ctx, cfg, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
}

func (o *oidcClient) userinfo(ctx context.Context, cfg *OIDCConfig, accessToken string) (map[string]interface{}, error) {
	m, err := o.metadata(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.UserinfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	res, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc userinfo: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc userinfo: HTTP %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return nil, fmt.Errorf("oidc userinfo: content type %q (signed userinfo is not supported)", ct)
	}
	claims := map[string]interface{}{}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&claims); err != nil {
		return nil, fmt.Errorf("oidc userinfo: %w", err)
	}
	return claims, nil
}

// identity is what the plugin takes from the provider.
type identity struct {
	Subject       string
	Username      string
	Phone         string // E.164
	PhoneVerified bool
}

func claimString(c map[string]interface{}, k string) string {
	if v, ok := c[k].(string); ok {
		return v
	}
	return ""
}

func identityFromClaims(c map[string]interface{}, usernameClaim string) (*identity, error) {
	id := &identity{
		Subject:  claimString(c, "sub"),
		Username: claimString(c, usernameClaim),
	}
	if id.Subject == "" {
		return nil, errors.New("the provider sent no subject")
	}
	if id.Username == "" {
		return nil, fmt.Errorf("the provider sent no %s claim", usernameClaim)
	}
	raw := claimString(c, "phone_number")
	if raw == "" {
		return nil, errors.New("the provider has no phone number for this account (scope phone, claim phone_number)")
	}
	p, err := normalizePhone(raw)
	if err != nil {
		return nil, err
	}
	id.Phone = p
	id.PhoneVerified, _ = c["phone_number_verified"].(bool)
	return id, nil
}
