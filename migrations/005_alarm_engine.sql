-- =====================================================================
-- PLC Monitoring — migration 005: AMS alarm engine (step 3)
--
-- NEW tables only. Nothing existing is changed. Safe to run more than once.
-- Needs 004 (alarm_config) first.
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f migrations/005_alarm_engine.sql
--
--   alarm_state           CURRENT state of each alarm (one row, updated)
--   alarm_events          every alarm transition (append-only history)
--   system_health_events  data-quality and engine faults (append-only)
--   ams_engine_status     one row: which engine instance runs, last heartbeat
--
-- State is kept as separate dimensions, not one status word:
--   process_active   is the process condition in alarm (true during the
--                    off-delay too: the alarm has not yet returned)
--   unacknowledged   operator has not acknowledged this activation (step 4)
--   data_valid       the process value can be trusted right now
--   (shelving, suppression and out-of-service live in their own tables, step 6)
-- =====================================================================

BEGIN;

CREATE TABLE IF NOT EXISTS alarm_state (
    alarm_id            INTEGER PRIMARY KEY REFERENCES alarm_config(alarm_id) ON DELETE RESTRICT,
    process_active      BOOLEAN NOT NULL DEFAULT FALSE,
    unacknowledged      BOOLEAN NOT NULL DEFAULT FALSE,
    cycle_no            INTEGER NOT NULL DEFAULT 0,        -- +1 at every activation
    on_pending_since    TIMESTAMPTZ NULL,                  -- condition true, on-delay running
    off_pending_since   TIMESTAMPTZ NULL,                  -- back inside deadband, off-delay running
    data_valid          BOOLEAN NOT NULL DEFAULT TRUE,
    active_version      INTEGER NULL,                      -- config version at activation
    activated_at        TIMESTAMPTZ NULL,                  -- source time of the sample
    activation_value    DOUBLE PRECISION NULL,
    returned_at         TIMESTAMPTZ NULL,
    last_value          DOUBLE PRECISION NULL,
    last_sample_ts      TIMESTAMPTZ NULL,
    delay_filtered_count INTEGER NOT NULL DEFAULT 0,       -- excursions swallowed by the on-delay
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Generic guard shared by the append-only tables below.
CREATE OR REPLACE FUNCTION ams_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only (% not allowed)', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TABLE IF NOT EXISTS alarm_events (
    event_id       BIGSERIAL PRIMARY KEY,
    alarm_id       INTEGER NOT NULL,
    config_version INTEGER NOT NULL,               -- version in force when it happened
    cycle_no       INTEGER NOT NULL,
    event_type     TEXT    NOT NULL CHECK (event_type IN (
                       'ACTIVATED', 'RETURNED_TO_NORMAL',
                       -- written by later steps:
                       'ACKNOWLEDGED', 'SHELVED', 'UNSHELVED', 'SUPPRESSED', 'UNSUPPRESSED',
                       'OUT_OF_SERVICE', 'RETURNED_TO_SERVICE')),
    event_ts       TIMESTAMPTZ NOT NULL,           -- source time (when the PLC value was read)
    ingested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),   -- when the server stored it
    process_value  DOUBLE PRECISION NULL,
    limit_value    DOUBLE PRECISION NULL,          -- the limit that applied
    source         TEXT    NOT NULL DEFAULT 'engine',    -- engine | user:<name> | system
    note           TEXT    NOT NULL DEFAULT '',
    FOREIGN KEY (alarm_id, config_version) REFERENCES alarm_config_versions (alarm_id, version)
);
CREATE INDEX IF NOT EXISTS alarm_events_alarm_idx ON alarm_events (alarm_id, event_id DESC);
CREATE INDEX IF NOT EXISTS alarm_events_ts_idx    ON alarm_events (event_ts DESC);

-- Idempotency: one activation and one return per alarm and cycle, however
-- often the same condition is evaluated or the engine restarts.
CREATE UNIQUE INDEX IF NOT EXISTS alarm_events_once_per_cycle_idx
    ON alarm_events (alarm_id, cycle_no, event_type)
    WHERE event_type IN ('ACTIVATED', 'RETURNED_TO_NORMAL');

DROP TRIGGER IF EXISTS alarm_events_no_change   ON alarm_events;
DROP TRIGGER IF EXISTS alarm_events_no_truncate ON alarm_events;
CREATE TRIGGER alarm_events_no_change   BEFORE UPDATE OR DELETE ON alarm_events
    FOR EACH ROW EXECUTE FUNCTION ams_append_only();
CREATE TRIGGER alarm_events_no_truncate BEFORE TRUNCATE ON alarm_events
    FOR EACH STATEMENT EXECUTE FUNCTION ams_append_only();

CREATE TABLE IF NOT EXISTS system_health_events (
    event_id  BIGSERIAL PRIMARY KEY,
    ts        TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind      TEXT NOT NULL CHECK (kind IN (
                  'DATA_BAD', 'DATA_RESTORED',
                  'ENGINE_STARTED', 'ENGINE_STOPPED', 'ENGINE_GAP',
                  'ENGINE_DB_ERROR', 'ENGINE_DB_RECOVERED')),
    severity  TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    component TEXT NOT NULL DEFAULT '',
    pv_id     INTEGER NULL,                         -- no foreign keys: history must outlive config
    alarm_id  INTEGER NULL,
    detail    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS system_health_ts_idx ON system_health_events (ts DESC);

DROP TRIGGER IF EXISTS system_health_no_change   ON system_health_events;
DROP TRIGGER IF EXISTS system_health_no_truncate ON system_health_events;
CREATE TRIGGER system_health_no_change   BEFORE UPDATE OR DELETE ON system_health_events
    FOR EACH ROW EXECUTE FUNCTION ams_append_only();
CREATE TRIGGER system_health_no_truncate BEFORE TRUNCATE ON system_health_events
    FOR EACH STATEMENT EXECUTE FUNCTION ams_append_only();

-- One row. Lets a restarted engine see how long alarm processing was not
-- running (alarms in that window were not evaluated) and report it.
CREATE TABLE IF NOT EXISTS ams_engine_status (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    instance       TEXT NOT NULL DEFAULT '',
    started_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_heartbeat TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;
