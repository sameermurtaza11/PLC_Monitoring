package store

import (
	"context"
	"errors"
	"time"
)

// ErrUnknownRole is returned by CreateUser when the role does not exist.
var ErrUnknownRole = errors.New("unknown role")

// UserCredentials is what the login check needs about one account.
type UserCredentials struct {
	ID           int
	Username     string
	DisplayName  string
	Role         string
	PasswordHash string
	Enabled      bool
	FailedLogins int
	LockedUntil  *time.Time
}

// AuthUser is an authenticated user with the permissions of their role.
type AuthUser struct {
	ID          int
	Username    string
	DisplayName string
	Role        string
	Permissions []string
}

// UserCredentialsByName returns the account for a (lower-case) username.
// pgx.ErrNoRows if it does not exist.
func (s *Store) UserCredentialsByName(ctx context.Context, username string) (UserCredentials, error) {
	var u UserCredentials
	err := s.pool.QueryRow(ctx, `
		SELECT u.user_id, u.username, u.display_name, r.role_name,
		       u.password_hash, u.enabled, u.failed_logins, u.locked_until
		FROM users u JOIN roles r ON r.role_id = u.role_id
		WHERE u.username = $1`, username).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role,
			&u.PasswordHash, &u.Enabled, &u.FailedLogins, &u.LockedUntil)
	return u, err
}

// RecordLoginFailure counts a wrong password and locks the account for
// lockFor once maxFails is reached.
func (s *Store) RecordLoginFailure(ctx context.Context, userID, maxFails int, lockFor time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE users SET
			failed_logins = failed_logins + 1,
			locked_until  = CASE WHEN failed_logins + 1 >= $2
			                     THEN now() + make_interval(secs => $3) ELSE locked_until END
		WHERE user_id = $1`, userID, maxFails, lockFor.Seconds())
	return err
}

func (s *Store) RecordLoginSuccess(ctx context.Context, userID int) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE users SET failed_logins = 0, locked_until = NULL, last_login_at = now()
		WHERE user_id = $1`, userID)
	return err
}

func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID int, expires time.Time, clientIP string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO user_sessions (token_hash, user_id, expires_at, client_ip)
		VALUES ($1, $2, $3, $4)`, tokenHash, userID, expires, clientIP)
	return err
}

// SessionUser returns the user of a live session (not expired, account
// enabled). pgx.ErrNoRows otherwise.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (AuthUser, error) {
	var u AuthUser
	err := s.pool.QueryRow(ctx, `
		WITH sess AS (
			UPDATE user_sessions SET last_seen_at = now()
			WHERE token_hash = $1 AND expires_at > now()
			RETURNING user_id
		)
		SELECT u.user_id, u.username, u.display_name, r.role_name,
		       COALESCE((SELECT array_agg(rp.permission_key ORDER BY rp.permission_key)
		                 FROM role_permissions rp WHERE rp.role_id = r.role_id), '{}')
		FROM sess
		JOIN users u ON u.user_id = sess.user_id AND u.enabled
		JOIN roles r ON r.role_id = u.role_id`, tokenHash).
		Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &u.Permissions)
	return u, err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE token_hash = $1`, tokenHash)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM user_sessions WHERE expires_at <= now()`)
	return err
}

// CreateUser inserts an account. Fails (integrity violation) if the
// username exists or the role is unknown.
func (s *Store) CreateUser(ctx context.Context, username, displayName, passwordHash, role string) error {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO users (username, display_name, password_hash, role_id)
		SELECT $1, $2, $3, role_id FROM roles WHERE role_name = $4`,
		username, displayName, passwordHash, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrUnknownRole
	}
	return nil
}
