-- =====================================================================
-- PLC Monitoring — migration 004: AMS alarm configuration (step 2)
--
-- NEW tables only. pv_metadata / pv_latest / pv_history are not changed.
-- Safe to run more than once. Needs 003 (users) first.
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f migrations/004_alarm_config.sql
--
-- Design
--   alarm_config           identity of an alarm: one PV + one condition
--                          (HIHI / HI / LO / LOLO / DISCRETE). Never edited.
--   alarm_config_versions  every change is a NEW version row (pending ->
--                          approved / rejected). An approved version is never
--                          modified, so an old event can always be read with
--                          the settings that were in force at that time.
--   alarm_approvals        who submitted / approved / rejected which version.
--   alarm_audit_log        append-only record of operator and engineer actions.
--
-- Condition, priority and class are three independent attributes.
-- PLC, equipment and subsystem are NOT copied: they are read from
-- pv_metadata / plc_metadata, so they cannot drift out of date.
-- =====================================================================

BEGIN;

-- ---------------------------------------------------------------------
-- Site-defined lookups
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_priorities (
    priority_id SERIAL PRIMARY KEY,
    name        TEXT     NOT NULL UNIQUE,
    rank        SMALLINT NOT NULL UNIQUE,            -- higher = more urgent
    description TEXT     NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS alarm_classes (
    class_id    SERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT ''
);

INSERT INTO alarm_priorities (name, rank, description) VALUES
    ('Minor',    1, 'Deviation to note; no immediate action needed'),
    ('Major',    2, 'Needs operator action soon'),
    ('Critical', 3, 'Needs immediate operator action')
ON CONFLICT DO NOTHING;

INSERT INTO alarm_classes (name, description) VALUES
    ('Process',       'Process deviation'),
    ('Safety',        'Personnel or plant safety'),
    ('Environmental', 'Environmental release or limit'),
    ('Equipment',     'Equipment condition or protection')
ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------
-- Alarm identity
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_config (
    alarm_id        SERIAL PRIMARY KEY,
    pv_id           INTEGER NOT NULL REFERENCES pv_metadata(pv_id) ON DELETE RESTRICT,
    condition       TEXT    NOT NULL CHECK (condition IN ('HIHI', 'HI', 'LO', 'LOLO', 'DISCRETE')),
    current_version INTEGER NULL,        -- the approved version in force; NULL until one is approved
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (pv_id, condition)
);

-- ---------------------------------------------------------------------
-- Versions
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_config_versions (
    alarm_id            INTEGER NOT NULL REFERENCES alarm_config(alarm_id) ON DELETE RESTRICT,
    version             INTEGER NOT NULL CHECK (version >= 1),

    alarm_tag           TEXT    NOT NULL CHECK (alarm_tag <> ''),
    location            TEXT    NOT NULL DEFAULT '',
    setpoint            DOUBLE PRECISION NULL,        -- analog conditions
    trigger_state       SMALLINT NULL CHECK (trigger_state IN (0, 1)),  -- DISCRETE
    deadband            DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (deadband >= 0),
    on_delay_s          INTEGER NOT NULL DEFAULT 0 CHECK (on_delay_s  BETWEEN 0 AND 3600),
    off_delay_s         INTEGER NOT NULL DEFAULT 0 CHECK (off_delay_s BETWEEN 0 AND 3600),

    priority_id         INTEGER NOT NULL REFERENCES alarm_priorities(priority_id),
    class_id            INTEGER NOT NULL REFERENCES alarm_classes(class_id),

    message             TEXT    NOT NULL CHECK (message <> ''),
    description         TEXT    NOT NULL DEFAULT '',
    cause               TEXT    NOT NULL DEFAULT '',
    consequence         TEXT    NOT NULL DEFAULT '',
    operator_action     TEXT    NOT NULL DEFAULT '',
    owner               TEXT    NOT NULL DEFAULT '',
    response_time_s     INTEGER NULL CHECK (response_time_s > 0),

    shelving_allowed    BOOLEAN NOT NULL DEFAULT TRUE,
    suppression_allowed BOOLEAN NOT NULL DEFAULT FALSE,
    enabled             BOOLEAN NOT NULL DEFAULT TRUE,

    status              TEXT    NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'approved', 'rejected', 'superseded')),
    change_reason       TEXT    NOT NULL CHECK (change_reason <> ''),
    created_by          INTEGER NULL REFERENCES users(user_id) ON DELETE RESTRICT,  -- NULL = system import
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_by         INTEGER NULL REFERENCES users(user_id) ON DELETE RESTRICT,
    reviewed_at         TIMESTAMPTZ NULL,
    review_comment      TEXT    NOT NULL DEFAULT '',

    PRIMARY KEY (alarm_id, version),
    -- exactly one trigger: a limit for analog alarms, a state for discrete ones
    CHECK ((setpoint IS NULL) <> (trigger_state IS NULL))
);

-- At most one version waits for approval per alarm.
CREATE UNIQUE INDEX IF NOT EXISTS alarm_one_pending_idx
    ON alarm_config_versions (alarm_id) WHERE status = 'pending';

-- At most one approved version in force per alarm.
CREATE UNIQUE INDEX IF NOT EXISTS alarm_one_approved_idx
    ON alarm_config_versions (alarm_id) WHERE status = 'approved';

-- ---------------------------------------------------------------------
-- Approval history
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_approvals (
    approval_id   BIGSERIAL PRIMARY KEY,
    alarm_id      INTEGER NOT NULL,
    version       INTEGER NOT NULL,
    action        TEXT    NOT NULL CHECK (action IN ('submitted', 'approved', 'rejected')),
    user_id       INTEGER NOT NULL REFERENCES users(user_id) ON DELETE RESTRICT,
    username      TEXT    NOT NULL,                  -- name at the time of the action
    comment       TEXT    NOT NULL DEFAULT '',
    self_approved BOOLEAN NOT NULL DEFAULT FALSE,    -- approver is also the author (admin only)
    ts            TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (alarm_id, version) REFERENCES alarm_config_versions (alarm_id, version)
);
CREATE INDEX IF NOT EXISTS alarm_approvals_alarm_idx ON alarm_approvals (alarm_id, version);

-- ---------------------------------------------------------------------
-- Audit log (append-only)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alarm_audit_log (
    audit_id    BIGSERIAL PRIMARY KEY,
    ts          TIMESTAMPTZ NOT NULL DEFAULT now(),
    user_id     INTEGER NULL,                        -- not a foreign key: the record must outlive the account
    username    TEXT    NOT NULL DEFAULT '',         -- '' = no logged-in user
    action      TEXT    NOT NULL,                    -- e.g. config.submit, config.approve, config.reject
    target_type TEXT    NOT NULL DEFAULT '',         -- e.g. alarm_config
    target_id   TEXT    NOT NULL DEFAULT '',
    outcome     TEXT    NOT NULL CHECK (outcome IN ('success', 'denied', 'rejected', 'failed')),
    reason      TEXT    NOT NULL DEFAULT '',
    before_data JSONB   NULL,
    after_data  JSONB   NULL,
    client_ip   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alarm_audit_ts_idx     ON alarm_audit_log (ts DESC);
CREATE INDEX IF NOT EXISTS alarm_audit_target_idx ON alarm_audit_log (target_type, target_id);

CREATE OR REPLACE FUNCTION alarm_audit_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'alarm_audit_log is append-only (% not allowed)', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS alarm_audit_no_change   ON alarm_audit_log;
DROP TRIGGER IF EXISTS alarm_audit_no_truncate ON alarm_audit_log;
CREATE TRIGGER alarm_audit_no_change   BEFORE UPDATE OR DELETE ON alarm_audit_log
    FOR EACH ROW EXECUTE FUNCTION alarm_audit_immutable();
CREATE TRIGGER alarm_audit_no_truncate BEFORE TRUNCATE ON alarm_audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION alarm_audit_immutable();

-- ---------------------------------------------------------------------
-- Import the alarm limits that already exist in pv_metadata, so the AMS
-- starts with today's behaviour instead of an empty list.
--   alarm_high   -> HI        alarm_low -> LO       alarm_state -> DISCRETE
-- Imported as version 1, approved, priority Major (the old "priority"
-- column has no defined meaning for alarms), class Process.
-- REVIEW priorities after the import. The old columns are left in place;
-- they are retired in a later step, once the AMS matches the dashboard.
-- ---------------------------------------------------------------------
INSERT INTO alarm_config (pv_id, condition, current_version)
SELECT pv_id, 'HI', 1 FROM pv_metadata
 WHERE alarm_high IS NOT NULL AND register_address NOT BETWEEN 10001 AND 19999
UNION ALL
SELECT pv_id, 'LO', 1 FROM pv_metadata
 WHERE alarm_low IS NOT NULL AND register_address NOT BETWEEN 10001 AND 19999
UNION ALL
SELECT pv_id, 'DISCRETE', 1 FROM pv_metadata
 WHERE alarm_state IS NOT NULL AND register_address BETWEEN 10001 AND 19999
ON CONFLICT (pv_id, condition) DO NOTHING;

INSERT INTO alarm_config_versions
    (alarm_id, version, alarm_tag, setpoint, trigger_state, deadband,
     priority_id, class_id, message, description, enabled, status, change_reason)
SELECT c.alarm_id, 1, pv.pv_name,
       CASE c.condition WHEN 'HI' THEN pv.alarm_high WHEN 'LO' THEN pv.alarm_low END,
       CASE WHEN c.condition = 'DISCRETE' THEN pv.alarm_state END,
       CASE WHEN c.condition = 'DISCRETE' THEN 0 ELSE pv.deadband END,
       (SELECT priority_id FROM alarm_priorities WHERE name = 'Major'),
       (SELECT class_id    FROM alarm_classes    WHERE name = 'Process'),
       CASE c.condition
            WHEN 'HI' THEN pv.pv_name || ' high'
            WHEN 'LO' THEN pv.pv_name || ' low'
            ELSE pv.pv_name || ' ' || CASE WHEN pv.alarm_state = 1 THEN pv.state1_text ELSE pv.state0_text END
       END,
       pv.description, pv.enabled, 'approved',
       'Imported from pv_metadata (migration 004)'
FROM alarm_config c
JOIN pv_metadata pv ON pv.pv_id = c.pv_id
WHERE NOT EXISTS (SELECT 1 FROM alarm_config_versions v
                  WHERE v.alarm_id = c.alarm_id AND v.version = 1);

COMMIT;
