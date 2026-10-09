// Package auth handles individual user accounts: password login, server-side
// sessions and permission checks. The acting user is always taken from the
// session on the server, never from anything the client sends.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"PLC_Monitoring/internal/store"
)

const (
	SessionLifetime = 12 * time.Hour
	MaxFailedLogins = 5
	LockDuration    = 5 * time.Minute
	MinPasswordLen  = 8
)

var (
	// ErrInvalidCredentials is returned for an unknown user, a wrong password
	// or a disabled account — the caller cannot tell which, on purpose.
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrLocked             = errors.New("account temporarily locked after too many failed logins")
	ErrNoSession          = errors.New("not logged in")
	ErrWeakPassword       = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
)

// Repository is the part of the store the auth service needs.
// *store.Store implements it; tests use a fake.
type Repository interface {
	UserCredentialsByName(ctx context.Context, username string) (store.UserCredentials, error)
	RecordLoginFailure(ctx context.Context, userID, maxFails int, lockFor time.Duration) error
	RecordLoginSuccess(ctx context.Context, userID int) error
	CreateSession(ctx context.Context, tokenHash []byte, userID int, expires time.Time, clientIP string) error
	SessionUser(ctx context.Context, tokenHash []byte) (store.AuthUser, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error
	DeleteExpiredSessions(ctx context.Context) error
	CreateUser(ctx context.Context, username, displayName, passwordHash, role string) error
}

// User is the authenticated user attached to a request.
type User struct {
	ID          int
	Username    string
	DisplayName string
	Role        string
	perms       map[string]bool
}

// Can reports whether the user's role grants the permission.
func (u *User) Can(permission string) bool { return u != nil && u.perms[permission] }

// Permissions returns the granted permission keys, sorted.
func (u *User) Permissions() []string {
	keys := make([]string, 0, len(u.perms))
	for k := range u.perms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func fromStore(u store.AuthUser) *User {
	perms := make(map[string]bool, len(u.Permissions))
	for _, p := range u.Permissions {
		perms[p] = true
	}
	return &User{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: u.Role, perms: perms}
}

type Service struct {
	repo Repository
	now  func() time.Time
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: time.Now}
}

// dummyHash is compared when the username does not exist, so a login for an
// unknown user takes about as long as one for a real user.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), bcrypt.DefaultCost)

func normalize(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

// Login checks the password and starts a session. It returns the raw session
// token (for the cookie) — only its hash is stored — and its expiry.
func (s *Service) Login(ctx context.Context, username, password, clientIP string) (token string, expires time.Time, err error) {
	username = normalize(username)
	cred, err := s.repo.UserCredentialsByName(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return "", time.Time{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("load user: %w", err)
	}

	if cred.LockedUntil != nil && cred.LockedUntil.After(s.now()) {
		return "", time.Time{}, ErrLocked
	}
	if !cred.Enabled {
		bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return "", time.Time{}, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(cred.PasswordHash), []byte(password)) != nil {
		if err := s.repo.RecordLoginFailure(ctx, cred.ID, MaxFailedLogins, LockDuration); err != nil {
			return "", time.Time{}, fmt.Errorf("record login failure: %w", err)
		}
		return "", time.Time{}, ErrInvalidCredentials
	}

	token, hash, err := newToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expires = s.now().Add(SessionLifetime)
	if err := s.repo.CreateSession(ctx, hash, cred.ID, expires, clientIP); err != nil {
		return "", time.Time{}, fmt.Errorf("create session: %w", err)
	}
	if err := s.repo.RecordLoginSuccess(ctx, cred.ID); err != nil {
		return "", time.Time{}, fmt.Errorf("record login: %w", err)
	}
	_ = s.repo.DeleteExpiredSessions(ctx) // housekeeping; failure is harmless
	return token, expires, nil
}

// Authenticate returns the user of a session token, or ErrNoSession.
func (s *Service) Authenticate(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, ErrNoSession
	}
	u, err := s.repo.SessionUser(ctx, hashToken(token))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoSession
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	return fromStore(u), nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.repo.DeleteSession(ctx, hashToken(token))
}

// CreateUser hashes the password and stores a new account.
func (s *Service) CreateUser(ctx context.Context, username, displayName, password, role string) error {
	username = normalize(username)
	if username == "" {
		return errors.New("username is required")
	}
	if len(password) < MinPasswordLen {
		return ErrWeakPassword
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	return s.repo.CreateUser(ctx, username, strings.TrimSpace(displayName), string(hash), role)
}

func newToken() (token string, hash []byte, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}
