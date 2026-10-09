// Package ams is the Alarm Management System. This file holds the alarm
// configuration rules: validation, permissions and the submit/approve
// workflow. It knows nothing about HTTP; the web layer calls it, and a
// later alarm engine uses the same definitions.
package ams

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// Alarm conditions. Condition, priority and class are independent attributes.
const (
	CondHIHI     = "HIHI"
	CondHI       = "HI"
	CondLO       = "LO"
	CondLOLO     = "LOLO"
	CondDiscrete = "DISCRETE"
)

// Permissions used here (seeded by migration 003).
const (
	PermView    = "alarm.view"
	PermConfig  = "alarm.config"
	PermApprove = "alarm.approve"
)

var (
	ErrForbidden = errors.New("not permitted")
	ErrNotFound  = errors.New("alarm not found")
)

// ValidationError carries one message per form field.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for f, m := range e.Fields {
		parts = append(parts, f+": "+m)
	}
	return "invalid alarm definition: " + strings.Join(parts, "; ")
}

// Field-length limits.
const (
	maxTag   = 60
	maxShort = 120
	maxText  = 1000
)

// Repo is the part of the store the configuration service needs.
type Repo interface {
	LatestValue(ctx context.Context, pvID int) (store.LatestRow, error)
	SiblingSetpoints(ctx context.Context, pvID, exceptAlarmID int) (map[string]float64, error)
	AlarmCondition(ctx context.Context, alarmID int) (pvID int, condition string, err error)
	ListAlarmPriorities(ctx context.Context) ([]store.AlarmPriority, error)
	ListAlarmClasses(ctx context.Context) ([]store.AlarmClass, error)
	SubmitAlarmVersion(ctx context.Context, in store.AlarmInput, actor store.Actor) (alarmID, version int, err error)
	ReviewAlarmVersion(ctx context.Context, alarmID, version int, approve bool, comment string, actor store.Actor, allowSelf bool) error
	RecordAudit(ctx context.Context, e store.AuditEntry) error
}

type ConfigService struct{ repo Repo }

func NewConfigService(repo Repo) *ConfigService { return &ConfigService{repo: repo} }

// Submit validates a definition and stores it as a pending version.
// in.AlarmID == 0 creates a new alarm (in.PVID and in.Condition are used);
// otherwise PV and condition come from the stored alarm, never from the request.
func (s *ConfigService) Submit(ctx context.Context, user *auth.User, ip string, in store.AlarmInput) (alarmID, version int, err error) {
	actor := actorOf(user, ip)
	if !user.Can(PermConfig) {
		s.denied(ctx, actor, "config.submit", in.AlarmID, PermConfig)
		return 0, 0, ErrForbidden
	}

	pvID, condition := in.PVID, strings.ToUpper(strings.TrimSpace(in.Condition))
	if in.AlarmID != 0 {
		var e error
		pvID, condition, e = s.repo.AlarmCondition(ctx, in.AlarmID)
		if e != nil {
			return 0, 0, ErrNotFound
		}
	}
	in.PVID, in.Condition = pvID, condition

	pv, e := s.repo.LatestValue(ctx, pvID)
	if e != nil {
		return 0, 0, &ValidationError{Fields: map[string]string{"pv_id": "choose an existing PV"}}
	}
	siblings, e := s.repo.SiblingSetpoints(ctx, pvID, in.AlarmID)
	if e != nil {
		return 0, 0, fmt.Errorf("load sibling alarms: %w", e)
	}
	prios, e := s.repo.ListAlarmPriorities(ctx)
	if e != nil {
		return 0, 0, fmt.Errorf("load priorities: %w", e)
	}
	classes, e := s.repo.ListAlarmClasses(ctx)
	if e != nil {
		return 0, 0, fmt.Errorf("load classes: %w", e)
	}

	in = normalize(in)
	if fields := Validate(in, pv.PV, siblings, prios, classes); len(fields) > 0 {
		return 0, 0, &ValidationError{Fields: fields}
	}
	return s.repo.SubmitAlarmVersion(ctx, in, actor)
}

// Review approves or rejects the pending version of an alarm.
func (s *ConfigService) Review(ctx context.Context, user *auth.User, ip string, alarmID, version int, approve bool, comment string) error {
	actor := actorOf(user, ip)
	action := "config.reject"
	if approve {
		action = "config.approve"
	}
	if !user.Can(PermApprove) {
		s.denied(ctx, actor, action, alarmID, PermApprove)
		return ErrForbidden
	}
	comment = strings.TrimSpace(comment)
	if !approve && comment == "" {
		return &ValidationError{Fields: map[string]string{"comment": "give a reason for rejecting"}}
	}
	if len(comment) > maxText {
		return &ValidationError{Fields: map[string]string{"comment": "too long"}}
	}
	// Only an administrator may approve their own change (a one-person site
	// needs this); the approval is recorded as self-approved.
	return s.repo.ReviewAlarmVersion(ctx, alarmID, version, approve, comment, actor, user.Role == "admin")
}

func (s *ConfigService) denied(ctx context.Context, actor store.Actor, action string, alarmID int, perm string) {
	target := ""
	if alarmID != 0 {
		target = fmt.Sprint(alarmID)
	}
	_ = s.repo.RecordAudit(ctx, store.AuditEntry{
		Actor: actor, Action: action, TargetType: "alarm_config", TargetID: target,
		Outcome: "denied", Reason: "missing permission " + perm,
	})
}

func actorOf(u *auth.User, ip string) store.Actor {
	if u == nil {
		return store.Actor{IP: ip}
	}
	return store.Actor{ID: u.ID, Username: u.Username, IP: ip}
}

func normalize(in store.AlarmInput) store.AlarmInput {
	in.Tag = strings.TrimSpace(in.Tag)
	in.Location = strings.TrimSpace(in.Location)
	in.Message = strings.TrimSpace(in.Message)
	in.Description = strings.TrimSpace(in.Description)
	in.Cause = strings.TrimSpace(in.Cause)
	in.Consequence = strings.TrimSpace(in.Consequence)
	in.OperatorAction = strings.TrimSpace(in.OperatorAction)
	in.Owner = strings.TrimSpace(in.Owner)
	in.ChangeReason = strings.TrimSpace(in.ChangeReason)
	return in
}

// analogOrder lists the analog conditions from the highest limit to the lowest.
var analogOrder = []string{CondHIHI, CondHI, CondLO, CondLOLO}

func isHigh(cond string) bool { return cond == CondHI || cond == CondHIHI }

// Validate checks one definition against the PV it belongs to and the other
// alarms of that PV. It returns a message per field name, empty when valid.
func Validate(in store.AlarmInput, pv store.PV, siblings map[string]float64, prios []store.AlarmPriority, classes []store.AlarmClass) map[string]string {
	f := map[string]string{}
	digital := pv.IsDigital()

	switch in.Condition {
	case CondHIHI, CondHI, CondLO, CondLOLO:
		if digital {
			f["condition"] = "a digital input can only have a DISCRETE alarm"
		}
	case CondDiscrete:
		if !digital {
			f["condition"] = "DISCRETE alarms are for digital inputs only"
		}
	default:
		f["condition"] = "choose HIHI, HI, LO, LOLO or DISCRETE"
	}

	if in.Tag == "" {
		f["alarm_tag"] = "required"
	} else if len(in.Tag) > maxTag {
		f["alarm_tag"] = fmt.Sprintf("at most %d characters", maxTag)
	}
	if in.Message == "" {
		f["message"] = "required"
	} else if len(in.Message) > maxShort {
		f["message"] = fmt.Sprintf("at most %d characters", maxShort)
	}
	if in.ChangeReason == "" {
		f["change_reason"] = "required — say why this alarm is being created or changed"
	}
	for name, v := range map[string]string{
		"location": in.Location, "owner": in.Owner,
	} {
		if len(v) > maxShort {
			f[name] = fmt.Sprintf("at most %d characters", maxShort)
		}
	}
	for name, v := range map[string]string{
		"description": in.Description, "cause": in.Cause, "consequence": in.Consequence,
		"operator_action": in.OperatorAction, "change_reason": in.ChangeReason,
	} {
		if len(v) > maxText {
			f[name] = fmt.Sprintf("at most %d characters", maxText)
		}
	}

	if !hasPriority(prios, in.PriorityID) {
		f["priority_id"] = "choose a priority"
	}
	if !hasClass(classes, in.ClassID) {
		f["class_id"] = "choose a class"
	}
	if in.ResponseTimeS != nil && *in.ResponseTimeS <= 0 {
		f["response_time_s"] = "must be greater than 0 (or leave empty)"
	}
	if in.OnDelayS < 0 || in.OnDelayS > 3600 {
		f["on_delay_s"] = "between 0 and 3600 seconds"
	}
	if in.OffDelayS < 0 || in.OffDelayS > 3600 {
		f["off_delay_s"] = "between 0 and 3600 seconds"
	}
	if math.IsNaN(in.Deadband) || math.IsInf(in.Deadband, 0) || in.Deadband < 0 {
		f["deadband"] = "must be 0 or more"
	}

	if in.Condition == CondDiscrete {
		if in.TriggerState == nil || (*in.TriggerState != 0 && *in.TriggerState != 1) {
			f["trigger_state"] = "choose the alarm state"
		}
		if in.Setpoint != nil {
			f["setpoint"] = "not used for a DISCRETE alarm"
		}
		if in.Deadband != 0 {
			f["deadband"] = "not used for a DISCRETE alarm (use the delays)"
		}
		return f
	}

	// Analog conditions from here on.
	if in.TriggerState != nil {
		f["trigger_state"] = "only for DISCRETE alarms"
	}
	sp := in.Setpoint
	if sp == nil || math.IsNaN(*sp) || math.IsInf(*sp, 0) {
		f["setpoint"] = "required"
		return f
	}
	if *sp < pv.Min || *sp > pv.Max {
		f["setpoint"] = fmt.Sprintf("outside the PV range %g … %g — it could never trigger", pv.Min, pv.Max)
	}
	if in.Deadband >= pv.Max-pv.Min {
		f["deadband"] = "must be smaller than the PV range"
	}

	// HIHI > HI > LO > LOLO, with the other alarms of the same PV.
	all := map[string]float64{}
	for k, v := range siblings {
		all[k] = v
	}
	all[in.Condition] = *sp
	for i, hi := range analogOrder {
		hv, ok := all[hi]
		if !ok {
			continue
		}
		for _, lo := range analogOrder[i+1:] {
			lv, ok := all[lo]
			if ok && hv <= lv && (hi == in.Condition || lo == in.Condition) {
				f["setpoint"] = fmt.Sprintf("%s (%g) must be above %s (%g)", hi, hv, lo, lv)
			}
		}
	}

	// The deadband must not reach the opposite side: a HI alarm that only
	// clears below the LO limit (or the reverse) would hide that condition.
	if _, bad := f["setpoint"]; !bad && f["deadband"] == "" {
		for _, other := range analogOrder {
			ov, ok := siblings[other]
			if !ok || isHigh(other) == isHigh(in.Condition) {
				continue
			}
			if isHigh(in.Condition) && *sp-in.Deadband <= ov {
				f["deadband"] = fmt.Sprintf("alarm would only clear at %g, at or below the %s limit (%g)", *sp-in.Deadband, other, ov)
			}
			if !isHigh(in.Condition) && *sp+in.Deadband >= ov {
				f["deadband"] = fmt.Sprintf("alarm would only clear at %g, at or above the %s limit (%g)", *sp+in.Deadband, other, ov)
			}
		}
	}
	return f
}

func hasPriority(list []store.AlarmPriority, id int) bool {
	for _, p := range list {
		if p.ID == id {
			return true
		}
	}
	return false
}

func hasClass(list []store.AlarmClass, id int) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}
