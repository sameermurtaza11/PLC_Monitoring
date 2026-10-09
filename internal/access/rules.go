// Package access decides who may open which page and use which feature.
//
// Two independent switches, both editable on the Access page:
//
//	AUTHENTICATION (per page):   does the page need a login?  true / false
//	AUTHORIZATION  (per feature): may this role use it?        true / false
//
// Every route of the application is listed in Rules. A route that is not
// listed is NOT left open: it needs a login and an administrator (see
// DefaultRule), and a test fails if a route is missing from the table.
package access

// Page is one switchable page group. RequiresLogin(Key) is its authentication
// switch; the routes that belong to it are the Rules entries with that Page.
type Page struct {
	Key         string
	Label       string
	Description string
}

// Pages are the groups shown on the Access page, in display order.
var Pages = []Page{
	{"dashboard", "Dashboard", "Live values of all PVs"},
	{"pv_detail", "PV detail", "Gauge, trend and history of one PV"},
	{"plc_config", "PLC / PV configuration", "The Configuration page"},
	{"alarm_config", "Alarm configuration", "Alarm definitions, versions and approval"},
	{"alarm_overview", "Alarm overview", "Current alarm states and acknowledgement"},
}

// Rule says what a route needs.
type Rule struct {
	// Page is the group whose authentication switch applies.
	Page string
	// Public routes are always open (login, health check, static files).
	Public bool
	// AlwaysLogin routes need a login whatever the page switches say.
	AlwaysLogin bool
	// Action marks routes that change data or show an edit form. They always
	// need a login, even when their page is open to everyone: what they do
	// must be attributable to a person (audit trail, CSRF token).
	Action bool
	// Permission is the feature a logged-in user must hold.
	Permission string
	// Label names the feature in the "not permitted" message.
	Label string
	// AuditAction, if set, is written to the audit log when a logged-in user is refused.
	AuditAction string
	AuditTarget string
}

// DefaultRule applies to a route missing from Rules: closed to everyone except
// administrators.
var DefaultRule = Rule{AlwaysLogin: true, Permission: "users.manage", Label: "an unregistered page"}

var (
	pubRule = Rule{Public: true}

	dashboard = Rule{Page: "dashboard", Permission: "dashboard.view", Label: "the dashboard"}
	pvView    = Rule{Page: "pv_detail", Permission: "pv.view", Label: "PV detail and history"}
	cfgView   = Rule{Page: "plc_config", Permission: "plc.config.view", Label: "the PLC / PV configuration"}
	cfgEdit   = Rule{Page: "plc_config", Permission: "plc.config.edit", Action: true, Label: "changing the PLC / PV configuration"}

	alarmCfgView = Rule{Page: "alarm_config", Permission: "alarm.view", Label: "alarm configuration"}
	alarmCfgEdit = Rule{Page: "alarm_config", Permission: "alarm.config", Action: true, Label: "changing alarm definitions",
		AuditAction: "config.submit", AuditTarget: "alarm_config"}
	alarmCfgReview = Rule{Page: "alarm_config", Permission: "alarm.approve", Action: true, Label: "approving alarm changes",
		AuditAction: "config.review", AuditTarget: "alarm_config"}

	alarmView = Rule{Page: "alarm_overview", Permission: "alarm.view", Label: "alarm states"}
	alarmAck  = Rule{Page: "alarm_overview", Permission: "alarm.ack", Action: true, Label: "acknowledging alarms",
		AuditAction: "alarm.acknowledge", AuditTarget: "alarm"}

	accessAdmin = Rule{AlwaysLogin: true, Permission: "access.manage", Label: "the Access settings",
		AuditAction: "access.update", AuditTarget: "access"}
	accessSave = Rule{AlwaysLogin: true, Action: true, Permission: "access.manage", Label: "the Access settings",
		AuditAction: "access.update", AuditTarget: "access"}
)

// Rules maps "METHOD /gin/route/pattern" to what the route needs.
var Rules = map[string]Rule{
	// always open
	"GET /login":             pubRule,
	"POST /login":            pubRule,
	"POST /logout":           pubRule,
	"GET /healthz":           pubRule,
	"GET /favicon.ico":       pubRule,
	"GET /static/*filepath":  pubRule,
	"HEAD /static/*filepath": pubRule,

	// dashboard
	"GET /":                 dashboard,
	"GET /partials/pv-rows": dashboard,
	"GET /pv/:id":           pvView,
	"GET /pv/:id/live":      pvView,
	"GET /pv/:id/history":   pvView,

	// PLC / PV configuration: lists are "view", forms and saves are "edit"
	"GET /config":               cfgView,
	"GET /config/plcs":          cfgView,
	"GET /config/pvs":           cfgView,
	"GET /config/plcs/new":      cfgEdit,
	"GET /config/plcs/:id/edit": cfgEdit,
	"POST /config/plcs":         cfgEdit,
	"POST /config/plcs/:id":     cfgEdit,
	"DELETE /config/plcs/:id":   cfgEdit,
	"GET /config/pvs/new":       cfgEdit,
	"GET /config/pvs/:id/edit":  cfgEdit,
	"POST /config/pvs":          cfgEdit,
	"POST /config/pvs/:id":      cfgEdit,
	"DELETE /config/pvs/:id":    cfgEdit,

	// alarm configuration
	"GET /ams/config":             alarmCfgView,
	"GET /ams/config/:id":         alarmCfgView,
	"GET /ams/config/new":         alarmCfgEdit,
	"POST /ams/config":            alarmCfgEdit,
	"POST /ams/config/:id":        alarmCfgEdit,
	"POST /ams/config/:id/review": alarmCfgReview,

	// alarm overview API
	"GET /ams/api/alarms":          alarmView,
	"GET /ams/api/csrf":            alarmView,
	"POST /ams/api/alarms/:id/ack": alarmAck,

	// access settings: never open, administrators only
	"GET /admin/access":  accessAdmin,
	"POST /admin/access": accessSave,
}
