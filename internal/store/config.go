package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PLCRecord is a full plc_metadata row for the configuration page.
type PLCRecord struct {
	ID             int
	Name           string
	IP             string
	Port           int
	UnitID         int
	ScanIntervalMS int
	TimeoutMS      int
	Enabled        bool
	Description    string
	PVCount        int // read-only, for the list
}

// PVRecord is a full pv_metadata row for the configuration page.
type PVRecord struct {
	PV
	PLCName string
	Enabled bool
}

// ErrInUse is returned when a delete is blocked by dependent rows.
var ErrInUse = errors.New("in use")

// ConstraintError is a unique/check violation reported by PostgreSQL,
// translated so the form can show it next to the right field.
type ConstraintError struct {
	Constraint string
	Message    string
}

func (e *ConstraintError) Error() string { return e.Message }

// asConstraintError turns PostgreSQL integrity errors into ConstraintError.
// The database is the final guard: even if validation in Go misses a case,
// a duplicate register on the same PLC can never be stored.
func asConstraintError(err error) error {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch pg.Code {
	case "23505", "23514", "23503": // unique, check, foreign key
		return &ConstraintError{Constraint: pg.ConstraintName, Message: pg.Message}
	}
	return err
}

// ---------------------------------------------------------------------
// PLCs
// ---------------------------------------------------------------------

func (s *Store) ListPLCs(ctx context.Context) ([]PLCRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT p.plc_id, p.plc_name, host(p.ip_address), p.port, p.unit_id,
		       p.scan_interval_ms, p.timeout_ms, p.enabled, p.description,
		       (SELECT count(*) FROM pv_metadata pv WHERE pv.plc_id = p.plc_id)
		FROM plc_metadata p
		ORDER BY p.plc_name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PLCRecord, error) {
		var p PLCRecord
		err := r.Scan(&p.ID, &p.Name, &p.IP, &p.Port, &p.UnitID,
			&p.ScanIntervalMS, &p.TimeoutMS, &p.Enabled, &p.Description, &p.PVCount)
		return p, err
	})
}

func (s *Store) GetPLC(ctx context.Context, id int) (PLCRecord, error) {
	var p PLCRecord
	err := s.pool.QueryRow(ctx, `
		SELECT plc_id, plc_name, host(ip_address), port, unit_id,
		       scan_interval_ms, timeout_ms, enabled, description
		FROM plc_metadata WHERE plc_id = $1`, id).
		Scan(&p.ID, &p.Name, &p.IP, &p.Port, &p.UnitID,
			&p.ScanIntervalMS, &p.TimeoutMS, &p.Enabled, &p.Description)
	return p, err
}

// SavePLC inserts (p.ID == 0) or updates a PLC. Returns the plc_id.
func (s *Store) SavePLC(ctx context.Context, p PLCRecord) (int, error) {
	var err error
	if p.ID == 0 {
		err = s.pool.QueryRow(ctx, `
			INSERT INTO plc_metadata
				(plc_name, ip_address, port, unit_id, scan_interval_ms, timeout_ms, enabled, description)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING plc_id`,
			p.Name, p.IP, p.Port, p.UnitID, p.ScanIntervalMS, p.TimeoutMS, p.Enabled, p.Description).
			Scan(&p.ID)
	} else {
		var tag pgconn.CommandTag
		tag, err = s.pool.Exec(ctx, `
			UPDATE plc_metadata SET
				plc_name = $2, ip_address = $3, port = $4, unit_id = $5,
				scan_interval_ms = $6, timeout_ms = $7, enabled = $8, description = $9,
				updated_at = now()
			WHERE plc_id = $1`,
			p.ID, p.Name, p.IP, p.Port, p.UnitID, p.ScanIntervalMS, p.TimeoutMS, p.Enabled, p.Description)
		if err == nil && tag.RowsAffected() == 0 {
			err = pgx.ErrNoRows
		}
	}
	return p.ID, asConstraintError(err)
}

// DeletePLC removes a PLC. Blocked (ErrInUse) while PVs still belong to it,
// so a PLC can never be deleted together with its PV configuration by accident.
func (s *Store) DeletePLC(ctx context.Context, id int) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM plc_metadata WHERE plc_id = $1`, id)
	if ce := asConstraintError(err); ce != err {
		return fmt.Errorf("%w: PLC still has PVs", ErrInUse)
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ---------------------------------------------------------------------
// PVs
// ---------------------------------------------------------------------

const pvRecordSelect = `
	SELECT ` + pvColumns + `, p.plc_name, pv.enabled
	FROM pv_metadata pv
	JOIN plc_metadata p ON p.plc_id = pv.plc_id`

func scanPVRecord(r pgx.Row, rec *PVRecord) error {
	return scanPV(r, &rec.PV, &rec.PLCName, &rec.Enabled)
}

// ListPVs returns all PVs (enabled or not); plcID = 0 means all PLCs.
func (s *Store) ListPVs(ctx context.Context, plcID int) ([]PVRecord, error) {
	rows, err := s.pool.Query(ctx, pvRecordSelect+`
		WHERE $1 = 0 OR pv.plc_id = $1
		ORDER BY p.plc_name, pv.register_address`, plcID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PVRecord, error) {
		var rec PVRecord
		err := scanPVRecord(r, &rec)
		return rec, err
	})
}

func (s *Store) GetPV(ctx context.Context, id int) (PVRecord, error) {
	var rec PVRecord
	err := scanPVRecord(s.pool.QueryRow(ctx, pvRecordSelect+` WHERE pv.pv_id = $1`, id), &rec)
	return rec, err
}

// SavePV inserts (ID == 0) or updates a PV. Returns the pv_id.
func (s *Store) SavePV(ctx context.Context, r PVRecord) (int, error) {
	args := []any{r.PLCID, r.RegisterAddress, r.Name, r.SystemName, r.Equipment,
		r.Description, r.Unit, r.Min, r.Max, r.Priority, r.AlarmLow, r.AlarmHigh,
		r.Deadband, r.Enabled, r.State0Text, r.State1Text, r.AlarmState}
	var err error
	if r.ID == 0 {
		err = s.pool.QueryRow(ctx, `
			INSERT INTO pv_metadata
				(plc_id, register_address, pv_name, system_name, equipment, description,
				 unit, pv_min, pv_max, priority, alarm_low, alarm_high, deadband, enabled,
				 state0_text, state1_text, alarm_state)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
			RETURNING pv_id`, args...).Scan(&r.ID)
	} else {
		var tag pgconn.CommandTag
		tag, err = s.pool.Exec(ctx, `
			UPDATE pv_metadata SET
				plc_id = $1, register_address = $2, pv_name = $3, system_name = $4,
				equipment = $5, description = $6, unit = $7, pv_min = $8, pv_max = $9,
				priority = $10, alarm_low = $11, alarm_high = $12, deadband = $13,
				enabled = $14, state0_text = $15, state1_text = $16, alarm_state = $17,
				updated_at = now()
			WHERE pv_id = $18`, append(args, r.ID)...)
		if err == nil && tag.RowsAffected() == 0 {
			err = pgx.ErrNoRows
		}
	}
	return r.ID, asConstraintError(err)
}

// HistoryCount returns how many history rows a PV has.
func (s *Store) HistoryCount(ctx context.Context, pvID int) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pv_history WHERE pv_id = $1`, pvID).Scan(&n)
	return n, err
}

// DeletePV removes a PV. If it has history, ErrInUse is returned unless
// withHistory is true — then history and PV are deleted in one transaction.
func (s *Store) DeletePV(ctx context.Context, id int, withHistory bool) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if withHistory {
			if _, err := tx.Exec(ctx, `DELETE FROM pv_history WHERE pv_id = $1`, id); err != nil {
				return err
			}
		}
		tag, err := tx.Exec(ctx, `DELETE FROM pv_metadata WHERE pv_id = $1`, id)
		if ce := asConstraintError(err); ce != err {
			return fmt.Errorf("%w: PV has history", ErrInUse)
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}
