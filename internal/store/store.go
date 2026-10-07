// Package store contains every SQL query the application runs.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// ---------------------------------------------------------------------
// Configuration (read by the acquisition manager)
// ---------------------------------------------------------------------

const pvColumns = `
	pv.pv_id, pv.plc_id, pv.register_address, pv.pv_name, pv.system_name,
	pv.equipment, pv.description, pv.unit, pv.pv_min, pv.pv_max,
	pv.priority, pv.alarm_low, pv.alarm_high, pv.deadband,
	pv.state0_text, pv.state1_text, pv.alarm_state`

func scanPV(row pgx.Row, pv *PV, extra ...any) error {
	dest := []any{
		&pv.ID, &pv.PLCID, &pv.RegisterAddress, &pv.Name, &pv.SystemName,
		&pv.Equipment, &pv.Description, &pv.Unit, &pv.Min, &pv.Max,
		&pv.Priority, &pv.AlarmLow, &pv.AlarmHigh, &pv.Deadband,
		&pv.State0Text, &pv.State1Text, &pv.AlarmState,
	}
	return row.Scan(append(dest, extra...)...)
}

// LoadAcquisitionConfig returns every enabled PLC with its enabled PVs.
func (s *Store) LoadAcquisitionConfig(ctx context.Context) ([]PLC, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT plc_id, plc_name, host(ip_address), port, unit_id,
		       scan_interval_ms, timeout_ms
		FROM plc_metadata
		WHERE enabled
		ORDER BY plc_id`)
	if err != nil {
		return nil, fmt.Errorf("query plc_metadata: %w", err)
	}
	plcs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PLC, error) {
		var p PLC
		err := r.Scan(&p.ID, &p.Name, &p.IP, &p.Port, &p.UnitID, &p.ScanIntervalMS, &p.TimeoutMS)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("read plc_metadata: %w", err)
	}

	rows, err = s.pool.Query(ctx, `
		SELECT `+pvColumns+`
		FROM pv_metadata pv
		JOIN plc_metadata p ON p.plc_id = pv.plc_id
		WHERE pv.enabled AND p.enabled
		ORDER BY pv.plc_id, pv.register_address`)
	if err != nil {
		return nil, fmt.Errorf("query pv_metadata: %w", err)
	}
	pvs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (PV, error) {
		var pv PV
		err := scanPV(r, &pv)
		return pv, err
	})
	if err != nil {
		return nil, fmt.Errorf("read pv_metadata: %w", err)
	}

	// Attach each PV to its own PLC. Register addresses are only unique
	// inside one PLC, so the PLC id is always part of the lookup.
	byID := make(map[int]*PLC, len(plcs))
	for i := range plcs {
		byID[plcs[i].ID] = &plcs[i]
	}
	for _, pv := range pvs {
		if p := byID[pv.PLCID]; p != nil {
			p.PVs = append(p.PVs, pv)
		}
	}
	return plcs, nil
}

// ---------------------------------------------------------------------
// Writing acquired data
// ---------------------------------------------------------------------

// WriteScan stores one scan's results in a single transaction:
//   - latest:   good values  → pv_latest (upsert)
//   - history:  good values  → pv_history (insert, duplicates ignored)
//   - failures: bad reads    → pv_latest quality/error only (last value kept)
func (s *Store) WriteScan(ctx context.Context, latest, history []Sample, failures []Failure) error {
	b := &pgx.Batch{}

	if len(latest) > 0 {
		ids, ts, raws, vals := columns(latest)
		b.Queue(`
			INSERT INTO pv_latest (pv_id, ts, raw_value, pv_value, quality, error, last_good_ts)
			SELECT id, t, r, v, 'GOOD', '', t
			FROM unnest($1::int[], $2::timestamptz[], $3::int[], $4::float8[]) AS x(id, t, r, v)
			JOIN pv_metadata m ON m.pv_id = x.id -- skip PVs deleted meanwhile
			ON CONFLICT (pv_id) DO UPDATE SET
				ts = EXCLUDED.ts, raw_value = EXCLUDED.raw_value, pv_value = EXCLUDED.pv_value,
				quality = 'GOOD', error = '', last_good_ts = EXCLUDED.ts`,
			ids, ts, raws, vals)
	}

	if len(history) > 0 {
		ids, ts, raws, vals := columns(history)
		b.Queue(`
			INSERT INTO pv_history (pv_id, ts, raw_value, pv_value)
			SELECT x.* FROM unnest($1::int[], $2::timestamptz[], $3::int[], $4::float8[]) AS x(id, t, r, v)
			JOIN pv_metadata m ON m.pv_id = x.id -- skip PVs deleted meanwhile
			ON CONFLICT (pv_id, ts) DO NOTHING`,
			ids, ts, raws, vals)
	}

	if len(failures) > 0 {
		ids := make([]int32, len(failures))
		ts := make([]time.Time, len(failures))
		qs := make([]string, len(failures))
		errs := make([]string, len(failures))
		for i, f := range failures {
			ids[i], ts[i], qs[i], errs[i] = int32(f.PVID), f.TS, f.Quality, f.Error
		}
		b.Queue(`
			INSERT INTO pv_latest (pv_id, ts, quality, error)
			SELECT x.* FROM unnest($1::int[], $2::timestamptz[], $3::text[], $4::text[]) AS x(id, t, q, e)
			JOIN pv_metadata m ON m.pv_id = x.id -- skip PVs deleted meanwhile
			ON CONFLICT (pv_id) DO UPDATE SET
				ts = EXCLUDED.ts, quality = EXCLUDED.quality, error = EXCLUDED.error`,
			ids, ts, qs, errs)
	}

	if b.Len() == 0 {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return tx.SendBatch(ctx, b).Close()
	})
}

func columns(samples []Sample) (ids []int32, ts []time.Time, raws []int32, vals []float64) {
	ids = make([]int32, len(samples))
	ts = make([]time.Time, len(samples))
	raws = make([]int32, len(samples))
	vals = make([]float64, len(samples))
	for i, s := range samples {
		ids[i], ts[i], raws[i], vals[i] = int32(s.PVID), s.TS, int32(s.Raw), s.Value
	}
	return
}

// ---------------------------------------------------------------------
// Reading for the web UI
// ---------------------------------------------------------------------

const latestSelect = `
	SELECT ` + pvColumns + `,
	       p.plc_name, p.scan_interval_ms,
	       l.raw_value, l.pv_value, l.quality, COALESCE(l.error, ''), l.ts, l.last_good_ts
	FROM pv_metadata pv
	JOIN plc_metadata p ON p.plc_id = pv.plc_id
	LEFT JOIN pv_latest l ON l.pv_id = pv.pv_id`

func scanLatest(r pgx.Row, row *LatestRow) error {
	return scanPV(r, &row.PV,
		&row.PLCName, &row.ScanIntervalMS,
		&row.Raw, &row.Value, &row.Quality, &row.Error, &row.TS, &row.LastGoodTS)
}

// LatestValues returns every enabled PV with its latest value (dashboard).
func (s *Store) LatestValues(ctx context.Context) ([]LatestRow, error) {
	rows, err := s.pool.Query(ctx, latestSelect+`
		WHERE pv.enabled AND p.enabled
		ORDER BY p.plc_name, pv.register_address`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (LatestRow, error) {
		var row LatestRow
		err := scanLatest(r, &row)
		return row, err
	})
}

// LatestValue returns one PV (detail modal). pgx.ErrNoRows if not found.
func (s *Store) LatestValue(ctx context.Context, pvID int) (LatestRow, error) {
	var row LatestRow
	err := scanLatest(s.pool.QueryRow(ctx, latestSelect+` WHERE pv.pv_id = $1`, pvID), &row)
	return row, err
}

// IsPermanent reports whether a write error will fail again on retry
// (integrity violation) rather than being transient (DB down, timeout).
func IsPermanent(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && strings.HasPrefix(pg.Code, "23")
}
