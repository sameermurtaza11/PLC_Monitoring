package access

import (
	"testing"

	"PLC_Monitoring/internal/store"
)

func TestGuardrailsKeepTheAdministratorLockedIn(t *testing.T) {
	// someone submits a matrix where admin has nothing at all
	submitted := store.Matrix{"viewer": {"dashboard.view": true}, "admin": {}}
	got := EnforceGuardrails(submitted)

	for _, p := range AdminLocked {
		if !got["admin"][p] {
			t.Errorf("admin lost %s; it must always be kept", p)
		}
	}
	if !got["viewer"]["dashboard.view"] {
		t.Error("other roles must be left as submitted")
	}
	if submitted["admin"]["access.manage"] {
		t.Error("the submitted matrix must not be modified")
	}
}

func TestGuardrailsWorkWhenAdminIsMissingFromTheMatrix(t *testing.T) {
	got := EnforceGuardrails(store.Matrix{})
	if !got["admin"]["access.manage"] || !got["admin"]["users.manage"] {
		t.Errorf("admin features not restored: %v", got["admin"])
	}
}

func TestIsLocked(t *testing.T) {
	if !IsLocked("admin", "access.manage") || !IsLocked("admin", "users.manage") {
		t.Error("admin's access.manage and users.manage are locked")
	}
	if IsLocked("admin", "alarm.view") || IsLocked("viewer", "access.manage") {
		t.Error("only the administrator's two locked features are fixed")
	}
}

// Every feature a rule asks for must exist as a permission the matrix can grant.
func TestEveryRulePermissionIsKnownAndEveryPageHasItsSwitch(t *testing.T) {
	known := map[string]bool{
		"alarm.view": true, "alarm.ack": true, "alarm.shelve": true, "alarm.suppress": true, "alarm.service": true,
		"alarm.config": true, "alarm.approve": true, "users.manage": true, "audit.view": true,
		"dashboard.view": true, "pv.view": true, "plc.config.view": true, "plc.config.edit": true, "access.manage": true,
	}
	pages := map[string]bool{}
	for _, p := range Pages {
		pages[p.Key] = true
	}
	used := map[string]bool{}
	for route, r := range Rules {
		if r.Public {
			continue
		}
		if r.Permission != "" && !known[r.Permission] {
			t.Errorf("%s needs unknown feature %q", route, r.Permission)
		}
		if r.Page != "" {
			if !pages[r.Page] {
				t.Errorf("%s belongs to page %q, which has no authentication switch", route, r.Page)
			}
			used[r.Page] = true
		} else if !r.AlwaysLogin {
			t.Errorf("%s is neither public, nor always-login, nor part of a switchable page", route)
		}
	}
	for _, p := range Pages {
		if !used[p.Key] {
			t.Errorf("page %q has a switch but no route uses it", p.Key)
		}
	}
}
