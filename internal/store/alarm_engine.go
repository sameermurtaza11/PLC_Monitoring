package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Alarm event types written by the engine (more are added by later steps).
const (
	EventActivated        = "ACTIVATED"
	EventReturnedToNormal = "RETURNED_TO_NORMAL"
)

// System-health event kinds.
const (
	HealthDataBad          = "DATA_BAD"
	HealthDataRestored     = "DATA_RESTORED"
	HealthEngineStarted    = "ENGINE_STARTED"
	HealthEngineStopped    = "ENGINE_STOPPED"
	HealthEngineGap        = "ENGINE_GAP"
	HealthEngineDBError    = "ENGINE_DB_ERROR"
	HealthEngineDBRecovery = "ENGINE_DB_RECOVERED"
)

// AlarmStateRow is one row of alarm_state. Exists is false until the alarm
// has had its first state change (a never-triggered alarm has no row).
type AlarmStateRow struct {
	Exists          bool
	ProcessActive   bool
	Unacknowledged  bool
	CycleNo         int
	OnPendingSince  *time.Time
	OffPendingSince *time.Time
	DataValid       bool // true until the engine finds the value untrustworthy
	ActiveVersion   *int
	ActivatedAt     *time.Time
	ActivationValue *float64
	ReturnedAt      *time.Time
	LastValue       *float64
	LastSampleTS    *time.Time
	DelayFiltered   int
}

// EngineAlarm is everything the engine needs to evaluate one alarm: the
// approved definition, the latest PV observation and the stored state.
type EngineAlarm struct {
	AlarmID   int
	PVID      int
	Condition string
	Version   int // approved version in force

	Setpoint     *float64
	TriggerState *int
	Deadband     float64
	OnDelayS     int
	OffDelayS    int
	Enabled      bool

	PLCName string
	PVName  string
	Tag     string
	// ScanIntervalMS decides when a value counts as stale.
	ScanIntervalMS int

	// Latest observation from pv_latest (nil = no row yet).
	Value    *float64
	Quality  *string
	SampleTS *time.Time

	State AlarmStateRow
}

// StateUpdate is a full replacement of one alarm_state row.
type StateUpdate struct {
	AlarmID int
	State   AlarmStateRow
}

// NewAlarmEvent is one transition to append to alarm_events.
type NewAlarmEvent struct {
	AlarmID int
	Version int
	CycleNo int
	Type    string
	EventTS time.Time
	Value   *float64
	Limit   *float64
	Note    string
}

// HealthEvent is one row for system_health_events.
type HealthEvent struct {
	TS        time.Time
	Kind      string
	Severity  string // info | warning | critical
	Component string
	PVID      *int
	AlarmID   *int
	Detail    string
}

// engineAlarmSelect reads the approved definition, the latest value and the
// stored state of every alarm in one query.
const engineAlarmSelect = `
	SELECT c.alarm_id, c.pv_id, c.condition, v.version,
	       v.setpoint, v.trigger_state, v.deadband, v.on_delay_s, v.off_delay_s, v.enabled,
	       plc.plc_name, pv.pv_name, v.alarm_tag, plc.scan_interval_ms,
	       l.pv_value, l.quality, l.ts,
	       s.alarm_id IS NOT NULL,
	       COALESCE(s.process_active, FALSE), COALESCE(s.unacknowledged, FALSE), COALESCE(s.cycle_no, 0),
	       s.on_pending_since, s.off_pending_since, COALESCE(s.data_valid, TRUE),
	       s.active_version, s.activated_at, s.activation_value, s.returned_at,
	       s.last_value, s.last_sample_ts, COALESCE(s.delay_filtered_count, 0)
	FROM alarm_config c
	JOIN alarm_config_versions v ON v.alarm_id = c.alarm_id AND v.version = c.current_version AND v.status = 'approved'
	JOIN pv_metadata pv   ON pv.pv_id = c.pv_id
	JOIN plc_metadata plc ON plc.plc_id = pv.plc_id
	LEFT JOIN pv_latest l ON l.pv_id = c.pv_id
	LEFT JOIN alarm_state s ON s.alarm_id = c.alarm_id
	ORDER BY c.alarm_id`

// LoadEngineInputs returns every alarm that has an approved version.
func (s *Store) LoadEngineInputs(ctx context.Context) ([]EngineAlarm, error) {
	rows, err := s.pool.Query(ctx, engineAlarmSelect)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EngineAlarm, error) {
		var a EngineAlarm
		st := &a.State
		err := r.Scan(&a.AlarmID, &a.PVID, &a.Condition, &a.Version,
			&a.Setpoint, &a.TriggerState, &a.Deadband, &a.OnDelayS, &a.OffDelayS, &a.Enabled,
			&a.PLCName, &a.PVName, &a.Tag, &a.ScanIntervalMS,
			&a.Value, &a.Quality, &a.SampleTS,
			&st.Exists, &st.ProcessActive, &st.Unacknowledged, &st.CycleNo,
			&st.OnPendingSince, &st.OffPendingSince, &st.DataValid,
			&st.ActiveVersion, &st.ActivatedAt, &st.ActivationValue, &st.ReturnedAt,
			&st.LastValue, &st.LastSampleTS, &st.DelayFiltered)
		return a, err
	})
}

// ApplyEngineResults stores one evaluation round in a single transaction:
// new alarm states, the transitions (events) and the health events. A
// transition is therefore never visible without its state change or the
// reverse. Events are idempotent: an activation or return that already
// exists for the same alarm and cycle is silently skipped.
func (s *Store) ApplyEngineResults(ctx context.Context, states []StateUpdate, events []NewAlarmEvent, health []HealthEvent) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, e := range events {
			if _, err := tx.Exec(ctx, `
				INSERT INTO alarm_events
					(alarm_id, config_version, cycle_no, event_type, event_ts, process_value, limit_value, note)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (alarm_id, cycle_no, event_type)
				WHERE event_type IN ('ACTIVATED', 'RETURNED_TO_NORMAL') DO NOTHING`,
				e.AlarmID, e.Version, e.CycleNo, e.Type, e.EventTS, e.Value, e.Limit, e.Note); err != nil {
				return fmt.Errorf("insert event: %w", err)
			}
		}
		for _, u := range states {
			st := u.State
			if _, err := tx.Exec(ctx, `
				INSERT INTO alarm_state
					(alarm_id, process_active, unacknowledged, cycle_no, on_pending_since, off_pending_since,
					 data_valid, active_version, activated_at, activation_value, returned_at,
					 last_value, last_sample_ts, delay_filtered_count, updated_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14, now())
				ON CONFLICT (alarm_id) DO UPDATE SET
					process_active = EXCLUDED.process_active,
					-- The engine owns the acknowledgement flag only when a NEW activation
					-- starts. Otherwise the stored value wins, so a write based on an older
					-- read can never undo an operator's acknowledgement made in between.
					unacknowledged = CASE WHEN EXCLUDED.cycle_no > alarm_state.cycle_no
					                      THEN EXCLUDED.unacknowledged ELSE alarm_state.unacknowledged END,
					acknowledged_by = CASE WHEN EXCLUDED.cycle_no > alarm_state.cycle_no
					                       THEN NULL ELSE alarm_state.acknowledged_by END,
					acknowledged_at = CASE WHEN EXCLUDED.cycle_no > alarm_state.cycle_no
					                       THEN NULL ELSE alarm_state.acknowledged_at END,
					cycle_no = EXCLUDED.cycle_no, on_pending_since = EXCLUDED.on_pending_since,
					off_pending_since = EXCLUDED.off_pending_since, data_valid = EXCLUDED.data_valid,
					active_version = EXCLUDED.active_version, activated_at = EXCLUDED.activated_at,
					activation_value = EXCLUDED.activation_value, returned_at = EXCLUDED.returned_at,
					last_value = EXCLUDED.last_value, last_sample_ts = EXCLUDED.last_sample_ts,
					delay_filtered_count = EXCLUDED.delay_filtered_count, updated_at = now()`,
				u.AlarmID, st.ProcessActive, st.Unacknowledged, st.CycleNo, st.OnPendingSince, st.OffPendingSince,
				st.DataValid, st.ActiveVersion, st.ActivatedAt, st.ActivationValue, st.ReturnedAt,
				st.LastValue, st.LastSampleTS, st.DelayFiltered); err != nil {
				return fmt.Errorf("upsert alarm_state: %w", err)
			}
		}
		for _, h := range health {
			if err := insertHealth(ctx, tx, h); err != nil {
				return err
			}
		}
		return nil
	})
}

func insertHealth(ctx context.Context, db execer, h HealthEvent) error {
	ts := h.TS
	if ts.IsZero() {
		ts = time.Now()
	}
	_, err := db.Exec(ctx, `
		INSERT INTO system_health_events (ts, kind, severity, component, pv_id, alarm_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		ts, h.Kind, h.Severity, h.Component, h.PVID, h.AlarmID, h.Detail)
	if err != nil {
		return fmt.Errorf("insert health event: %w", err)
	}
	return nil
}

// RecordHealth appends one system-health event.
func (s *Store) RecordHealth(ctx context.Context, h HealthEvent) error {
	return insertHealth(ctx, s.pool, h)
}

// engineLockKey identifies the alarm engine among PostgreSQL advisory locks.
const engineLockKey = 7_265_001

// TryEngineLock tries to become the one alarm engine of this database. Two
// application instances (for example a second copy started by mistake) must
// not both evaluate alarms. The lock is held on a dedicated connection and
// freed by release, or automatically if the process dies.
func (s *Store) TryEngineLock(ctx context.Context) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, engineLockKey).Scan(&got); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		c, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := conn.Exec(c, `SELECT pg_advisory_unlock($1)`, engineLockKey); err != nil {
			conn.Conn().Close(c) // closing the session frees the lock anyway
		}
		conn.Release()
	}, true, nil
}

// ErrEngineSuperseded means another engine instance has taken over.
var ErrEngineSuperseded = errors.New("another alarm engine instance has taken over")

// EngineHeartbeat reports that this engine instance is alive.
//   - first == true: returns when the previous instance last reported (zero
//     time if there was none) and records this instance as the running one.
//   - first == false: refreshes the heartbeat. If another instance has
//     registered in the meantime (for example the advisory lock was lost),
//     ErrEngineSuperseded is returned and this instance must stop.
func (s *Store) EngineHeartbeat(ctx context.Context, instance string, first bool) (previous time.Time, err error) {
	if first {
		var prev *time.Time
		err = s.pool.QueryRow(ctx, `SELECT (SELECT last_heartbeat FROM ams_engine_status WHERE id = 1)`).Scan(&prev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, err
		}
		if prev != nil {
			previous = *prev
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO ams_engine_status (id, instance, started_at, last_heartbeat)
			VALUES (1, $1, now(), now())
			ON CONFLICT (id) DO UPDATE SET instance = $1, started_at = now(), last_heartbeat = now()`, instance)
		return previous, err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE ams_engine_status SET last_heartbeat = now() WHERE id = 1 AND instance = $1`, instance)
	if err == nil && tag.RowsAffected() == 0 {
		err = ErrEngineSuperseded
	}
	return time.Time{}, err
}
