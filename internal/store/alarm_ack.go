package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Acknowledgement results.
const (
	AckAcknowledged        = "acknowledged"
	AckAlreadyAcknowledged = "already_acknowledged"
)

var (
	// ErrNothingToAck: the alarm has never been activated, so there is nothing to acknowledge.
	ErrNothingToAck = errors.New("this alarm has not been activated")
	// ErrAckStale: the alarm moved on (a newer activation) since the operator saw it.
	ErrAckStale = errors.New("the alarm changed since it was displayed; check the current state")
)

// AckResult says what an acknowledge request did.
type AckResult struct {
	Result  string // AckAcknowledged | AckAlreadyAcknowledged
	CycleNo int
	By      string
	At      time.Time
}

// AcknowledgeAlarm acknowledges one activation (alarm + cycle) of an alarm.
//
// It changes only the acknowledgement of the alarm. The process condition
// is untouched: an acknowledged alarm that is still abnormal stays active.
//
// Concurrency: the state row is locked for the duration, the request names the
// activation (cycleNo) the operator saw, and the database allows one
// ACKNOWLEDGED event per activation. So two operators clicking together
// produce one acknowledgement and one "already acknowledged" answer, and a
// click on an old activation cannot acknowledge a newer one.
func (s *Store) AcknowledgeAlarm(ctx context.Context, alarmID, cycleNo int, actor Actor) (AckResult, error) {
	var res AckResult
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var (
			cycle        int
			unack        bool
			activeVer    *int
			lastValue    *float64
			ackBy        *string
			ackAt        *time.Time
			before       []byte
			currentVer   *int
			limitDefault *float64
		)
		err := tx.QueryRow(ctx, `
			SELECT s.cycle_no, s.unacknowledged, s.active_version, s.last_value,
			       u.username, s.acknowledged_at, to_jsonb(s), c.current_version
			FROM alarm_state s
			JOIN alarm_config c ON c.alarm_id = s.alarm_id
			LEFT JOIN users u ON u.user_id = s.acknowledged_by
			WHERE s.alarm_id = $1
			FOR UPDATE OF s`, alarmID).
			Scan(&cycle, &unack, &activeVer, &lastValue, &ackBy, &ackAt, &before, &currentVer)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNothingToAck
		}
		if err != nil {
			return err
		}
		if cycle == 0 {
			return ErrNothingToAck
		}
		if cycle != cycleNo {
			return ErrAckStale
		}
		if !unack {
			res = AckResult{Result: AckAlreadyAcknowledged, CycleNo: cycle}
			if ackBy != nil {
				res.By = *ackBy
			}
			if ackAt != nil {
				res.At = *ackAt
			}
			return nil
		}

		version := currentVer
		if activeVer != nil {
			version = activeVer
		}
		if version == nil {
			return fmt.Errorf("alarm %d has no version to record the acknowledgement against", alarmID)
		}
		_ = tx.QueryRow(ctx, `SELECT COALESCE(setpoint, trigger_state) FROM alarm_config_versions WHERE alarm_id = $1 AND version = $2`,
			alarmID, *version).Scan(&limitDefault)

		var at time.Time
		if err := tx.QueryRow(ctx, `
			UPDATE alarm_state
			SET unacknowledged = FALSE, acknowledged_by = $2, acknowledged_at = now(), updated_at = now()
			WHERE alarm_id = $1
			RETURNING acknowledged_at`, alarmID, actor.ID).Scan(&at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO alarm_events
				(alarm_id, config_version, cycle_no, event_type, event_ts, process_value, limit_value, source)
			VALUES ($1, $2, $3, 'ACKNOWLEDGED', $4, $5, $6, $7)`,
			alarmID, *version, cycle, at, lastValue, limitDefault, "user:"+actor.Username); err != nil {
			return fmt.Errorf("insert acknowledgement event: %w", err)
		}

		var after []byte
		_ = tx.QueryRow(ctx, `SELECT to_jsonb(s) FROM alarm_state s WHERE alarm_id = $1`, alarmID).Scan(&after)
		if err := insertAudit(ctx, tx, AuditEntry{
			Actor: actor, Action: "alarm.acknowledge", TargetType: "alarm", TargetID: fmt.Sprint(alarmID),
			Outcome: "success", Reason: fmt.Sprintf("cycle %d", cycle), Before: before, After: after,
		}); err != nil {
			return err
		}
		res = AckResult{Result: AckAcknowledged, CycleNo: cycle, By: actor.Username, At: at}
		return nil
	})

	if errors.Is(err, ErrAckStale) {
		// A refused acknowledgement is worth keeping; it is written outside the rolled-back transaction.
		_ = s.RecordAudit(ctx, AuditEntry{
			Actor: actor, Action: "alarm.acknowledge", TargetType: "alarm", TargetID: fmt.Sprint(alarmID),
			Outcome: "rejected", Reason: fmt.Sprintf("stale: request was for activation %d", cycleNo),
		})
	}
	return res, err
}

// AlarmCurrent is one alarm with its definition (the approved version) and
// its current state, for the operator views.
type AlarmCurrent struct {
	AlarmID      int    `json:"alarm_id"`
	PVID         int    `json:"pv_id"`
	Tag          string `json:"tag"`
	Message      string `json:"message"`
	Condition    string `json:"condition"`
	Trigger      string `json:"trigger"`
	Priority     string `json:"priority"`
	PriorityRank int    `json:"priority_rank"`
	Class        string `json:"class"`
	Enabled      bool   `json:"enabled"`

	PLCName   string `json:"plc"`
	PVName    string `json:"pv"`
	Unit      string `json:"unit"`
	Equipment string `json:"equipment"`
	System    string `json:"system"`

	ProcessActive   bool       `json:"process_active"`
	Unacknowledged  bool       `json:"unacknowledged"`
	CycleNo         int        `json:"cycle_no"`
	DataValid       bool       `json:"data_valid"`
	ActivatedAt     *time.Time `json:"activated_at"`
	ActivationValue *float64   `json:"activation_value"`
	ReturnedAt      *time.Time `json:"returned_at"`
	LastValue       *float64   `json:"last_value"`
	AcknowledgedAt  *time.Time `json:"acknowledged_at"`
	AcknowledgedBy  *string    `json:"acknowledged_by"`

	Phase string `json:"phase"` // see AlarmPhase
}

// Alarm phases: how an operator sees the combination of process condition and acknowledgement.
const (
	PhaseActiveUnack = "active_unack" // in alarm, not acknowledged
	PhaseActiveAck   = "active_ack"   // in alarm, acknowledged — stays on the active list
	PhaseRTNUnack    = "rtn_unack"    // returned to normal before it was acknowledged
	PhaseNormal      = "normal"       // normal (and nothing left to acknowledge)
)

// AlarmPhase combines the independent state dimensions for display.
func AlarmPhase(processActive, unacknowledged bool, cycleNo int) string {
	switch {
	case processActive && unacknowledged:
		return PhaseActiveUnack
	case processActive:
		return PhaseActiveAck
	case unacknowledged && cycleNo > 0:
		return PhaseRTNUnack
	default:
		return PhaseNormal
	}
}

// ListAlarmCurrent returns every alarm that has an approved version, with its state.
func (s *Store) ListAlarmCurrent(ctx context.Context) ([]AlarmCurrent, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.alarm_id, c.pv_id, v.alarm_tag, v.message, c.condition,
		       v.setpoint, v.trigger_state, pr.name, pr.rank, cl.name, v.enabled,
		       plc.plc_name, pv.pv_name, pv.unit, pv.equipment, pv.system_name,
		       pv.state0_text, pv.state1_text,
		       COALESCE(s.process_active, FALSE), COALESCE(s.unacknowledged, FALSE), COALESCE(s.cycle_no, 0),
		       COALESCE(s.data_valid, TRUE), s.activated_at, s.activation_value, s.returned_at,
		       s.last_value, s.acknowledged_at, au.username
		FROM alarm_config c
		JOIN alarm_config_versions v ON v.alarm_id = c.alarm_id AND v.version = c.current_version AND v.status = 'approved'
		JOIN pv_metadata pv   ON pv.pv_id = c.pv_id
		JOIN plc_metadata plc ON plc.plc_id = pv.plc_id
		JOIN alarm_priorities pr ON pr.priority_id = v.priority_id
		JOIN alarm_classes    cl ON cl.class_id    = v.class_id
		LEFT JOIN alarm_state s  ON s.alarm_id = c.alarm_id
		LEFT JOIN users au ON au.user_id = s.acknowledged_by
		ORDER BY pr.rank DESC, s.activated_at DESC NULLS LAST, v.alarm_tag`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmCurrent, error) {
		var a AlarmCurrent
		var setpoint *float64
		var trigger *int
		var s0, s1 string
		err := r.Scan(&a.AlarmID, &a.PVID, &a.Tag, &a.Message, &a.Condition,
			&setpoint, &trigger, &a.Priority, &a.PriorityRank, &a.Class, &a.Enabled,
			&a.PLCName, &a.PVName, &a.Unit, &a.Equipment, &a.System,
			&s0, &s1,
			&a.ProcessActive, &a.Unacknowledged, &a.CycleNo,
			&a.DataValid, &a.ActivatedAt, &a.ActivationValue, &a.ReturnedAt,
			&a.LastValue, &a.AcknowledgedAt, &a.AcknowledgedBy)
		a.Trigger = triggerText(a.Condition, setpoint, trigger, a.Unit, s0, s1)
		a.Phase = AlarmPhase(a.ProcessActive, a.Unacknowledged, a.CycleNo)
		return a, err
	})
}
