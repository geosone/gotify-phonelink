package main

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="referrer" content="no-referrer"><title>{{.Title}}</title>
<style>
body{font-family:system-ui,sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem;color:#222;background:#fafafa}
h1{font-size:1.3rem} .box{background:#fff;border:1px solid #ddd;border-radius:8px;padding:1rem 1.2rem}
.err{border-color:#d33;color:#a00} pre{white-space:pre-wrap;word-break:break-word;font:inherit;margin:0}
input{font-size:1.4rem;width:8rem;letter-spacing:.2rem} button{font-size:1rem;padding:.4rem 1rem}
@media (prefers-color-scheme: dark){body{background:#1e1e1e;color:#eee}.box{background:#2a2a2a;border-color:#444}}
</style></head><body><h1>{{.Title}}</h1>
<div class="box{{if .Error}} err{{end}}">
{{if .Text}}<pre>{{.Text}}</pre>{{end}}
{{if .Form}}<form method="post" action="{{.Form}}">
{{if .CodeField}}<p><input name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="6" autofocus required></p>{{end}}
<button type="submit">{{.Button}}</button></form>{{end}}
</div>
{{if .Back}}<p><a href="{{.Back}}">Back to Gotify</a></p>{{end}}
</body></html>`))

type pageData struct {
	Title, Text, Form, Button, Back string
	Error, CodeField                bool
}

func render(c *gin.Context, status int, d pageData) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(status)
	if err := page.Execute(c.Writer, d); err != nil {
		log.Printf("gotify-phonelink: render: %v", err)
	}
}

func (i *Instance) back() string {
	_, cfg := i.h.control()
	if cfg == nil {
		return ""
	}
	return strings.TrimSuffix(cfg.PublicURL, "/") + "/#/plugins"
}

func (i *Instance) fail(c *gin.Context, status int, err error) {
	render(c, status, pageData{Title: "Phone link", Text: err.Error(), Error: true, Back: i.back()})
}

func (i *Instance) isControl() bool {
	ctrl, _ := i.h.control()
	return ctrl == i
}

func (i *Instance) routes(g *gin.RouterGroup) {
	// user pages (only reachable while this user's plugin is enabled)
	g.GET("link", i.handleLink)
	g.GET("verify", i.handleVerifyForm)
	g.POST("verify", i.handleVerify)
	g.GET("unlink", i.handleUnlinkForm)
	g.POST("unlink", i.handleUnlink)
	// control instance endpoints
	g.GET("oidc/callback", i.handleCallback)
	g.POST("send", i.handleSend)
	g.GET("m/:msg", i.handleOpen) // not :id - gotify mounts plugins under /plugin/:id/
}

func (i *Instance) handleLink(c *gin.Context) {
	u, err := i.h.startLink(c.Request.Context(), i)
	if err != nil {
		i.fail(c, http.StatusServiceUnavailable, err)
		return
	}
	c.Redirect(http.StatusFound, u)
}

func (i *Instance) handleCallback(c *gin.Context) {
	if !i.isControl() {
		c.Status(http.StatusNotFound)
		return
	}
	if e := c.Query("error"); e != "" {
		i.fail(c, http.StatusBadRequest, errors.New("the identity provider refused: "+e+" "+c.Query("error_description")))
		return
	}
	res, err := i.h.finishLink(c.Request.Context(), c.Query("state"), c.Query("code"))
	if err != nil {
		i.fail(c, http.StatusBadRequest, err)
		return
	}
	_, cfg := i.h.control()
	if res.needsCode {
		c.Redirect(http.StatusFound, i.h.publicURL(cfg, res.inst, "verify"))
		return
	}
	render(c, http.StatusOK, pageData{Title: "Phone number linked",
		Text: "Messages to " + res.phone + " now arrive in your Gotify account.", Back: res.inst.back()})
}

func (i *Instance) handleVerifyForm(c *gin.Context) {
	i.h.mu.Lock()
	v := i.h.verifies[i.user.ID]
	i.h.mu.Unlock()
	if v == nil {
		i.fail(c, http.StatusNotFound, errors.New("no code is pending - link your phone number first"))
		return
	}
	render(c, http.StatusOK, pageData{Title: "Confirm phone number",
		Text: "A code was sent by SMS to " + maskPhone(v.identity.Phone) + ".",
		Form: "verify", Button: "Confirm", CodeField: true})
}

func (i *Instance) handleVerify(c *gin.Context) {
	if err := i.h.verifyCode(i, c.PostForm("code")); err != nil {
		i.h.mu.Lock()
		retry := i.h.verifies[i.user.ID] != nil
		i.h.mu.Unlock()
		if retry {
			render(c, http.StatusBadRequest, pageData{Title: "Confirm phone number", Text: err.Error(),
				Error: true, Form: "verify", Button: "Confirm", CodeField: true})
			return
		}
		i.fail(c, http.StatusBadRequest, err)
		return
	}
	i.h.mu.Lock()
	phone := ""
	if i.link != nil {
		phone = i.link.Phone
	}
	i.h.mu.Unlock()
	render(c, http.StatusOK, pageData{Title: "Phone number linked",
		Text: "Messages to " + phone + " now arrive in your Gotify account.", Back: i.back()})
}

func (i *Instance) handleUnlinkForm(c *gin.Context) {
	i.h.mu.Lock()
	link := i.link
	i.h.mu.Unlock()
	if link == nil {
		i.fail(c, http.StatusNotFound, errors.New("no phone number is linked"))
		return
	}
	render(c, http.StatusOK, pageData{Title: "Unlink phone number",
		Text: "Stop delivering messages for " + link.Phone + " to this account?", Form: "unlink", Button: "Unlink", Back: i.back()})
}

func (i *Instance) handleUnlink(c *gin.Context) {
	i.h.unlink(i, "unlinked by the user")
	render(c, http.StatusOK, pageData{Title: "Phone number unlinked", Text: "Done.", Back: i.back()})
}

func (i *Instance) handleSend(c *gin.Context) {
	ctrl, cfg := i.h.control()
	if ctrl != i {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	if token == "" {
		token = c.GetHeader("X-Phonelink-Token")
	}
	s := i.h.senderByToken(cfg, token)
	if token == "" || s == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unknown sender token"})
		return
	}
	var req sendRequest
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	if err != nil || json.Unmarshal(body, &req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON"})
		return
	}
	res, err := i.h.send(c.Request.Context(), cfg, ctrl, s, &req)
	if err != nil {
		var se *sendError
		if errors.As(err, &se) {
			c.JSON(se.code, gin.H{"error": se.msg})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, res)
}

func (i *Instance) handleOpen(c *gin.Context) {
	if !i.isControl() {
		c.Status(http.StatusNotFound)
		return
	}
	d, ok := i.h.open(c.Param("msg"), c.Query("k"))
	if !ok {
		i.fail(c, http.StatusNotFound, errors.New("this message is unknown or expired"))
		return
	}
	title := d.title
	if title == "" {
		title = "Message"
	}
	note := ""
	if d.smsSent {
		note = "\n\n(It was also sent by SMS because it was not opened in time.)"
	}
	render(c, http.StatusOK, pageData{Title: title, Text: d.message + note})
}
