package store

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// PVOptions lists every enabled PV for the compare drop-down.
func (s *Store) PVOptions(ctx context.Context) ([]PVOption, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pv.pv_id, p.plc_name, pv.pv_name, pv.unit
		FROM pv_metadata pv
		JOIN plc_metadata p ON p.plc_id = pv.plc_id
		WHERE pv.enabled
		ORDER BY p.plc_name, pv.pv_name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PVOption, error) {
		var o PVOption
		err := r.Scan(&o.ID, &o.PLCName, &o.Name, &o.Unit)
		return o, err
	})
}

// History returns the samples of one PV in [from, to).
//
// If there are more than maxPoints samples, they are averaged into equal
// time buckets so the browser never receives more than ~maxPoints rows.
// (A 7-day range at a 1 s scan rate is 604 800 samples.)
// Min/Max per bucket are kept so short spikes are not hidden by the average.
func (s *Store) History(ctx context.Context, pvID int, from, to time.Time, maxPoints int) (Series, error) {
	pv, err := s.LatestValue(ctx, pvID)
	if err != nil {
		return Series{}, err
	}
	series := Series{PV: pv}

	var count int
	err = s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pv_history WHERE pv_id = $1 AND ts >= $2 AND ts < $3`,
		pvID, from, to).Scan(&count)
	if err != nil {
		return Series{}, fmt.Errorf("count history: %w", err)
	}

	var rows pgx.Rows
	if count <= maxPoints {
		rows, err = s.pool.Query(ctx, `
			SELECT ts, pv_value, pv_value, pv_value, 1
			FROM pv_history
			WHERE pv_id = $1 AND ts >= $2 AND ts < $3
			ORDER BY ts`, pvID, from, to)
	} else {
		bucket := int(math.Ceil(to.Sub(from).Seconds() / float64(maxPoints)))
		series.BucketSeconds = bucket
		// floor(epoch / bucket) * bucket groups samples into fixed windows.
		// (Works on every PostgreSQL version; date_bin needs PG 14+.)
		rows, err = s.pool.Query(ctx, `
			SELECT to_timestamp(floor(extract(epoch FROM ts) / $4) * $4) AS bucket,
			       avg(pv_value), min(pv_value), max(pv_value), count(*)
			FROM pv_history
			WHERE pv_id = $1 AND ts >= $2 AND ts < $3
			GROUP BY bucket
			ORDER BY bucket`, pvID, from, to, bucket)
	}
	if err != nil {
		return Series{}, fmt.Errorf("query history: %w", err)
	}
	series.Points, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (HistoryPoint, error) {
		var p HistoryPoint
		err := r.Scan(&p.TS, &p.Value, &p.Min, &p.Max, &p.Count)
		return p, err
	})
	if err != nil {
		return Series{}, fmt.Errorf("read history: %w", err)
	}
	return series, nil
}
