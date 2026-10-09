package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/access"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// pageData is the data every full page gets: who is logged in, the CSRF
// token, and the menu links this visitor is allowed to open.
func (s *Server) pageData(c *gin.Context, page string, extra gin.H) gin.H {
	user := auth.FromContext(c)
	h := gin.H{
		"Page": page,
		"User": user,
		"CSRF": auth.CSRFToken(c),
		"Nav":  s.guard.Nav(user),
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// denied answers a request the access guard refused, in the form the caller
// expects: a redirect to the login page for a browser page, a login redirect
// header for HTMX, JSON for the API, a "not permitted" page otherwise.
// Refusals of logged-in users are written to the audit log.
func (s *Server) denied(c *gin.Context, d access.Denial) {
	if d.Status == http.StatusForbidden && d.Rule.AuditAction != "" && d.User != nil {
		_ = s.store.RecordAudit(c.Request.Context(), store.AuditEntry{
			Actor:  store.Actor{ID: d.User.ID, Username: d.User.Username, IP: c.ClientIP()},
			Action: d.Rule.AuditAction, TargetType: d.Rule.AuditTarget, TargetID: c.Param("id"),
			Outcome: "denied", Reason: "missing permission " + d.Rule.Permission + " on " + c.Request.Method + " " + c.FullPath(),
		})
	}

	api := strings.HasPrefix(c.Request.URL.Path, "/ams/api/")
	hx := c.GetHeader("HX-Request") != ""

	switch {
	case api && d.NeedLogin:
		c.JSON(http.StatusUnauthorized, gin.H{"error": "login required"})
	case api:
		c.JSON(http.StatusForbidden, gin.H{"error": "not permitted: " + d.Rule.Permission})
	case d.NeedLogin && hx:
		// htmx follows HX-Redirect with a full page load, so a polled fragment
		// never fills with the login page when a session ends.
		c.Header("HX-Redirect", loginURL(hxPath(c)))
		c.String(http.StatusUnauthorized, "login required")
	case d.NeedLogin && c.Request.Method == http.MethodGet:
		c.Redirect(http.StatusFound, loginURL(c.Request.URL.RequestURI()))
	case d.NeedLogin:
		c.String(http.StatusUnauthorized, "login required")
	case hx:
		c.String(http.StatusForbidden, "not permitted: "+d.Rule.Label)
	default:
		data := s.pageData(c, "forbidden", gin.H{"Label": d.Rule.Label, "Permission": d.Rule.Permission})
		c.HTML(http.StatusForbidden, "forbidden.html", data)
	}
}

// hxPath is the page the browser is on when an htmx request is made.
func hxPath(c *gin.Context) string {
	if u, err := url.Parse(c.GetHeader("HX-Current-URL")); err == nil && u.Path != "" {
		return u.RequestURI()
	}
	return "/"
}

func loginURL(next string) string {
	if next = safeNext(next); next == "/" {
		return "/login"
	}
	return "/login?next=" + url.QueryEscape(next)
}

// safeNext returns a local path to go back to after login. Anything that
// could lead to another site (absolute URLs, //host, backslashes) becomes "/".
func safeNext(next string) string {
	if next == "" || len(next) > 500 || !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") ||
		strings.HasPrefix(next, "/login") {
		return "/"
	}
	return next
}
