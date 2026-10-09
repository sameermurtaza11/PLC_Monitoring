package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"PLC_Monitoring/internal/store"
)

// fakeRepo is an in-memory Repository.
type fakeRepo struct {
	users    map[string]*store.UserCredentials
	perms    map[string][]string // role -> permissions
	sessions map[string]sessionRow
}

type sessionRow struct {
	userID  int
	expires time.Time
}

func newFakeRepo(t *testing.T, now time.Time) *fakeRepo {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("correct-horse"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return &fakeRepo{
		users: map[string]*store.UserCredentials{
			"anna": {ID: 1, Username: "anna", DisplayName: "Anna", Role: "operator", PasswordHash: string(hash), Enabled: true},
			"off":  {ID: 2, Username: "off", Role: "viewer", PasswordHash: string(hash), Enabled: false},
		},
		perms:    map[string][]string{"operator": {"alarm.ack", "alarm.view"}, "viewer": {"alarm.view"}},
		sessions: map[string]sessionRow{},
	}
}

func (f *fakeRepo) UserCredentialsByName(_ context.Context, name string) (store.UserCredentials, error) {
	if u, ok := f.users[name]; ok {
		return *u, nil
	}
	return store.UserCredentials{}, pgx.ErrNoRows
}

func (f *fakeRepo) RecordLoginFailure(_ context.Context, id, maxFails int, lockFor time.Duration) error {
	for _, u := range f.users {
		if u.ID == id {
			u.FailedLogins++
			if u.FailedLogins >= maxFails {
				until := time.Now().Add(lockFor)
				u.LockedUntil = &until
			}
		}
	}
	return nil
}

func (f *fakeRepo) RecordLoginSuccess(_ context.Context, id int) error {
	for _, u := range f.users {
		if u.ID == id {
			u.FailedLogins, u.LockedUntil = 0, nil
		}
	}
	return nil
}

func (f *fakeRepo) CreateSession(_ context.Context, hash []byte, userID int, expires time.Time, _ string) error {
	f.sessions[string(hash)] = sessionRow{userID, expires}
	return nil
}

func (f *fakeRepo) SessionUser(_ context.Context, hash []byte) (store.AuthUser, error) {
	s, ok := f.sessions[string(hash)]
	if !ok || !s.expires.After(time.Now()) {
		return store.AuthUser{}, pgx.ErrNoRows
	}
	for _, u := range f.users {
		if u.ID == s.userID && u.Enabled {
			return store.AuthUser{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, Permissions: f.perms[u.Role]}, nil
		}
	}
	return store.AuthUser{}, pgx.ErrNoRows
}

func (f *fakeRepo) DeleteSession(_ context.Context, hash []byte) error {
	delete(f.sessions, string(hash))
	return nil
}
func (f *fakeRepo) DeleteExpiredSessions(context.Context) error                      { return nil }
func (f *fakeRepo) CreateUser(context.Context, string, string, string, string) error { return nil }

func TestLoginCreatesSessionAndAuthenticates(t *testing.T) {
	repo := newFakeRepo(t, time.Now())
	svc := NewService(repo)

	token, expires, err := svc.Login(context.Background(), "  Anna ", "correct-horse", "127.0.0.1")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if time.Until(expires) < 11*time.Hour {
		t.Errorf("session expires too soon: %v", expires)
	}
	for k := range repo.sessions {
		if k == token {
			t.Fatal("raw token stored; only its hash may be stored")
		}
	}

	u, err := svc.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if u.Username != "anna" || u.Role != "operator" {
		t.Errorf("wrong user: %+v", u)
	}
	if !u.Can("alarm.ack") {
		t.Error("operator should be able to acknowledge")
	}
	if u.Can("alarm.config") {
		t.Error("operator must not be able to change configuration")
	}
}

func TestWrongPasswordAndUnknownUserLookAlike(t *testing.T) {
	svc := NewService(newFakeRepo(t, time.Now()))
	for _, c := range []struct{ user, pass string }{
		{"anna", "wrong"}, {"nobody", "correct-horse"}, {"off", "correct-horse"},
	} {
		if _, _, err := svc.Login(context.Background(), c.user, c.pass, ""); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s/%s: got %v, want ErrInvalidCredentials", c.user, c.pass, err)
		}
	}
}

func TestAccountLocksAfterRepeatedFailures(t *testing.T) {
	svc := NewService(newFakeRepo(t, time.Now()))
	for i := 0; i < MaxFailedLogins; i++ {
		svc.Login(context.Background(), "anna", "bad", "")
	}
	// Even the right password is refused while locked.
	if _, _, err := svc.Login(context.Background(), "anna", "correct-horse", ""); !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v, want ErrLocked", err)
	}
}

func TestLogoutEndsSession(t *testing.T) {
	svc := NewService(newFakeRepo(t, time.Now()))
	token, _, err := svc.Login(context.Background(), "anna", "correct-horse", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(context.Background(), token); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(context.Background(), token); !errors.Is(err, ErrNoSession) {
		t.Fatalf("got %v, want ErrNoSession", err)
	}
}

func TestAuthenticateRejectsEmptyAndUnknownTokens(t *testing.T) {
	svc := NewService(newFakeRepo(t, time.Now()))
	for _, tok := range []string{"", "not-a-token"} {
		if _, err := svc.Authenticate(context.Background(), tok); !errors.Is(err, ErrNoSession) {
			t.Errorf("token %q: got %v, want ErrNoSession", tok, err)
		}
	}
}

func TestCreateUserRejectsWeakPassword(t *testing.T) {
	svc := NewService(newFakeRepo(t, time.Now()))
	if err := svc.CreateUser(context.Background(), "bob", "", "short", "viewer"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("got %v, want ErrWeakPassword", err)
	}
}

func TestNilUserCannotDoAnything(t *testing.T) {
	var u *User
	if u.Can("alarm.view") {
		t.Fatal("nil user must have no permissions")
	}
}
