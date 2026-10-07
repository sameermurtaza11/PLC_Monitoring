-- =====================================================================
-- PLC Monitoring — Version 01 schema
--
-- Safe to run more than once (IF NOT EXISTS / ON CONFLICT everywhere).
-- NON-DESTRUCTIVE: pv_mapping, pv_data and raw_data are left untouched.
--
-- Run:  psql -U postgres -d plc_monitoring -f migrations/001_v01_schema.sql
-- =====================================================================

BEGIN;

-- ---------------------------------------------------------------------
-- 1. PLCs  (one row per physical PLC / Modbus TCP server)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS plc_metadata (
    plc_id            SERIAL PRIMARY KEY,
    plc_name          TEXT        NOT NULL UNIQUE,
    ip_address        INET        NOT NULL,
    port              INTEGER     NOT NULL DEFAULT 502
                                  CHECK (port BETWEEN 1 AND 65535),
    unit_id           SMALLINT    NOT NULL DEFAULT 1
                                  CHECK (unit_id BETWEEN 0 AND 255),
    scan_interval_ms  INTEGER     NOT NULL DEFAULT 1000
                                  CHECK (scan_interval_ms >= 100),
    timeout_ms        INTEGER     NOT NULL DEFAULT 3000
                                  CHECK (timeout_ms BETWEEN 100 AND 60000),
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    description       TEXT        NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------
-- 2. PVs  (register address is scoped to its PLC)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS pv_metadata (
    pv_id             SERIAL PRIMARY KEY,
    plc_id            INTEGER     NOT NULL
                                  REFERENCES plc_metadata(plc_id) ON DELETE RESTRICT,
    register_address  INTEGER     NOT NULL
                                  CHECK (register_address BETWEEN 30001 AND 39999
                                      OR register_address BETWEEN 40001 AND 49999),
    pv_name           TEXT        NOT NULL CHECK (btrim(pv_name) <> ''),
    system_name       TEXT        NOT NULL DEFAULT '',
    equipment         TEXT        NOT NULL DEFAULT '',
    description       TEXT        NOT NULL DEFAULT '',
    unit              TEXT        NOT NULL DEFAULT '',
    pv_min            DOUBLE PRECISION NOT NULL DEFAULT 0,
    pv_max            DOUBLE PRECISION NOT NULL DEFAULT 100,
    priority          SMALLINT    NOT NULL DEFAULT 3 CHECK (priority BETWEEN 1 AND 5),
    alarm_low         DOUBLE PRECISION NULL,
    alarm_high        DOUBLE PRECISION NULL,
    deadband          DOUBLE PRECISION NOT NULL DEFAULT 0 CHECK (deadband >= 0),
    enabled           BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT pv_range_valid          CHECK (pv_max > pv_min),
    CONSTRAINT pv_unique_register      UNIQUE (plc_id, register_address),
    CONSTRAINT pv_unique_name          UNIQUE (plc_id, pv_name)
);

CREATE INDEX IF NOT EXISTS pv_metadata_plc_idx ON pv_metadata (plc_id);

-- ---------------------------------------------------------------------
-- 3. Latest value per PV  (dashboard reads ONLY this small table)
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS pv_latest (
    pv_id         INTEGER     PRIMARY KEY
                              REFERENCES pv_metadata(pv_id) ON DELETE CASCADE,
    raw_value     INTEGER     NULL,
    pv_value      DOUBLE PRECISION NULL,
    quality       TEXT        NOT NULL DEFAULT 'GOOD',   -- GOOD | COMM_FAIL | READ_FAIL
    error         TEXT        NOT NULL DEFAULT '',
    ts            TIMESTAMPTZ NOT NULL,                  -- time of last update (good or bad)
    last_good_ts  TIMESTAMPTZ NULL                       -- time of last GOOD value
);

-- ---------------------------------------------------------------------
-- 4. History  (one row per stored sample)
--    PK (pv_id, ts) = fast per-PV time-range queries AND no duplicates.
--    ON DELETE RESTRICT: a PV with history cannot be deleted by accident;
--    disable it instead (enabled = false).
-- ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS pv_history (
    pv_id      INTEGER     NOT NULL
                           REFERENCES pv_metadata(pv_id) ON DELETE RESTRICT,
    ts         TIMESTAMPTZ NOT NULL,
    raw_value  INTEGER     NOT NULL,
    pv_value   DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (pv_id, ts)
);

-- ---------------------------------------------------------------------
-- 5. One-time import of the old pv_mapping (only if it exists).
--    All old PVs go under PLC-1 = the local simulator (127.0.0.1:5020).
-- ---------------------------------------------------------------------
DO $$
BEGIN
    IF to_regclass('public.pv_mapping') IS NOT NULL THEN

        INSERT INTO plc_metadata (plc_name, ip_address, port, description)
        VALUES ('PLC-1', '127.0.0.1', 5020, 'Imported from pv_mapping (local simulator)')
        ON CONFLICT (plc_name) DO NOTHING;

        INSERT INTO pv_metadata (plc_id, register_address, pv_name, unit, pv_min, pv_max)
        SELECT p.plc_id,
               m.register_address,
               m.pv_name,
               COALESCE(m.unit, ''),
               COALESCE(m.minimum, 0),
               COALESCE(m.maximum, 100)
        FROM pv_mapping m
        CROSS JOIN (SELECT plc_id FROM plc_metadata WHERE plc_name = 'PLC-1') p
        ON CONFLICT DO NOTHING;

        RAISE NOTICE 'pv_mapping imported into pv_metadata under PLC-1';
    END IF;
END $$;

COMMIT;
