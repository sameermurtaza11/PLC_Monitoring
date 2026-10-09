package auth

// NewTestUser builds a User with the given permissions, for tests of code
// that takes an authenticated user. Production users only come from Login.
func NewTestUser(id int, username, role string, permissions ...string) *User {
	perms := make(map[string]bool, len(permissions))
	for _, p := range permissions {
		perms[p] = true
	}
	return &User{ID: id, Username: username, Role: role, perms: perms}
}
