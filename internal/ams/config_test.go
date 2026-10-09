package ams

import (
	"context"
	"errors"
	"strings"
	"testing"

	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

func f64(v float64) *float64 { return &v }
func i(v int) *int           { return &v }

var (
	prios   = []store.AlarmPriority{{ID: 1, Name: "Minor", Rank: 1}, {ID: 2, Name: "Major", Rank: 2}, {ID: 3, Name: "Critical", Rank: 3}}
	classes = []store.AlarmClass{{ID: 1, Name: "Process"}}
	// 0…10 bar analog PV and a digital PV
	analog  = store.PV{ID: 1, RegisterAddress: 30001, Min: 0, Max: 10, Unit: "bar"}
	digital = store.PV{ID: 2, RegisterAddress: 10001}
)

func valid(cond string) store.AlarmInput {
	in := store.AlarmInput{
		Condition: cond, Tag: "PT-101", Message: "Pressure high", ChangeReason: "initial",
		PriorityID: 2, ClassID: 1, Setpoint: f64(8), Deadband: 0.2, Enabled: true,
	}
	if cond == CondDiscrete {
		in.Setpoint, in.Deadband, in.TriggerState = nil, 0, i(1)
	}
	return in
}

func TestValidateAcceptsGoodDefinitions(t *testing.T) {
	if f := Validate(valid(CondHI), analog, nil, prios, classes); len(f) != 0 {
		t.Errorf("analog HI: %v", f)
	}
	if f := Validate(valid(CondDiscrete), digital, nil, prios, classes); len(f) != 0 {
		t.Errorf("discrete: %v", f)
	}
}

func TestValidateConditionMustMatchPVType(t *testing.T) {
	if f := Validate(valid(CondHI), digital, nil, prios, classes); f["condition"] == "" {
		t.Error("HI on a digital input must be refused")
	}
	if f := Validate(valid(CondDiscrete), analog, nil, prios, classes); f["condition"] == "" {
		t.Error("DISCRETE on an analog PV must be refused")
	}
	bad := valid(CondHI)
	bad.Condition = "HIGH"
	if f := Validate(bad, analog, nil, prios, classes); f["condition"] == "" {
		t.Error("unknown condition must be refused")
	}
}

func TestValidateSetpointInsidePVRange(t *testing.T) {
	in := valid(CondHI)
	in.Setpoint = f64(11) // PV can never exceed 10
	if f := Validate(in, analog, nil, prios, classes); f["setpoint"] == "" {
		t.Error("a limit outside the PV range can never trigger and must be refused")
	}
	in.Setpoint = nil
	if f := Validate(in, analog, nil, prios, classes); f["setpoint"] == "" {
		t.Error("analog alarm without a limit must be refused")
	}
}

func TestValidateLimitOrdering(t *testing.T) {
	// HI is 8; HIHI at 7 is below it.
	in := valid(CondHIHI)
	in.Setpoint = f64(7)
	if f := Validate(in, analog, map[string]float64{CondHI: 8}, prios, classes); f["setpoint"] == "" {
		t.Error("HIHI below HI must be refused")
	}
	in.Setpoint = f64(9)
	if f := Validate(in, analog, map[string]float64{CondHI: 8}, prios, classes); len(f) != 0 {
		t.Errorf("HIHI above HI should pass: %v", f)
	}
	// LO must stay below HI
	lo := valid(CondLO)
	lo.Setpoint, lo.Deadband = f64(8), 0
	if f := Validate(lo, analog, map[string]float64{CondHI: 8}, prios, classes); f["setpoint"] == "" {
		t.Error("LO equal to HI must be refused")
	}
}

func TestValidateDeadbandCannotHideTheOppositeAlarm(t *testing.T) {
	// HI at 8 with deadband 6 would only clear at 2, at/below the LO limit 3.
	in := valid(CondHI)
	in.Deadband = 6
	if f := Validate(in, analog, map[string]float64{CondLO: 3}, prios, classes); f["deadband"] == "" {
		t.Error("deadband reaching the LO limit must be refused")
	}
	in.Deadband = 0.2
	if f := Validate(in, analog, map[string]float64{CondLO: 3}, prios, classes); len(f) != 0 {
		t.Errorf("small deadband should pass: %v", f)
	}
	in.Deadband = -1
	if f := Validate(in, analog, nil, prios, classes); f["deadband"] == "" {
		t.Error("negative deadband must be refused")
	}
}

func TestValidateDelaysRequiredFieldsAndReferences(t *testing.T) {
	in := valid(CondHI)
	in.OnDelayS, in.OffDelayS = -1, 4000
	f := Validate(in, analog, nil, prios, classes)
	if f["on_delay_s"] == "" || f["off_delay_s"] == "" {
		t.Errorf("delays outside 0…3600 must be refused: %v", f)
	}

	in = valid(CondHI)
	in.Tag, in.Message, in.ChangeReason, in.PriorityID, in.ClassID = "", "", "", 99, 99
	f = Validate(in, analog, nil, prios, classes)
	for _, k := range []string{"alarm_tag", "message", "change_reason", "priority_id", "class_id"} {
		if f[k] == "" {
			t.Errorf("missing %s should be reported", k)
		}
	}

	in = valid(CondHI)
	in.Message = strings.Repeat("x", 500)
	if f := Validate(in, analog, nil, prios, classes); f["message"] == "" {
		t.Error("over-long message must be refused")
	}
}

func TestValidateDiscreteRules(t *testing.T) {
	in := valid(CondDiscrete)
	in.TriggerState = nil
	if f := Validate(in, digital, nil, prios, classes); f["trigger_state"] == "" {
		t.Error("DISCRETE needs an alarm state")
	}
	in = valid(CondDiscrete)
	in.Setpoint = f64(1)
	if f := Validate(in, digital, nil, prios, classes); f["setpoint"] == "" {
		t.Error("DISCRETE must not carry an analog limit")
	}
}

// --- service: permissions and workflow ---------------------------------

type fakeRepo struct {
	pv          store.PV
	submitted   []store.AlarmInput
	reviewed    int
	allowSelf   bool
	audits      []store.AuditEntry
	submitCalls int
}

func (f *fakeRepo) LatestValue(context.Context, int) (store.LatestRow, error) {
	return store.LatestRow{PV: f.pv}, nil
}
func (f *fakeRepo) SiblingSetpoints(context.Context, int, int) (map[string]float64, error) {
	return nil, nil
}
func (f *fakeRepo) AlarmCondition(context.Context, int) (int, string, error) {
	return f.pv.ID, CondHI, nil
}
func (f *fakeRepo) ListAlarmPriorities(context.Context) ([]store.AlarmPriority, error) {
	return prios, nil
}
func (f *fakeRepo) ListAlarmClasses(context.Context) ([]store.AlarmClass, error) { return classes, nil }
func (f *fakeRepo) SubmitAlarmVersion(_ context.Context, in store.AlarmInput, _ store.Actor) (int, int, error) {
	f.submitCalls++
	f.submitted = append(f.submitted, in)
	return 7, 1, nil
}
func (f *fakeRepo) ReviewAlarmVersion(_ context.Context, _, _ int, _ bool, _ string, _ store.Actor, allowSelf bool) error {
	f.reviewed++
	f.allowSelf = allowSelf
	return nil
}
func (f *fakeRepo) RecordAudit(_ context.Context, e store.AuditEntry) error {
	f.audits = append(f.audits, e)
	return nil
}

// userWith builds an authenticated user holding the given permissions.
func userWith(role string, perms ...string) *auth.User {
	return auth.NewTestUser(1, "tester", role, perms...)
}

func TestSubmitRequiresConfigPermissionAndAuditsRefusal(t *testing.T) {
	repo := &fakeRepo{pv: analog}
	svc := NewConfigService(repo)

	_, _, err := svc.Submit(context.Background(), userWith("operator", PermView, "alarm.ack"), "1.2.3.4", valid(CondHI))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v, want ErrForbidden", err)
	}
	if repo.submitCalls != 0 {
		t.Error("nothing may be stored for an unauthorised user")
	}
	if len(repo.audits) != 1 || repo.audits[0].Outcome != "denied" || repo.audits[0].Actor.Username != "tester" {
		t.Errorf("refusal must be audited with the actor: %+v", repo.audits)
	}

	if _, _, err := svc.Submit(context.Background(), nil, "", valid(CondHI)); !errors.Is(err, ErrForbidden) {
		t.Errorf("anonymous submit: got %v, want ErrForbidden", err)
	}
}

func TestSubmitStoresNormalisedValidInput(t *testing.T) {
	repo := &fakeRepo{pv: analog}
	svc := NewConfigService(repo)
	in := valid(CondHI)
	in.PVID, in.Tag, in.Condition = 1, "  PT-101  ", " hi "
	id, ver, err := svc.Submit(context.Background(), userWith("engineer", PermConfig), "", in)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if id != 7 || ver != 1 || len(repo.submitted) != 1 {
		t.Fatalf("unexpected result %d/%d/%d", id, ver, len(repo.submitted))
	}
	if got := repo.submitted[0]; got.Tag != "PT-101" || got.Condition != "HI" {
		t.Errorf("input not normalised: %+v", got)
	}
}

func TestSubmitIgnoresClientSuppliedPVAndConditionForExistingAlarm(t *testing.T) {
	repo := &fakeRepo{pv: analog}
	svc := NewConfigService(repo)
	in := valid(CondHI)
	in.AlarmID, in.PVID, in.Condition = 5, 999, CondLOLO // client lies about both
	if _, _, err := svc.Submit(context.Background(), userWith("engineer", PermConfig), "", in); err != nil {
		t.Fatalf("submit: %v", err)
	}
	got := repo.submitted[0]
	if got.PVID != analog.ID || got.Condition != CondHI {
		t.Errorf("PV/condition must come from the stored alarm: %+v", got)
	}
}

func TestSubmitInvalidInputIsNotStored(t *testing.T) {
	repo := &fakeRepo{pv: analog}
	svc := NewConfigService(repo)
	in := valid(CondHI)
	in.Setpoint = f64(50)
	_, _, err := svc.Submit(context.Background(), userWith("engineer", PermConfig), "", in)
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Fields["setpoint"] == "" {
		t.Fatalf("got %v, want a setpoint validation error", err)
	}
	if repo.submitCalls != 0 {
		t.Error("invalid input must not reach the database")
	}
}

func TestReviewNeedsApprovePermission(t *testing.T) {
	repo := &fakeRepo{pv: analog}
	svc := NewConfigService(repo)
	err := svc.Review(context.Background(), userWith("engineer", PermConfig), "", 1, 2, true, "")
	if !errors.Is(err, ErrForbidden) || repo.reviewed != 0 {
		t.Fatalf("engineer must not approve: err=%v reviewed=%d", err, repo.reviewed)
	}
	if len(repo.audits) != 1 || repo.audits[0].Outcome != "denied" {
		t.Errorf("refusal must be audited: %+v", repo.audits)
	}
}

func TestReviewRejectNeedsReasonAndOnlyAdminMaySelfApprove(t *testing.T) {
	repo := &fakeRepo{pv: analog}
	svc := NewConfigService(repo)

	err := svc.Review(context.Background(), userWith("supervisor", PermApprove), "", 1, 2, false, "  ")
	var ve *ValidationError
	if !errors.As(err, &ve) || repo.reviewed != 0 {
		t.Fatalf("reject without reason: err=%v reviewed=%d", err, repo.reviewed)
	}

	if err := svc.Review(context.Background(), userWith("supervisor", PermApprove), "", 1, 2, true, "ok"); err != nil {
		t.Fatal(err)
	}
	if repo.allowSelf {
		t.Error("a supervisor must not be allowed to approve their own change")
	}
	if err := svc.Review(context.Background(), userWith("admin", PermApprove), "", 1, 2, true, "ok"); err != nil {
		t.Fatal(err)
	}
	if !repo.allowSelf {
		t.Error("admin may self-approve (recorded as such)")
	}
}
