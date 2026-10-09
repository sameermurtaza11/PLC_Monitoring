package access

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/auth"
)

// PageFlags tells whether a page group needs a login (its authentication switch).
type PageFlags interface {
	RequiresLogin(pageKey string) bool
}

// Denial describes a refused request, for the OnDeny callback.
type Denial struct {
	Status    int // 401 (login needed) or 403 (logged in, not permitted)
	NeedLogin bool
	Rule      Rule
	User      *auth.User
}

// Guard enforces Rules on every request.
type Guard struct {
	Rules  map[string]Rule
	Flags  PageFlags
	OnDeny func(c *gin.Context, d Denial) // renders the refusal and may audit it
}

func NewGuard(flags PageFlags, onDeny func(*gin.Context, Denial)) *Guard {
	return &Guard{Rules: Rules, Flags: flags, OnDeny: onDeny}
}

// RuleFor returns the rule of a route (DefaultRule if it is not listed).
func (g *Guard) RuleFor(method, route string) Rule {
	if r, ok := g.Rules[method+" "+route]; ok {
		return r
	}
	return DefaultRule
}

// Decide is the whole policy:
//
//	Public route                                  → open
//	needs login = AlwaysLogin, or Action, or the page's switch is on
//	no login needed                               → open (whoever you are)
//	login needed, nobody logged in                → 401
//	logged in, role lacks the feature             → 403
//	otherwise                                     → open
//
// It returns nil when the request may go on.
func (g *Guard) Decide(rule Rule, user *auth.User) *Denial {
	if rule.Public {
		return nil
	}
	needsLogin := rule.AlwaysLogin || rule.Action || g.Flags.RequiresLogin(rule.Page)
	if !needsLogin {
		return nil
	}
	if user == nil {
		return &Denial{Status: http.StatusUnauthorized, NeedLogin: true, Rule: rule}
	}
	if rule.Permission != "" && !user.Can(rule.Permission) {
		return &Denial{Status: http.StatusForbidden, Rule: rule, User: user}
	}
	return nil
}

// Middleware must run after auth.Load, which identifies the user.
func (g *Guard) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		route := c.FullPath()
		if route == "" {
			return // no such route: gin answers 404
		}
		rule, listed := g.Rules[c.Request.Method+" "+route]
		if !listed {
			log.Printf("access: route %s %s has no access rule; closed to non-administrators", c.Request.Method, route)
			rule = DefaultRule
		}
		if d := g.Decide(rule, auth.FromContext(c)); d != nil {
			g.OnDeny(c, *d)
			c.Abort()
		}
	}
}

// NavItem is one link of the top menu.
type NavItem struct {
	Key   string // matches the page's "Page" value, to mark the active link
	Label string
	Href  string
}

var navItems = []struct {
	NavItem
	route string // the rule of the page the link opens
}{
	{NavItem{"dashboard", "Dashboard", "/"}, "GET /"},
	{NavItem{"config", "Configuration", "/config"}, "GET /config"},
	{NavItem{"ams-config", "Alarm Config", "/ams/config"}, "GET /ams/config"},
	{NavItem{"access", "Access", "/admin/access"}, "GET /admin/access"},
}

// Nav returns the menu links this visitor may actually open.
func (g *Guard) Nav(user *auth.User) []NavItem {
	var out []NavItem
	for _, it := range navItems {
		if g.Decide(g.Rules[it.route], user) == nil {
			out = append(out, it.NavItem)
		}
	}
	return out
}

// PageCache serves the authentication switches from memory and re-reads them
// from the database every TTL, and immediately after Reload. If the database
// cannot be read the last good values are kept; if there never were any, every
// page counts as login-required (fail closed).
type PageCache struct {
	load func() (map[string]bool, error)
	ttl  time.Duration

	mu        sync.RWMutex
	flags     map[string]bool
	loaded    bool
	fetchedAt time.Time
	busy      atomic.Bool
}

func NewPageCache(load func() (map[string]bool, error), ttl time.Duration) *PageCache {
	return &PageCache{load: load, ttl: ttl}
}

func (p *PageCache) RequiresLogin(pageKey string) bool {
	p.mu.RLock()
	stale := !p.loaded || time.Since(p.fetchedAt) > p.ttl
	p.mu.RUnlock()
	if stale {
		p.Reload()
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	v, ok := p.flags[pageKey]
	if !p.loaded || !ok {
		return true // unknown page or no data: protected
	}
	return v
}

// safeLoad runs the loader and turns a panic into an error, so a broken
// loader fails closed (login required) instead of failing the request.
func (p *PageCache) safeLoad() (flags map[string]bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			flags, err = nil, fmt.Errorf("page settings loader panicked: %v", r)
		}
	}()
	return p.load()
}

// Reload re-reads the switches now (one reload at a time).
func (p *PageCache) Reload() {
	if !p.busy.CompareAndSwap(false, true) {
		return
	}
	defer p.busy.Store(false)
	flags, err := p.safeLoad()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fetchedAt = time.Now() // also after a failure: do not hammer a broken database
	if err != nil {
		log.Printf("access: could not read page settings (keeping the last known): %v", err)
		return
	}
	p.flags, p.loaded = flags, true
}
