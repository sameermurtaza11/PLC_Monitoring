-- =====================================================================
-- PLC Monitoring — migration 007: page protection + feature access
--
-- Adds ONE table and new rows in existing lookup tables. Nothing existing
-- is changed. Safe to run more than once. Needs 003 (roles/permissions).
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f migrations/007_access.sql
--
-- Two separate switches:
--   AUTHENTICATION  per page      page_access.requires_login  true / false
--                                 false = the page opens without a login
--   AUTHORIZATION   per feature   role_permissions            true / false
--                                 a row = this role may use this feature
-- Both are edited on the "Access" page (/admin/access); this migration
-- only sets the starting values.
-- =====================================================================

BEGIN;

CREATE TABLE IF NOT EXISTS page_access (
    page_key       TEXT PRIMARY KEY,
    requires_login BOOLEAN NOT NULL DEFAULT TRUE,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by     INTEGER NULL REFERENCES users(user_id) ON DELETE SET NULL
);

-- Starting point: the whole system needs a login.
INSERT INTO page_access (page_key, requires_login) VALUES
    ('dashboard',      TRUE),
    ('pv_detail',      TRUE),
    ('plc_config',     TRUE),
    ('alarm_config',   TRUE),
    ('alarm_overview', TRUE)
ON CONFLICT (page_key) DO NOTHING;

-- Features for the pages that existed before the AMS.
INSERT INTO permissions (permission_key, description) VALUES
    ('dashboard.view',  'Dashboard: see the live values of all PVs'),
    ('pv.view',         'PV detail: gauge, trend and history'),
    ('plc.config.view', 'PLC / PV configuration: view the lists'),
    ('plc.config.edit', 'PLC / PV configuration: add, change and delete PLCs and PVs'),
    ('access.manage',   'Access page: choose which pages need a login and what each role may do')
ON CONFLICT (permission_key) DO NOTHING;

-- Clearer wording for the AMS features (same keys as before).
UPDATE permissions SET description = 'Alarms: see alarm lists, states and history' WHERE permission_key = 'alarm.view'     AND description = 'View alarm data and history';
UPDATE permissions SET description = 'Alarms: acknowledge'                         WHERE permission_key = 'alarm.ack'      AND description = 'Acknowledge alarms';
UPDATE permissions SET description = 'Alarms: shelve and unshelve'                 WHERE permission_key = 'alarm.shelve'   AND description = 'Shelve and unshelve alarms';
UPDATE permissions SET description = 'Alarms: apply or remove suppression'         WHERE permission_key = 'alarm.suppress' AND description = 'Apply or remove suppression';
UPDATE permissions SET description = 'Alarms: place out of service and restore'    WHERE permission_key = 'alarm.service'  AND description = 'Place alarms out of service and restore them';
UPDATE permissions SET description = 'Alarm configuration: create and change definitions' WHERE permission_key = 'alarm.config'  AND description = 'Create and modify alarm definitions';
UPDATE permissions SET description = 'Alarm configuration: approve or reject changes'     WHERE permission_key = 'alarm.approve' AND description = 'Approve alarm configuration changes';
UPDATE permissions SET description = 'Users: manage accounts and roles'            WHERE permission_key = 'users.manage'   AND description = 'Manage users and roles';
UPDATE permissions SET description = 'Reports: audit log and performance reports'  WHERE permission_key = 'audit.view'     AND description = 'Access audit and performance reports';

-- Starting values of the role matrix for the new features.
INSERT INTO role_permissions (role_id, permission_key)
SELECT r.role_id, p.key
FROM roles r
JOIN (VALUES
    ('viewer',     'dashboard.view'), ('viewer',     'pv.view'),
    ('operator',   'dashboard.view'), ('operator',   'pv.view'),
    ('engineer',   'dashboard.view'), ('engineer',   'pv.view'),
    ('engineer',   'plc.config.view'), ('engineer',  'plc.config.edit'),
    ('supervisor', 'dashboard.view'), ('supervisor', 'pv.view'),
    ('supervisor', 'plc.config.view'),
    ('admin',      'dashboard.view'), ('admin',      'pv.view'),
    ('admin',      'plc.config.view'), ('admin',     'plc.config.edit'),
    ('admin',      'access.manage')
) AS p(role, key) ON p.role = r.role_name
ON CONFLICT DO NOTHING;

COMMIT;
