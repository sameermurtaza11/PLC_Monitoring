-- =====================================================================
-- PLC Monitoring — migration 003: users, roles, permissions, sessions
--
-- AMS step 1 (foundation). Adds NEW tables only; nothing existing is
-- changed. Safe to run more than once.
--
-- Run:  psql -U postgres -d plc_monitoring -f migrations/003_auth.sql
--       (Windows: set  $env:PGCLIENTENCODING = "UTF8"  first)
--
-- No user is created here (a password must never live in a migration).
-- Create the first administrator with:
--       go run ./cmd/adduser -user admin -role admin
-- =====================================================================

BEGIN;

CREATE TABLE IF NOT EXISTS roles (
    role_id     SERIAL PRIMARY KEY,
    role_name   TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS permissions (
    permission_key TEXT PRIMARY KEY,
    description    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS role_permissions (
    role_id        INTEGER NOT NULL REFERENCES roles(role_id) ON DELETE CASCADE,
    permission_key TEXT    NOT NULL REFERENCES permissions(permission_key) ON DELETE CASCADE,
    PRIMARY KEY (role_id, permission_key)
);

CREATE TABLE IF NOT EXISTS users (
    user_id       SERIAL PRIMARY KEY,
    username      TEXT        NOT NULL UNIQUE CHECK (username = lower(username) AND username <> ''),
    display_name  TEXT        NOT NULL DEFAULT '',
    password_hash TEXT        NOT NULL,                 -- bcrypt, never the password
    role_id       INTEGER     NOT NULL REFERENCES roles(role_id),
    enabled       BOOLEAN     NOT NULL DEFAULT TRUE,
    failed_logins INTEGER     NOT NULL DEFAULT 0,
    locked_until  TIMESTAMPTZ NULL,
    last_login_at TIMESTAMPTZ NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Only a SHA-256 hash of the session token is stored, so a copy of this
-- table cannot be used to log in.
CREATE TABLE IF NOT EXISTS user_sessions (
    token_hash   BYTEA       PRIMARY KEY,
    user_id      INTEGER     NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    client_ip    TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS user_sessions_user_idx    ON user_sessions (user_id);
CREATE INDEX IF NOT EXISTS user_sessions_expires_idx ON user_sessions (expires_at);

-- ---------------------------------------------------------------------
-- Permissions (the list in the AMS specification, section 9)
-- ---------------------------------------------------------------------
INSERT INTO permissions (permission_key, description) VALUES
    ('alarm.view',     'View alarm data and history'),
    ('alarm.ack',      'Acknowledge alarms'),
    ('alarm.shelve',   'Shelve and unshelve alarms'),
    ('alarm.suppress', 'Apply or remove suppression'),
    ('alarm.service',  'Place alarms out of service and restore them'),
    ('alarm.config',   'Create and modify alarm definitions'),
    ('alarm.approve',  'Approve alarm configuration changes'),
    ('users.manage',   'Manage users and roles'),
    ('audit.view',     'Access audit and performance reports')
ON CONFLICT (permission_key) DO NOTHING;

INSERT INTO roles (role_name, description) VALUES
    ('viewer',     'Read-only access to alarms'),
    ('operator',   'Acknowledge and shelve alarms'),
    ('engineer',   'Operator rights plus suppression, out-of-service and alarm configuration'),
    ('supervisor', 'Operator rights plus suppression, out-of-service, approvals and audit reports'),
    ('admin',      'Everything, including user management')
ON CONFLICT (role_name) DO NOTHING;

-- Separation of duties: engineers edit alarm configuration, supervisors approve it.
INSERT INTO role_permissions (role_id, permission_key)
SELECT r.role_id, p.key
FROM roles r
JOIN (VALUES
    ('viewer',     'alarm.view'),
    ('operator',   'alarm.view'),     ('operator', 'alarm.ack'),   ('operator', 'alarm.shelve'),
    ('engineer',   'alarm.view'),     ('engineer', 'alarm.ack'),   ('engineer', 'alarm.shelve'),
    ('engineer',   'alarm.suppress'), ('engineer', 'alarm.service'), ('engineer', 'alarm.config'),
    ('supervisor', 'alarm.view'),     ('supervisor', 'alarm.ack'), ('supervisor', 'alarm.shelve'),
    ('supervisor', 'alarm.suppress'), ('supervisor', 'alarm.service'),
    ('supervisor', 'alarm.approve'),  ('supervisor', 'audit.view'),
    ('admin',      'alarm.view'),     ('admin', 'alarm.ack'),      ('admin', 'alarm.shelve'),
    ('admin',      'alarm.suppress'), ('admin', 'alarm.service'),  ('admin', 'alarm.config'),
    ('admin',      'alarm.approve'),  ('admin', 'users.manage'),   ('admin', 'audit.view')
) AS p(role, key) ON p.role = r.role_name
ON CONFLICT DO NOTHING;

COMMIT;
