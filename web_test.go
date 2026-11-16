package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The plugin group is mounted under gotify's own :id parameter.
func TestOpenHTTP(t *testing.T) {
	e := newEnv(t, false)
	bob := e.user(t, 2, "bob", false)
	linkDirect(t, e, bob, "+436761234567")
	r := gin.New()
	e.admin.routes(r.Group("/plugin/:id/custom/tok/"))
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/plugin/1/custom/tok/send", bytes.NewBufferString(`{"to":"436761234567","message":"hi"}`))
	req.Header.Set("Authorization", "Bearer ma1sd-token-0123456789")
	r.ServeHTTP(w, req)
	m := e.msgs[2].all()
	click := m[len(m)-1].Extras["client::notification"].(map[string]interface{})["click"].(map[string]interface{})["url"].(string)
	u, _ := url.Parse(click)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hi") {
		t.Fatalf("%s -> %d %s", u.RequestURI(), w.Code, w.Body)
	}
}
