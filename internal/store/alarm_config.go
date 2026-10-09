package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Errors of the alarm-configuration workflow.
var (
	ErrAlarmExists   = errors.New("an alarm with this condition already exists for this PV")
	ErrPendingExists = errors.New("this alarm already has a version waiting for approval")
	ErrNotPending    = errors.New("this version is not waiting for approval")
	ErrSelfApproval  = errors.New("a version cannot be approved by the person who submitted it")
	ErrHasAlarms     = errors.New("PV has alarm definitions")
)

type AlarmPriority struct {
	ID   int
	Name string
	Rank int
}

type AlarmClass struct {
	ID   int
	Name string
}

// AlarmPV is one PV in the "new alarm" drop-down.
type AlarmPV struct {
	ID      int
	PLCName string
	Name    string
	Unit    string
	Digital bool
}

// AlarmRow is one line of the alarm configuration list. The settings shown
// come from the approved version, or the newest one while none is approved.
type AlarmRow struct {
	AlarmID        int
	PVID           int
	Condition      string
	CurrentVersion *int // approved version in force
	PendingVersion *int // version waiting for approval
	ShownVersion   int

	PLCName    string
	PVName     string
	Unit       string
	Equipment  string
	System     string
	Digital    bool
	State0Text string
	State1Text string

	Tag          string
	Setpoint     *float64
	TriggerState *int
	Deadband     float64
	Priority     string
	PriorityRank int
	Class        string
	Enabled      bool
	Message      string
}

// Trigger is the human-readable trigger, e.g. "≥ 80 PSI" or "when TRIP".
func (a AlarmRow) Trigger() string {
	return triggerText(a.Condition, a.Setpoint, a.TriggerState, a.Unit, a.State0Text, a.State1Text)
}

func triggerText(condition string, setpoint *float64, triggerState *int, unit, state0, state1 string) string {
	switch {
	case condition == "DISCRETE" && triggerState != nil:
		if *triggerState == 1 {
			return "when " + state1
		}
		return "when " + state0
	case setpoint == nil:
		return "—"
	case condition == "HI" || condition == "HIHI":
		return fmt.Sprintf("≥ %g %s", *setpoint, unit)
	default:
		return fmt.Sprintf("≤ %g %s", *setpoint, unit)
	}
}

// AlarmVersion is one stored version of an alarm definition.
type AlarmVersion struct {
	AlarmID int
	Version int

	Tag                string
	Location           string
	Setpoint           *float64
	TriggerState       *int
	Deadband           float64
	OnDelayS           int
	OffDelayS          int
	PriorityID         int
	PriorityName       string
	ClassID            int
	ClassName          string
	Message            string
	Description        string
	Cause              string
	Consequence        string
	OperatorAction     string
	Owner              string
	ResponseTimeS      *int
	ShelvingAllowed    bool
	SuppressionAllowed bool
	Enabled            bool

	Status        string // pending | approved | rejected | superseded
	ChangeReason  string
	CreatedBy     string // username, "" = system import
	CreatedByID   *int
	CreatedAt     time.Time
	ReviewedBy    string
	ReviewedAt    *time.Time
	ReviewComment string
}

// AlarmInput is what a user submits: a new alarm (AlarmID == 0) or a new
// version of an existing one.
type AlarmInput struct {
	AlarmID   int
	PVID      int    // new alarm only
	Condition string // new alarm only

	Tag                string
	Location           string
	Setpoint           *float64
	TriggerState       *int
	Deadband           float64
	OnDelayS           int
	OffDelayS          int
	PriorityID         int
	ClassID            int
	Message            string
	Description        string
	Cause              string
	Consequence        string
	OperatorAction     string
	Owner              string
	ResponseTimeS      *int
	ShelvingAllowed    bool
	SuppressionAllowed bool
	Enabled            bool
	ChangeReason       string
}

// Actor is the authenticated user performing an action. It always comes from
// the server-side session, never from the request.
type Actor struct {
	ID       int
	Username string
	IP       string
}

// AuditEntry is one line of alarm_audit_log.
type AuditEntry struct {
	Actor      Actor // ID 0 = no logged-in user
	Action     string
	TargetType string
	TargetID   string
	Outcome    string // success | denied | rejected | failed
	Reason     string
	Before     []byte // JSON, optional
	After      []byte // JSON, optional
}

func (s *Store) ListAlarmPriorities(ctx context.Context) ([]AlarmPriority, error) {
	rows, err := s.pool.Query(ctx, `SELECT priority_id, name, rank FROM alarm_priorities ORDER BY rank DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmPriority, error) {
		var p AlarmPriority
		return p, r.Scan(&p.ID, &p.Name, &p.Rank)
	})
}

func (s *Store) ListAlarmClasses(ctx context.Context) ([]AlarmClass, error) {
	rows, err := s.pool.Query(ctx, `SELECT class_id, name FROM alarm_classes ORDER BY class_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmClass, error) {
		var c AlarmClass
		return c, r.Scan(&c.ID, &c.Name)
	})
}

// ListAlarmPVs returns every PV an alarm can be attached to.
func (s *Store) ListAlarmPVs(ctx context.Context) ([]AlarmPV, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT pv.pv_id, p.plc_name, pv.pv_name, pv.unit,
		       pv.register_address BETWEEN 10001 AND 19999
		FROM pv_metadata pv JOIN plc_metadata p ON p.plc_id = pv.plc_id
		ORDER BY p.plc_name, pv.register_address`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmPV, error) {
		var v AlarmPV
		return v, r.Scan(&v.ID, &v.PLCName, &v.Name, &v.Unit, &v.Digital)
	})
}

const alarmRowSelect = `
	SELECT c.alarm_id, c.pv_id, c.condition, c.current_version,
	       (SELECT p.version FROM alarm_config_versions p
	         WHERE p.alarm_id = c.alarm_id AND p.status = 'pending'),
	       v.version,
	       plc.plc_name, pv.pv_name, pv.unit, pv.equipment, pv.system_name,
	       pv.register_address BETWEEN 10001 AND 19999, pv.state0_text, pv.state1_text,
	       v.alarm_tag, v.setpoint, v.trigger_state, v.deadband,
	       pr.name, pr.rank, cl.name, v.enabled, v.message
	FROM alarm_config c
	JOIN pv_metadata pv  ON pv.pv_id  = c.pv_id
	JOIN plc_metadata plc ON plc.plc_id = pv.plc_id
	JOIN alarm_config_versions v ON v.alarm_id = c.alarm_id
	     AND v.version = COALESCE(c.current_version,
	                              (SELECT max(x.version) FROM alarm_config_versions x WHERE x.alarm_id = c.alarm_id))
	JOIN alarm_priorities pr ON pr.priority_id = v.priority_id
	JOIN alarm_classes    cl ON cl.class_id    = v.class_id`

func scanAlarmRow(r pgx.Row, a *AlarmRow) error {
	return r.Scan(&a.AlarmID, &a.PVID, &a.Condition, &a.CurrentVersion, &a.PendingVersion, &a.ShownVersion,
		&a.PLCName, &a.PVName, &a.Unit, &a.Equipment, &a.System,
		&a.Digital, &a.State0Text, &a.State1Text,
		&a.Tag, &a.Setpoint, &a.TriggerState, &a.Deadband,
		&a.Priority, &a.PriorityRank, &a.Class, &a.Enabled, &a.Message)
}

func (s *Store) ListAlarms(ctx context.Context) ([]AlarmRow, error) {
	rows, err := s.pool.Query(ctx, alarmRowSelect+`
		ORDER BY plc.plc_name, pv.register_address,
		         array_position(ARRAY['HIHI','HI','LO','LOLO','DISCRETE'], c.condition)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmRow, error) {
		var a AlarmRow
		return a, scanAlarmRow(r, &a)
	})
}

// GetAlarm returns one alarm. pgx.ErrNoRows if it does not exist.
func (s *Store) GetAlarm(ctx context.Context, alarmID int) (AlarmRow, error) {
	var a AlarmRow
	err := scanAlarmRow(s.pool.QueryRow(ctx, alarmRowSelect+` WHERE c.alarm_id = $1`, alarmID), &a)
	return a, err
}

// ListAlarmVersions returns every version of an alarm, newest first.
func (s *Store) ListAlarmVersions(ctx context.Context, alarmID int) ([]AlarmVersion, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT v.alarm_id, v.version, v.alarm_tag, v.location, v.setpoint, v.trigger_state,
		       v.deadband, v.on_delay_s, v.off_delay_s,
		       v.priority_id, pr.name, v.class_id, cl.name,
		       v.message, v.description, v.cause, v.consequence, v.operator_action, v.owner,
		       v.response_time_s, v.shelving_allowed, v.suppression_allowed, v.enabled,
		       v.status, v.change_reason, COALESCE(cu.username, ''), v.created_by, v.created_at,
		       COALESCE(ru.username, ''), v.reviewed_at, v.review_comment
		FROM alarm_config_versions v
		JOIN alarm_priorities pr ON pr.priority_id = v.priority_id
		JOIN alarm_classes    cl ON cl.class_id    = v.class_id
		LEFT JOIN users cu ON cu.user_id = v.created_by
		LEFT JOIN users ru ON ru.user_id = v.reviewed_by
		WHERE v.alarm_id = $1
		ORDER BY v.version DESC`, alarmID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AlarmVersion, error) {
		var v AlarmVersion
		err := r.Scan(&v.AlarmID, &v.Version, &v.Tag, &v.Location, &v.Setpoint, &v.TriggerState,
			&v.Deadband, &v.OnDelayS, &v.OffDelayS,
			&v.PriorityID, &v.PriorityName, &v.ClassID, &v.ClassName,
			&v.Message, &v.Description, &v.Cause, &v.Consequence, &v.OperatorAction, &v.Owner,
			&v.ResponseTimeS, &v.ShelvingAllowed, &v.SuppressionAllowed, &v.Enabled,
			&v.Status, &v.ChangeReason, &v.CreatedBy, &v.CreatedByID, &v.CreatedAt,
			&v.ReviewedBy, &v.ReviewedAt, &v.ReviewComment)
		return v, err
	})
}

// SiblingSetpoints returns the limits of the other analog alarms of a PV
// (pending version if there is one, otherwise the approved one), keyed by
// condition. Used to check HIHI > HI > LO > LOLO.
func (s *Store) SiblingSetpoints(ctx context.Context, pvID, exceptAlarmID int) (map[string]float64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.condition, v.setpoint
		FROM alarm_config c
		JOIN alarm_config_versions v ON v.alarm_id = c.alarm_id AND v.status IN ('pending', 'approved')
		WHERE c.pv_id = $1 AND c.alarm_id <> $2 AND v.setpoint IS NOT NULL
		ORDER BY v.status`, pvID, exceptAlarmID) // 'approved' sorts before 'pending', so a pending limit overwrites it below
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var cond string
		var sp float64
		if err := rows.Scan(&cond, &sp); err != nil {
			return nil, err
		}
		out[cond] = sp
	}
	return out, rows.Err()
}

// AlarmCondition returns the condition and PV of an alarm.
func (s *Store) AlarmCondition(ctx context.Context, alarmID int) (pvID int, condition string, err error) {
	err = s.pool.QueryRow(ctx, `SELECT pv_id, condition FROM alarm_config WHERE alarm_id = $1`, alarmID).Scan(&pvID, &condition)
	return
}

// SubmitAlarmVersion stores a new alarm (version 1) or a new version of an
// existing alarm. The version waits for approval ("pending"); the alarm in
// force does not change until it is approved. Everything — identity, version,
// approval history and audit entry — is written in one transaction.
func (s *Store) SubmitAlarmVersion(ctx context.Context, in AlarmInput, actor Actor) (alarmID, version int, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var before []byte
		action := "config.submit"
		if in.AlarmID == 0 {
			if e := tx.QueryRow(ctx, `
				INSERT INTO alarm_config (pv_id, condition) VALUES ($1, $2) RETURNING alarm_id`,
				in.PVID, in.Condition).Scan(&alarmID); e != nil {
				if isConstraint(e, "alarm_config_pv_id_condition_key") {
					return ErrAlarmExists
				}
				return e
			}
			version = 1
			action = "config.create"
		} else {
			alarmID = in.AlarmID
			if e := tx.QueryRow(ctx, `SELECT alarm_id FROM alarm_config WHERE alarm_id = $1 FOR UPDATE`, alarmID).Scan(&alarmID); e != nil {
				return e // pgx.ErrNoRows if the alarm does not exist
			}
			if e := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM alarm_config_versions WHERE alarm_id = $1`, alarmID).Scan(&version); e != nil {
				return e
			}
			// "Before" = the version in force (or, failing that, the newest one).
			_ = tx.QueryRow(ctx, `
				SELECT to_jsonb(v) FROM alarm_config_versions v
				WHERE v.alarm_id = $1 ORDER BY (v.status = 'approved') DESC, v.version DESC LIMIT 1`, alarmID).Scan(&before)
		}

		if _, e := tx.Exec(ctx, `
			INSERT INTO alarm_config_versions
				(alarm_id, version, alarm_tag, location, setpoint, trigger_state, deadband,
				 on_delay_s, off_delay_s, priority_id, class_id,
				 message, description, cause, consequence, operator_action, owner, response_time_s,
				 shelving_allowed, suppression_allowed, enabled, status, change_reason, created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,'pending',$22,$23)`,
			alarmID, version, in.Tag, in.Location, in.Setpoint, in.TriggerState, in.Deadband,
			in.OnDelayS, in.OffDelayS, in.PriorityID, in.ClassID,
			in.Message, in.Description, in.Cause, in.Consequence, in.OperatorAction, in.Owner, in.ResponseTimeS,
			in.ShelvingAllowed, in.SuppressionAllowed, in.Enabled, in.ChangeReason, actor.ID); e != nil {
			if isConstraint(e, "alarm_one_pending_idx") {
				return ErrPendingExists
			}
			return e
		}

		if _, e := tx.Exec(ctx, `
			INSERT INTO alarm_approvals (alarm_id, version, action, user_id, username, comment)
			VALUES ($1, $2, 'submitted', $3, $4, $5)`,
			alarmID, version, actor.ID, actor.Username, in.ChangeReason); e != nil {
			return e
		}

		var after []byte
		if e := tx.QueryRow(ctx, `SELECT to_jsonb(v) FROM alarm_config_versions v WHERE alarm_id = $1 AND version = $2`,
			alarmID, version).Scan(&after); e != nil {
			return e
		}
		return insertAudit(ctx, tx, AuditEntry{
			Actor: actor, Action: action, TargetType: "alarm_config", TargetID: fmt.Sprint(alarmID),
			Outcome: "success", Reason: in.ChangeReason, Before: before, After: after,
		})
	})
	if err != nil {
		return 0, 0, err
	}
	return alarmID, version, nil
}

// ReviewAlarmVersion approves or rejects the pending version. The approver
// must differ from the author unless allowSelf is set (the self-approval is
// then recorded as such). Approving makes the version the one in force and
// marks the previous one superseded.
func (s *Store) ReviewAlarmVersion(ctx context.Context, alarmID, version int, approve bool, comment string, actor Actor, allowSelf bool) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SELECT 1 FROM alarm_config WHERE alarm_id = $1 FOR UPDATE`, alarmID); e != nil {
			return e
		}
		var status string
		var createdBy *int
		if e := tx.QueryRow(ctx, `SELECT status, created_by FROM alarm_config_versions WHERE alarm_id = $1 AND version = $2`,
			alarmID, version).Scan(&status, &createdBy); e != nil {
			return e // pgx.ErrNoRows
		}
		if status != "pending" {
			return ErrNotPending
		}
		self := createdBy != nil && *createdBy == actor.ID
		if self && !allowSelf {
			return ErrSelfApproval
		}

		var before []byte
		_ = tx.QueryRow(ctx, `SELECT to_jsonb(v) FROM alarm_config_versions v WHERE v.alarm_id = $1 AND v.status = 'approved'`, alarmID).Scan(&before)

		newStatus, action := "rejected", "rejected"
		if approve {
			newStatus, action = "approved", "approved"
			if _, e := tx.Exec(ctx, `UPDATE alarm_config_versions SET status = 'superseded' WHERE alarm_id = $1 AND status = 'approved'`, alarmID); e != nil {
				return e
			}
		}
		if _, e := tx.Exec(ctx, `
			UPDATE alarm_config_versions
			SET status = $3, reviewed_by = $4, reviewed_at = now(), review_comment = $5
			WHERE alarm_id = $1 AND version = $2`, alarmID, version, newStatus, actor.ID, comment); e != nil {
			return e
		}
		if approve {
			if _, e := tx.Exec(ctx, `UPDATE alarm_config SET current_version = $2 WHERE alarm_id = $1`, alarmID, version); e != nil {
				return e
			}
		}
		if _, e := tx.Exec(ctx, `
			INSERT INTO alarm_approvals (alarm_id, version, action, user_id, username, comment, self_approved)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			alarmID, version, action, actor.ID, actor.Username, comment, self); e != nil {
			return e
		}
		var after []byte
		_ = tx.QueryRow(ctx, `SELECT to_jsonb(v) FROM alarm_config_versions v WHERE alarm_id = $1 AND version = $2`, alarmID, version).Scan(&after)
		return insertAudit(ctx, tx, AuditEntry{
			Actor: actor, Action: "config." + map[bool]string{true: "approve", false: "reject"}[approve],
			TargetType: "alarm_config", TargetID: fmt.Sprint(alarmID),
			Outcome: "success", Reason: comment, Before: before, After: after,
		})
	})
	if errors.Is(err, ErrSelfApproval) {
		// The refusal itself is auditable, outside the rolled-back transaction.
		_ = s.RecordAudit(ctx, AuditEntry{
			Actor: actor, Action: "config.approve", TargetType: "alarm_config", TargetID: fmt.Sprint(alarmID),
			Outcome: "rejected", Reason: "self-approval refused (version " + fmt.Sprint(version) + ")",
		})
	}
	return err
}

// RecordAudit appends one line to alarm_audit_log (for actions that are
// refused or fail, where there is no transaction to join).
func (s *Store) RecordAudit(ctx context.Context, e AuditEntry) error {
	return insertAudit(ctx, s.pool, e)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertAudit(ctx context.Context, db execer, e AuditEntry) error {
	var userID *int
	if e.Actor.ID != 0 {
		userID = &e.Actor.ID
	}
	_, err := db.Exec(ctx, `
		INSERT INTO alarm_audit_log
			(user_id, username, action, target_type, target_id, outcome, reason, before_data, after_data, client_ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		userID, e.Actor.Username, e.Action, e.TargetType, e.TargetID, e.Outcome, e.Reason,
		nilIfEmpty(e.Before), nilIfEmpty(e.After), e.Actor.IP)
	return err
}

func nilIfEmpty(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func isConstraint(err error, name string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.ConstraintName == name
}

// AlarmCountForPV returns how many alarm definitions reference a PV.
func (s *Store) AlarmCountForPV(ctx context.Context, pvID int) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_config WHERE pv_id = $1`, pvID).Scan(&n)
	return n, err
}
