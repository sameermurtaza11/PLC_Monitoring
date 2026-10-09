-- =====================================================================
-- PLC Monitoring — migration 006: AMS acknowledgement (step 4)
--
-- Only ADDS columns/an index; nothing existing changes. Safe to re-run.
-- Needs 005 (alarm_state, alarm_events).
--
-- Run:  $env:PGCLIENTENCODING = "UTF8"
--       psql -U postgres -d plc_monitoring -f migrations/006_alarm_ack.sql
--
-- Acknowledgement is its own dimension of alarm_state (`unacknowledged`,
-- set by the engine at each activation). This migration adds who and when,
-- and makes "one acknowledgement per activation" a database guarantee.
-- =====================================================================

BEGIN;

ALTER TABLE alarm_state
    ADD COLUMN IF NOT EXISTS acknowledged_by INTEGER NULL REFERENCES users(user_id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS acknowledged_at TIMESTAMPTZ NULL;

-- An activation (alarm_id + cycle_no) can be acknowledged only once, however
-- many operators click at the same time.
CREATE UNIQUE INDEX IF NOT EXISTS alarm_events_ack_once_idx
    ON alarm_events (alarm_id, cycle_no)
    WHERE event_type = 'ACKNOWLEDGED';

COMMIT;
