package web

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/access"
)

func init() { gin.SetMode(gin.TestMode) }

// The access guard closes any route that has no rule. This test makes sure
// that never happens by accident: every route the application registers must
// be listed in access.Rules, and every rule must belong to a real route.
func TestEveryRouteHasAnAccessRule(t *testing.T) {
	r := NewRouter(nil, func() {}) // handlers are never called here; this also proves all templates parse

	registered := map[string]bool{}
	for _, ri := range r.Routes() {
		key := ri.Method + " " + ri.Path
		registered[key] = true
		if _, ok := access.Rules[key]; !ok {
			t.Errorf("route %q has no access rule — add it to access.Rules (an unlisted route is closed to non-administrators)", key)
		}
	}

	var stale []string
	for key := range access.Rules {
		if !registered[key] {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	for _, key := range stale {
		t.Errorf("access rule %q matches no route (renamed or removed?)", key)
	}
}

// With nothing set up (no database), protected pages must refuse an anonymous
// visitor instead of failing open.
func TestProtectedPagesRefuseAnonymousVisitorsWhenSettingsAreUnavailable(t *testing.T) {
	r := NewRouter(nil, func() {})
	for _, path := range []string{"/", "/config", "/pv/1", "/ams/config", "/admin/access", "/ams/api/alarms"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		switch path {
		case "/ams/api/alarms":
			if w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s: %d, want 401", path, w.Code)
			}
		default:
			if w.Code != http.StatusFound || w.Header().Get("Location") == "" {
				t.Errorf("GET %s: %d → %q, want a redirect to the login page", path, w.Code, w.Header().Get("Location"))
			}
		}
	}
}

func TestPublicRoutesStayOpen(t *testing.T) {
	r := NewRouter(nil, func() {})
	for _, path := range []string{"/healthz", "/login", "/static/app.css"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: %d, want 200", path, w.Code)
		}
	}
}

func TestStateChangingRequestsAreRefusedWithoutALogin(t *testing.T) {
	r := NewRouter(nil, func() {})
	for _, c := range []struct{ method, path string }{
		{"POST", "/config/plcs"}, {"DELETE", "/config/pvs/3"}, {"POST", "/ams/config"},
		{"POST", "/ams/config/1/review"}, {"POST", "/ams/api/alarms/1/ack"}, {"POST", "/admin/access"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestHTMXRequestsGetALoginRedirectHeaderNotAPageFragment(t *testing.T) {
	r := NewRouter(nil, func() {})
	req := httptest.NewRequest(http.MethodGet, "/partials/pv-rows", nil)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Current-URL", "http://localhost:8080/config")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
	if got := w.Header().Get("HX-Redirect"); got != "/login?next=%2Fconfig" {
		t.Errorf("HX-Redirect = %q, want a redirect to the login page that returns to /config", got)
	}
}

func TestSafeNextOnlyAllowsLocalPaths(t *testing.T) {
	cases := map[string]string{
		"":                       "/",
		"/config":                "/config",
		"/pv/3?range=1h":         "/pv/3?range=1h",
		"https://evil.example/x": "/",
		"//evil.example/x":       "/",
		"/\\evil.example":        "/",
		"javascript:alert(1)":    "/",
		"config":                 "/",
		"/login":                 "/", // would loop
		"/login?next=/x":         "/",
		"/ok\r\nSet-Cookie: a=b": "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
	if got := loginURL("/config?x=1"); got != "/login?next=%2Fconfig%3Fx%3D1" {
		t.Errorf("loginURL = %q", got)
	}
	if got := loginURL("/"); got != "/login" {
		t.Errorf("loginURL(/) = %q, want a plain /login", got)
	}
}
