-- =====================================================================
-- Demo configuration: 3 PLCs with DIFFERENT IP addresses, each with
-- analog inputs (Input Registers 3xxxx) and digital inputs
-- (Discrete Inputs 1xxxx).
--
-- Matches D:\plc_simulator\plc_service.ps1  (127.0.0.1 / .2 / .3, port 502)
-- Needs migrations 001 + 002. Safe to re-run (upserts by name).
--
-- Run:  psql -U postgres -d plc_monitoring -f sql/demo_multi_plc.sql
-- The running app picks it up within CONFIG_RELOAD (10 s).
-- =====================================================================

BEGIN;

INSERT INTO plc_metadata (plc_name, ip_address, port, unit_id, scan_interval_ms, description) VALUES
    ('PLC-1', '127.0.0.1', 502, 1, 1000, 'Boiler house (simulator)'),
    ('PLC-2', '127.0.0.2', 502, 1, 1000, 'Pump station (simulator)'),
    ('PLC-3', '127.0.0.3', 502, 1,  500, 'Tank farm (simulator)')
ON CONFLICT (plc_name) DO UPDATE SET
    ip_address = EXCLUDED.ip_address, port = EXCLUDED.port,
    scan_interval_ms = EXCLUDED.scan_interval_ms, description = EXCLUDED.description,
    enabled = TRUE, updated_at = now();

-- One row per PV. Same register numbers on every PLC on purpose:
-- 30001 on PLC-1 and 30001 on PLC-2 are different physical signals.
WITH pv (plc, reg, name, sys, equip, descr, unit, lo, hi, alo, ahi, s0, s1, astate) AS (VALUES
    -- PLC-1 Boiler house
    ('PLC-1', 30001, 'PT101',  'Boiler', 'Drum',        'Drum pressure',          'bar',  0,  16,  NULL, 12.0, NULL, NULL, NULL),
    ('PLC-1', 30002, 'TT101',  'Boiler', 'Drum',        'Drum temperature',       '°C',   0, 250,  NULL, 200,  NULL, NULL, NULL),
    ('PLC-1', 30003, 'FT101',  'Boiler', 'Feedwater',   'Feedwater flow',         'm3/h', 0,  60,  5.0,  NULL, NULL, NULL, NULL),
    ('PLC-1', 30004, 'LT101',  'Boiler', 'Drum',        'Drum level',             '%',    0, 100,  20,   80,   NULL, NULL, NULL),
    ('PLC-1', 10001, 'XS101',  'Boiler', 'Burner',      'Burner running',         '',     0,   1,  NULL, NULL, 'STOPPED', 'RUNNING', NULL),
    ('PLC-1', 10002, 'ZS101',  'Boiler', 'Feedwater',   'Feed valve open',        '',     0,   1,  NULL, NULL, 'CLOSED',  'OPEN',    NULL),
    ('PLC-1', 10016, 'XA101',  'Boiler', 'Burner',      'Flame failure trip',     '',     0,   1,  NULL, NULL, 'NORMAL',  'TRIP',    1),
    -- PLC-2 Pump station
    ('PLC-2', 30001, 'PT201',  'Pumps',  'P-201',       'Discharge pressure',     'bar',  0,  10,  1.0,  8.5,  NULL, NULL, NULL),
    ('PLC-2', 30002, 'FT201',  'Pumps',  'Header',      'Header flow',            'm3/h', 0, 400,  NULL, NULL, NULL, NULL, NULL),
    ('PLC-2', 30003, 'IT201',  'Pumps',  'P-201',       'Motor current',          'A',    0, 120,  NULL, 100,  NULL, NULL, NULL),
    ('PLC-2', 10001, 'XS201',  'Pumps',  'P-201',       'Pump P-201 running',     '',     0,   1,  NULL, NULL, 'STOPPED', 'RUNNING', NULL),
    ('PLC-2', 10002, 'XS202',  'Pumps',  'P-202',       'Pump P-202 running',     '',     0,   1,  NULL, NULL, 'STOPPED', 'RUNNING', NULL),
    ('PLC-2', 10016, 'XA201',  'Pumps',  'P-201',       'Motor overload',         '',     0,   1,  NULL, NULL, 'NORMAL',  'TRIP',    1),
    -- PLC-3 Tank farm
    ('PLC-3', 30001, 'LT301',  'Tanks',  'TK-301',      'Tank TK-301 level',      '%',    0, 100,  10,   90,   NULL, NULL, NULL),
    ('PLC-3', 30002, 'LT302',  'Tanks',  'TK-302',      'Tank TK-302 level',      '%',    0, 100,  10,   90,   NULL, NULL, NULL),
    ('PLC-3', 30003, 'TT301',  'Tanks',  'TK-301',      'Tank TK-301 temperature','°C', -20,  80,  NULL, 60,   NULL, NULL, NULL),
    ('PLC-3', 10001, 'LSH301', 'Tanks',  'TK-301',      'High level switch',      '',     0,   1,  NULL, NULL, 'OK',      'HIGH',    1),
    ('PLC-3', 10002, 'ZS301',  'Tanks',  'TK-301',      'Inlet valve open',       '',     0,   1,  NULL, NULL, 'CLOSED',  'OPEN',    NULL)
)
INSERT INTO pv_metadata (plc_id, register_address, pv_name, system_name, equipment, description,
                         unit, pv_min, pv_max, alarm_low, alarm_high,
                         state0_text, state1_text, alarm_state)
SELECT p.plc_id, pv.reg, pv.name, pv.sys, pv.equip, pv.descr,
       pv.unit, pv.lo, pv.hi, pv.alo, pv.ahi,
       COALESCE(pv.s0, 'OFF'), COALESCE(pv.s1, 'ON'), pv.astate
FROM pv
JOIN plc_metadata p ON p.plc_name = pv.plc
-- Existing PVs (same PLC + register, or same PLC + name) are left as they are,
-- so PVs you already configured keep their names, scaling and history.
ON CONFLICT DO NOTHING;

COMMIT;

SELECT p.plc_name, host(p.ip_address) AS ip, p.port,
       count(*) FILTER (WHERE m.register_address BETWEEN 30001 AND 39999) AS analog,
       count(*) FILTER (WHERE m.register_address BETWEEN 10001 AND 19999) AS digital
FROM plc_metadata p LEFT JOIN pv_metadata m ON m.plc_id = p.plc_id
GROUP BY 1, 2, 3 ORDER BY 1;
