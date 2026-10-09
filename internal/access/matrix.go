package access

import "PLC_Monitoring/internal/store"

// AdminRole is the role that can never lose the features in AdminLocked.
const AdminRole = "admin"

// AdminLocked are the features the administrator role always keeps, so that
// nobody can lock the administrators out of the Access page or user management.
var AdminLocked = []string{"access.manage", "users.manage"}

// EnforceGuardrails returns the matrix with the administrator's locked
// features forced on. Whatever was submitted, these cannot be switched off.
func EnforceGuardrails(m store.Matrix) store.Matrix {
	out := store.Matrix{}
	for role, perms := range m {
		out[role] = map[string]bool{}
		for p, v := range perms {
			out[role][p] = v
		}
	}
	if out[AdminRole] == nil {
		out[AdminRole] = map[string]bool{}
	}
	for _, p := range AdminLocked {
		out[AdminRole][p] = true
	}
	return out
}

// IsLocked reports whether a role/feature box is fixed and cannot be changed.
func IsLocked(role, permission string) bool {
	if role != AdminRole {
		return false
	}
	for _, p := range AdminLocked {
		if p == permission {
			return true
		}
	}
	return false
}
