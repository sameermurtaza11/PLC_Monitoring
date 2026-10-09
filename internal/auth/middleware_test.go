package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// router with a fixed user injected, a protected GET and a CSRF-protected POST.
func testRouter(user *User, denied *int) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if user != nil {
			c.Set(userKey, user)
		}
	})
	onDeny := func(*gin.Context, *User) { *denied++ }
	r.GET("/secret", RequireWith("alarm.config", onDeny), func(c *gin.Context) { c.String(200, "ok") })
	r.POST("/act", RequireWith("alarm.ack", onDeny), RequireCSRF(), func(c *gin.Context) { c.String(200, "done") })
	return r
}

func do(r *gin.Engine, method, path string, form url.Values, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: CookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRequireAnonymousIsRedirectedOrRefused(t *testing.T) {
	var denied int
	r := testRouter(nil, &denied)
	if w := do(r, "GET", "/secret", nil, ""); w.Code != http.StatusFound || w.Header().Get("Location") != "/login" {
		t.Errorf("page: got %d → %q, want redirect to /login", w.Code, w.Header().Get("Location"))
	}
	if w := do(r, "POST", "/act", nil, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("POST: got %d, want 401", w.Code)
	}
}

func TestRequireRefusesMissingPermissionAndReportsIt(t *testing.T) {
	var denied int
	r := testRouter(NewTestUser(1, "olga", "operator", "alarm.view"), &denied)
	if w := do(r, "GET", "/secret", nil, ""); w.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", w.Code)
	}
	if denied != 1 {
		t.Errorf("onDeny called %d times, want 1", denied)
	}
}

func TestRequireAllowsPermittedUser(t *testing.T) {
	var denied int
	r := testRouter(NewTestUser(1, "eve", "engineer", "alarm.config"), &denied)
	if w := do(r, "GET", "/secret", nil, ""); w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}

func TestCSRFRejectsMissingWrongAndAcceptsRightToken(t *testing.T) {
	var denied int
	r := testRouter(NewTestUser(1, "olga", "operator", "alarm.ack"), &denied)
	const session = "session-cookie-value"

	if w := do(r, "POST", "/act", url.Values{}, session); w.Code != http.StatusForbidden {
		t.Errorf("no token: got %d, want 403", w.Code)
	}
	if w := do(r, "POST", "/act", url.Values{"csrf_token": {"forged"}}, session); w.Code != http.StatusForbidden {
		t.Errorf("wrong token: got %d, want 403", w.Code)
	}
	if w := do(r, "POST", "/act", url.Values{"csrf_token": {"x"}}, ""); w.Code != http.StatusForbidden {
		t.Errorf("no session: got %d, want 403", w.Code)
	}

	// the token a page would embed for this session
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/", nil)
	c.Request.AddCookie(&http.Cookie{Name: CookieName, Value: session})
	token := CSRFToken(c)
	if token == "" {
		t.Fatal("no token derived")
	}
	if w := do(r, "POST", "/act", url.Values{"csrf_token": {token}}, session); w.Code != http.StatusOK {
		t.Errorf("right token: got %d, want 200", w.Code)
	}
	// a token for one session is useless with another
	if w := do(r, "POST", "/act", url.Values{"csrf_token": {token}}, "another-session"); w.Code != http.StatusForbidden {
		t.Errorf("token from another session: got %d, want 403", w.Code)
	}
}
