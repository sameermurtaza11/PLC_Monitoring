# PLC Monitoring — Version 01

Instrument → PLC → Modbus TCP → Go acquisition → PostgreSQL → Gin → HTMX → ECharts

## Layout

```
cmd/server/main.go          starts DB pool, acquisition manager, Gin server
internal/config/            env / .env settings (DB URL, HTTP port) — NOT PLC config
internal/store/             all SQL (config load, scan writes, dashboard, history, CRUD)
internal/plc/               Modbus: address parsing (1xxxx/3xxxx/4xxxx), block reads, TCP client
internal/acquisition/       Manager (one worker per PLC) + Worker (scan loop) + scaling
internal/web/               Gin handlers, templates/, static/ (htmx + echarts served locally)
migrations/001_v01_schema.sql
migrations/002_digital_inputs_and_cleanup.sql
sql/demo_multi_plc.sql      demo: 3 PLCs on 127.0.0.1/.2/.3 with AI + DI points
```

## Point types (derived from the address — Modbus convention)

| Address | Modbus type | Size | Function | Value in the app |
|---|---|---|---|---|
| 10001–19999 | Discrete Input (DI) | 1 bit | FC02 | 0 / 1, shown as state text (e.g. STOPPED / RUNNING) |
| 30001–39999 | Input Register (AI) | 16 bit | FC04 | raw 0–65535 scaled to pv_min…pv_max |
| 40001–49999 | Holding Register | 16 bit | FC03 | raw 0–65535 scaled to pv_min…pv_max |

`PV = pv_min + (raw / 65535) × (pv_max − pv_min)` — digital inputs are not scaled.

Digital inputs:
- `state0_text` / `state1_text` — what 0 and 1 mean (OFF/ON, CLOSED/OPEN, …)
- `alarm_state` — NULL = no alarm, 0 or 1 = alarm in that state
- History is stored on **every state change** (+ one row per minute), not every scan.
  Nothing is lost: each transition is stored with the timestamp of the scan that saw it.

## Database tables (after 002)

| Table | Purpose |
|---|---|
| `plc_metadata` | one row per PLC: name, IP, port, unit id, scan interval, timeout, enabled |
| `pv_metadata`  | one row per PV: `plc_id` + register, name, system, equipment, unit, scaling, alarms, deadband, state texts |
| `pv_latest`    | last value + quality per PV (dashboard reads only this) |
| `pv_history`   | timestamped samples, PK `(pv_id, ts)` |

Constraints: `UNIQUE(plc_id, register_address)`, `UNIQUE(plc_id, pv_name)`, `pv_max > pv_min`,
register must be 10001–19999, 30001–39999 or 40001–49999, `alarm_state IN (0,1)`.

Old prototype tables `pv_mapping`, `pv_data`, `raw_data` are removed by 002
(matching `pv_data` rows are first moved into `pv_history`).

## Setup / upgrade (Windows, VS Code terminal, inside D:\PLC_Monitoring)

```powershell
# 0. Backup of the old tables (002 drops them)
pg_dump -U postgres -d plc_monitoring -t pv_mapping -t pv_data -t raw_data -f legacy_backup.sql

# 1. Schema (both safe to re-run)
psql -U postgres -d plc_monitoring -f migrations/001_v01_schema.sql
psql -U postgres -d plc_monitoring -f migrations/002_digital_inputs_and_cleanup.sql

# 2. Demo: 3 PLCs with different IPs (AI + DI)
psql -U postgres -d plc_monitoring -f sql/demo_multi_plc.sql

# 3. Simulated PLCs 127.0.0.1 / .2 / .3 : 502
cd D:\plc_simulator
.\plc_service.ps1 start
cd D:\PLC_Monitoring

# 4. App
go test ./...
go run ./cmd/server          # → http://localhost:8080
```

`.\plc_service.ps1 status` / `stop` / `restart` manage all simulated PLCs.
Every 127.x.x.x address is local loopback on Windows, so each simulated PLC has its own IP
without extra network adapters. A real PLC just needs its real IP in `plc_metadata`.

Optional: `copy .env.example .env` and set `DATABASE_URL` if your DB needs a password.

## Acquisition behaviour

- One goroutine per enabled PLC; a failed PLC only affects its own PVs (`COMM_FAIL`).
- Reconnect with backoff 1 s → 2 s → … → 30 s.
- Adjacent points of the same type are read in one request
  (registers: ≤125, gaps ≤8; discrete inputs: ≤2000 bits, gaps ≤64).
- Modbus exception (e.g. illegal address) → `READ_FAIL`, connection kept.
- Each scan = one DB transaction (latest upsert + history insert + failures).
- DB down → history buffered in memory (≤ 200 000 samples per PLC), written on recovery.
- Analog `deadband > 0` → history only when value moves ≥ deadband (+ one row per minute).
- Saving in `/config` reloads acquisition immediately; direct SQL edits within `CONFIG_RELOAD` (10 s).
- Dashboard status: `No data`, `COMM_FAIL`, `READ_FAIL`, `STALE`, `HIGH`, `LOW`, `ALARM` (digital), `Normal`.

## Web UI

| Page | What it does |
|---|---|
| `/` Dashboard | latest value of every PV (DI as state text), refresh 2 s; **More** opens the PV detail |
| PV detail | metadata; analog: live gauge; digital: status lamp; history trend + table; compare up to 4 PVs |
| `/config` | add / edit / delete PLCs and PVs; the PV form switches between analog and digital fields by address |

### Trend rules
- Ranges 15m … 7d, zoom, refresh 10 s. >1000 samples → averaged buckets with min/max
  (for a DI, Avg = fraction of time ON).
- Digital PVs are drawn as step lines.
- Same unit → real values. Different units, or digital + analog → % of each PV's range
  (digital: 0 % = OFF, 100 % = ON). One y-axis only.

### Delete rules
- PLC with PVs → blocked. PV with history → blocked unless "Delete PV and its history" is confirmed.
  Disabling keeps the history.
