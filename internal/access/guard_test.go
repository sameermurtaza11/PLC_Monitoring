package access

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/auth"
)

func init() { gin.SetMode(gin.TestMode) }

// flagSet is a PageFlags with fixed values; pages not listed need a login.
type flagSet map[string]bool

func (f flagSet) RequiresLogin(key string) bool {
	if v, ok := f[key]; ok {
		return v
	}
	return true
}

func user(role string, perms ...string) *auth.User {
	return auth.NewTestUser(1, "u_"+role, role, perms...)
}

func decide(flags flagSet, rule Rule, u *auth.User) *Denial {
	return NewGuard(flags, nil).Decide(rule, u)
}

func TestPublicRoutesAreAlwaysOpen(t *testing.T) {
	if d := decide(flagSet{}, pubRule, nil); d != nil {
		t.Errorf("login/health/static must be open: %+v", d)
	}
}

func TestProtectedPageNeedsLogin(t *testing.T) {
	d := decide(flagSet{"dashboard": true}, dashboard, nil)
	if d == nil || d.Status != http.StatusUnauthorized || !d.NeedLogin {
		t.Fatalf("anonymous on a protected page: %+v, want 401", d)
	}
}

func TestPageWithAuthenticationOffOpensWithoutLogin(t *testing.T) {
	if d := decide(flagSet{"dashboard": false}, dashboard, nil); d != nil {
		t.Errorf("a page with authentication = false must open for anyone: %+v", d)
	}
	// …and a logged-in user without the feature is not stopped either: the page is open.
	if d := decide(flagSet{"dashboard": false}, dashboard, user("viewer")); d != nil {
		t.Errorf("open page refused a logged-in user: %+v", d)
	}
}

func TestAuthorizationIsPerFeaturePerRole(t *testing.T) {
	flags := flagSet{"dashboard": true}
	if d := decide(flags, dashboard, user("viewer", "dashboard.view")); d != nil {
		t.Errorf("role with the feature refused: %+v", d)
	}
	d := decide(flags, dashboard, user("viewer", "pv.view")) // has another feature, not this one
	if d == nil || d.Status != http.StatusForbidden || d.NeedLogin {
		t.Fatalf("role without the feature: %+v, want 403", d)
	}
}

func TestActionsAlwaysNeedALoginEvenOnAnOpenPage(t *testing.T) {
	flags := flagSet{"plc_config": false} // the configuration page is open to everyone…
	if d := decide(flags, cfgView, nil); d != nil {
		t.Errorf("viewing the open page should work anonymously: %+v", d)
	}
	d := decide(flags, cfgEdit, nil) // …but changing it needs a person
	if d == nil || d.Status != http.StatusUnauthorized {
		t.Fatalf("anonymous edit: %+v, want 401", d)
	}
	if d := decide(flags, cfgEdit, user("viewer", "plc.config.view")); d == nil || d.Status != http.StatusForbidden {
		t.Errorf("edit without plc.config.edit: %+v, want 403", d)
	}
	if d := decide(flags, cfgEdit, user("engineer", "plc.config.edit")); d != nil {
		t.Errorf("edit with plc.config.edit refused: %+v", d)
	}
}

func TestAccessSettingsCannotBeOpenedToAnonymousUsers(t *testing.T) {
	// whatever the switches say, even with every page switched off
	flags := flagSet{"dashboard": false, "pv_detail": false, "plc_config": false, "alarm_config": false, "alarm_overview": false}
	if d := decide(flags, accessAdmin, nil); d == nil || d.Status != http.StatusUnauthorized {
		t.Errorf("anonymous on the Access page: %+v, want 401", d)
	}
	if d := decide(flags, accessAdmin, user("engineer", "plc.config.edit")); d == nil || d.Status != http.StatusForbidden {
		t.Errorf("non-admin on the Access page: %+v, want 403", d)
	}
	if d := decide(flags, accessSave, user("admin", "access.manage")); d != nil {
		t.Errorf("admin refused: %+v", d)
	}
}

func TestUnlistedRouteIsClosedToEveryoneButAdministrators(t *testing.T) {
	g := NewGuard(flagSet{}, nil)
	rule := g.RuleFor("GET", "/something/new")
	if d := g.Decide(rule, nil); d == nil || d.Status != http.StatusUnauthorized {
		t.Errorf("anonymous: %+v, want 401", d)
	}
	if d := g.Decide(rule, user("supervisor", "alarm.approve")); d == nil || d.Status != http.StatusForbidden {
		t.Errorf("ordinary role: %+v, want 403", d)
	}
	if d := g.Decide(rule, user("admin", "users.manage")); d != nil {
		t.Errorf("administrator refused: %+v", d)
	}
}

func TestNavShowsOnlyWhatTheVisitorCanOpen(t *testing.T) {
	g := NewGuard(flagSet{"dashboard": true, "plc_config": true, "alarm_config": true}, nil)
	labels := func(u *auth.User) []string {
		var out []string
		for _, it := range g.Nav(u) {
			out = append(out, it.Label)
		}
		return out
	}
	if got := labels(nil); len(got) != 0 {
		t.Errorf("anonymous sees %v, want no links (every page needs a login)", got)
	}
	viewer := user("viewer", "dashboard.view", "alarm.view")
	if got := labels(viewer); len(got) != 2 || got[0] != "Dashboard" || got[1] != "Alarm Config" {
		t.Errorf("viewer sees %v", got)
	}
	admin := user("admin", "dashboard.view", "plc.config.view", "alarm.view", "access.manage")
	if got := labels(admin); len(got) != 4 {
		t.Errorf("admin sees %v, want all four", got)
	}
}

// --- middleware wiring ---------------------------------------------------

func testEngine(flags flagSet, u *auth.User, denied *[]Denial) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if u != nil {
			c.Set("auth.user", u)
		}
	})
	g := NewGuard(flags, func(c *gin.Context, d Denial) {
		*denied = append(*denied, d)
		c.String(d.Status, "denied")
	})
	g.Rules = map[string]Rule{"GET /secret": dashboard, "POST /act": cfgEdit}
	r.Use(g.Middleware())
	ok := func(c *gin.Context) { c.String(200, "ok") }
	r.GET("/secret", ok)
	r.POST("/act", ok)
	r.GET("/unlisted", ok)
	return r
}

func TestMiddlewareBlocksHandlersAndCallsOnDeny(t *testing.T) {
	var denied []Denial
	r := testEngine(flagSet{"dashboard": true, "plc_config": true}, nil, &denied)

	for _, c := range []struct{ method, path string }{{"GET", "/secret"}, {"POST", "/act"}, {"GET", "/unlisted"}} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		if w.Code != http.StatusUnauthorized || w.Body.String() != "denied" {
			t.Errorf("%s %s: %d %q, want 401 from OnDeny (handler must not run)", c.method, c.path, w.Code, w.Body.String())
		}
	}
	if len(denied) != 3 {
		t.Errorf("OnDeny called %d times, want 3", len(denied))
	}
}

func TestMiddlewareLetsPermittedUsersThrough(t *testing.T) {
	var denied []Denial
	r := testEngine(flagSet{"dashboard": true}, user("viewer", "dashboard.view"), &denied)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/secret", nil))
	if w.Code != 200 || len(denied) != 0 {
		t.Errorf("got %d, denied=%v", w.Code, denied)
	}
}

func TestMiddlewareLeavesUnknownURLsTo404(t *testing.T) {
	var denied []Denial
	r := testEngine(flagSet{}, nil, &denied)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/nowhere", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", w.Code)
	}
}

// --- page cache ----------------------------------------------------------

func TestPageCacheFailsClosedUntilItHasData(t *testing.T) {
	c := NewPageCache(func() (map[string]bool, error) { return nil, errors.New("database down") }, time.Hour)
	if !c.RequiresLogin("dashboard") {
		t.Error("with no data every page must require a login")
	}
}

func TestPageCacheFailsClosedWhenTheLoaderPanics(t *testing.T) {
	c := NewPageCache(func() (map[string]bool, error) { panic("boom") }, time.Hour)
	if !c.RequiresLogin("dashboard") {
		t.Error("a panicking loader must leave pages protected")
	}
}

func TestPageCacheKeepsLastGoodValuesWhenTheDatabaseFails(t *testing.T) {
	var fail atomic.Bool
	c := NewPageCache(func() (map[string]bool, error) {
		if fail.Load() {
			return nil, errors.New("database down")
		}
		return map[string]bool{"dashboard": false}, nil
	}, time.Nanosecond)

	if c.RequiresLogin("dashboard") {
		t.Fatal("dashboard is switched off and should be open")
	}
	fail.Store(true)
	time.Sleep(2 * time.Millisecond)
	if c.RequiresLogin("dashboard") {
		t.Error("a database failure must not change the last known setting")
	}
	if !c.RequiresLogin("not_a_page") {
		t.Error("an unknown page must require a login")
	}
}

func TestPageCacheReloadPicksUpChangesImmediately(t *testing.T) {
	var open atomic.Bool
	c := NewPageCache(func() (map[string]bool, error) {
		return map[string]bool{"dashboard": !open.Load()}, nil
	}, time.Hour)
	if !c.RequiresLogin("dashboard") {
		t.Fatal("starts protected")
	}
	open.Store(true)
	if !c.RequiresLogin("dashboard") {
		t.Fatal("within the TTL the cached value is used")
	}
	c.Reload()
	if c.RequiresLogin("dashboard") {
		t.Error("after Reload the new setting must apply at once")
	}
}
