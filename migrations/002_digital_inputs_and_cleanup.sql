-- =====================================================================
-- PLC Monitoring — migration 002
--
--  A. Digital inputs: allow Modbus Discrete Inputs (10001-19999, 1 bit)
--  B. Cleanup: move old pv_data history into pv_history, then DROP the
--     legacy prototype tables pv_mapping, pv_data, raw_data.
--  C. Remove stale rows.
--
-- Run AFTER 001. Safe to run more than once.
--
-- !! B deletes tables. Take a backup first (one line):
--    pg_dump -U postgres -d plc_monitoring -t pv_mapping -t pv_data -t raw_data -f legacy_backup.sql
--
-- Run:  psql -U postgres -d plc_monitoring -f migrations/002_digital_inputs_and_cleanup.sql
-- =====================================================================

BEGIN;

-- ---------------------------------------------------------------------
-- A. Digital inputs
--    Register type is derived from the address (Modbus convention):
--      10001-19999  Discrete Input    1 bit   FC02  → value 0/1, no scaling
--      30001-39999  Input Register   16 bit   FC04  → scaled pv_min..pv_max
--      40001-49999  Holding Register 16 bit   FC03  → scaled pv_min..pv_max
-- ---------------------------------------------------------------------
ALTER TABLE pv_metadata DROP CONSTRAINT IF EXISTS pv_metadata_register_address_check;
ALTER TABLE pv_metadata ADD  CONSTRAINT pv_metadata_register_address_check
    CHECK (register_address BETWEEN 10001 AND 19999
        OR register_address BETWEEN 30001 AND 39999
        OR register_address BETWEEN 40001 AND 49999);

-- Text shown for each state of a digital input, e.g. STOPPED / RUNNING.
ALTER TABLE pv_metadata ADD COLUMN IF NOT EXISTS state0_text TEXT NOT NULL DEFAULT 'OFF';
ALTER TABLE pv_metadata ADD COLUMN IF NOT EXISTS state1_text TEXT NOT NULL DEFAULT 'ON';

-- Digital alarm: NULL = none, 0 = alarm when OFF, 1 = alarm when ON.
ALTER TABLE pv_metadata ADD COLUMN IF NOT EXISTS alarm_state SMALLINT NULL;
ALTER TABLE pv_metadata DROP CONSTRAINT IF EXISTS pv_alarm_state_valid;
ALTER TABLE pv_metadata ADD  CONSTRAINT pv_alarm_state_valid CHECK (alarm_state IN (0, 1));

-- ---------------------------------------------------------------------
-- B. Legacy tables
-- ---------------------------------------------------------------------
DO $$
DECLARE
    moved     bigint := 0;
    unmatched bigint := 0;
    n         bigint;
BEGIN
    -- B1. pv_data → pv_history.
    --     Old rows have only a register number; 001 imported the old
    --     pv_mapping as PLC-1, so match on PLC-1 + register_address.
    IF to_regclass('public.pv_data') IS NOT NULL THEN
        INSERT INTO pv_history (pv_id, ts, raw_value, pv_value)
        SELECT m.pv_id, d.timestamp::timestamptz, d.raw_value, d.pv_value
        FROM pv_data d
        JOIN plc_metadata p ON p.plc_name = 'PLC-1'
        JOIN pv_metadata  m ON m.plc_id = p.plc_id AND m.register_address = d.register_address
        WHERE d.raw_value IS NOT NULL AND d.pv_value IS NOT NULL AND d.timestamp IS NOT NULL
        ON CONFLICT (pv_id, ts) DO NOTHING;
        GET DIAGNOSTICS moved = ROW_COUNT;

        SELECT count(*) INTO n FROM pv_data;
        unmatched := n - moved;
        RAISE NOTICE 'pv_data: % rows, % moved to pv_history, % unmatched/duplicate (dropped)', n, moved, unmatched;
        DROP TABLE pv_data;
    END IF;

    -- B2. raw_data: first prototype, raw counts only, superseded by pv_data.
    IF to_regclass('public.raw_data') IS NOT NULL THEN
        SELECT count(*) INTO n FROM raw_data;
        RAISE NOTICE 'raw_data: % rows dropped', n;
        DROP TABLE raw_data;
    END IF;

    -- B3. pv_mapping: already copied into pv_metadata by 001.
    IF to_regclass('public.pv_mapping') IS NOT NULL THEN
        SELECT count(*) INTO n FROM pv_mapping;
        RAISE NOTICE 'pv_mapping: % rows dropped (config lives in pv_metadata)', n;
        DROP TABLE pv_mapping;
    END IF;

    -- -----------------------------------------------------------------
    -- C. Stale rows
    -- -----------------------------------------------------------------
    -- Latest values of disabled PVs / PVs of disabled PLCs are frozen
    -- snapshots nobody updates any more; the dashboard would show old data.
    DELETE FROM pv_latest l
    USING pv_metadata m, plc_metadata p
    WHERE l.pv_id = m.pv_id AND p.plc_id = m.plc_id
      AND (NOT m.enabled OR NOT p.enabled);
    GET DIAGNOSTICS n = ROW_COUNT;
    RAISE NOTICE 'pv_latest: % stale rows of disabled PVs removed', n;
END $$;

-- 001 references pv_mapping only if it exists, so re-running 001 stays safe.

COMMIT;

-- Reclaim disk space of the dropped tables' dead rows in pv_history etc.
VACUUM (ANALYZE) pv_history;
VACUUM (ANALYZE) pv_latest;
