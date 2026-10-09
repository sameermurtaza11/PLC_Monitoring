package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	CookieName = "plc_session"
	userKey    = "auth.user"
)

// Load attaches the logged-in user (if any) to the request. It never blocks
// a request: pages that need a login use Require.
func Load(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token, err := c.Cookie(CookieName); err == nil {
			if u, err := svc.Authenticate(c.Request.Context(), token); err == nil {
				c.Set(userKey, u)
			}
		}
		c.Next()
	}
}

// FromContext returns the logged-in user, or nil.
func FromContext(c *gin.Context) *User {
	u, _ := c.Get(userKey)
	user, _ := u.(*User)
	return user
}

// Require rejects the request unless the user is logged in and holds the
// permission. The check runs on the server for every request; hiding a
// button in the page is never the only protection.
func Require(permission string) gin.HandlerFunc { return RequireWith(permission, nil) }

// RequireWith is Require, and calls onDeny (if set) when a logged-in user is
// refused, so the refusal can be written to the audit log.
func RequireWith(permission string, onDeny func(c *gin.Context, u *User)) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := FromContext(c)
		switch {
		case u == nil:
			if c.Request.Method == http.MethodGet && c.GetHeader("HX-Request") == "" {
				c.Redirect(http.StatusFound, "/login")
			} else {
				c.String(http.StatusUnauthorized, "login required")
			}
			c.Abort()
		case !u.Can(permission):
			if onDeny != nil {
				onDeny(c, u)
			}
			c.String(http.StatusForbidden, "not permitted: "+permission)
			c.Abort()
		}
	}
}

// CSRFToken is the token a form must send back. It is derived from the
// session cookie, so another site (which cannot read the cookie) cannot
// forge it.
func CSRFToken(c *gin.Context) string {
	cookie, err := c.Cookie(CookieName)
	if err != nil || cookie == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("csrf:" + cookie))
	return hex.EncodeToString(sum[:])
}

// RequireCSRF rejects state-changing requests without the right token
// (form field "csrf_token" or header "X-CSRF-Token").
func RequireCSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			return
		}
		want := CSRFToken(c)
		got := c.PostForm("csrf_token")
		if got == "" {
			got = c.GetHeader("X-CSRF-Token")
		}
		if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			c.String(http.StatusForbidden, "invalid or missing CSRF token — reload the page and try again")
			c.Abort()
		}
	}
}

// SetCookie stores the session token. HttpOnly keeps it away from page
// scripts; SameSite=Lax stops other sites from sending it on a POST.
// (Set Secure once the app is served over HTTPS.)
func SetCookie(c *gin.Context, token string, expires time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: CookieName, Value: token, Path: "/", Expires: expires,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func ClearCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}
