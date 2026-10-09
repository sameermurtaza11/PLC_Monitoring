package ams

import (
	"context"
	"errors"
	"testing"

	"PLC_Monitoring/internal/store"
)

type fakeAckRepo struct {
	calls  []ackCall
	audits []store.AuditEntry
	result store.AckResult
	err    error
}

type ackCall struct {
	alarmID, cycleNo int
	actor            store.Actor
}

func (f *fakeAckRepo) AcknowledgeAlarm(_ context.Context, alarmID, cycleNo int, actor store.Actor) (store.AckResult, error) {
	f.calls = append(f.calls, ackCall{alarmID, cycleNo, actor})
	return f.result, f.err
}

func (f *fakeAckRepo) RecordAudit(_ context.Context, e store.AuditEntry) error {
	f.audits = append(f.audits, e)
	return nil
}

func TestAcknowledgeUsesTheSessionUserAsActor(t *testing.T) {
	repo := &fakeAckRepo{result: store.AckResult{Result: store.AckAcknowledged, CycleNo: 4}}
	svc := NewAckService(repo)

	res, err := svc.Acknowledge(context.Background(), userWith("operator", PermAck), "10.0.0.7", 12, 4)
	if err != nil || res.Result != store.AckAcknowledged {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(repo.calls) != 1 {
		t.Fatalf("%d calls", len(repo.calls))
	}
	c := repo.calls[0]
	if c.alarmID != 12 || c.cycleNo != 4 || c.actor.ID != 1 || c.actor.Username != "tester" || c.actor.IP != "10.0.0.7" {
		t.Errorf("wrong call: %+v", c)
	}
}

func TestAcknowledgeNeedsPermissionAndRefusalIsAudited(t *testing.T) {
	repo := &fakeAckRepo{}
	svc := NewAckService(repo)

	for _, u := range []struct {
		name string
		call func() error
	}{
		{"viewer", func() error {
			_, err := svc.Acknowledge(context.Background(), userWith("viewer", PermView), "", 1, 1)
			return err
		}},
		{"anonymous", func() error {
			_, err := svc.Acknowledge(context.Background(), nil, "", 1, 1)
			return err
		}},
	} {
		if err := u.call(); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: got %v, want ErrForbidden", u.name, err)
		}
	}
	if len(repo.calls) != 0 {
		t.Error("nothing may be acknowledged without the permission")
	}
	if len(repo.audits) != 2 || repo.audits[0].Outcome != "denied" || repo.audits[0].Actor.Username != "tester" {
		t.Errorf("refusals must be audited: %+v", repo.audits)
	}
}

func TestAcknowledgeRejectsMissingIdentifiers(t *testing.T) {
	repo := &fakeAckRepo{}
	svc := NewAckService(repo)
	for _, c := range [][2]int{{0, 1}, {1, 0}, {-1, 3}} {
		if _, err := svc.Acknowledge(context.Background(), userWith("operator", PermAck), "", c[0], c[1]); !errors.Is(err, ErrBadRequest) {
			t.Errorf("alarm %d cycle %d: got %v, want ErrBadRequest", c[0], c[1], err)
		}
	}
	if len(repo.calls) != 0 {
		t.Error("bad requests must not reach the store")
	}
}

func TestAcknowledgePassesStoreErrorsThrough(t *testing.T) {
	for _, want := range []error{store.ErrAckStale, store.ErrNothingToAck} {
		repo := &fakeAckRepo{err: want}
		_, err := NewAckService(repo).Acknowledge(context.Background(), userWith("operator", PermAck), "", 1, 1)
		if !errors.Is(err, want) {
			t.Errorf("got %v, want %v", err, want)
		}
	}
}

func TestAlarmPhaseKeepsAcknowledgedActiveAlarmsActive(t *testing.T) {
	cases := []struct {
		active, unack bool
		cycle         int
		want          string
	}{
		{true, true, 1, store.PhaseActiveUnack},
		{true, false, 1, store.PhaseActiveAck}, // acknowledged but still abnormal: stays on the active list
		{false, true, 1, store.PhaseRTNUnack},  // returned before anyone acknowledged
		{false, false, 1, store.PhaseNormal},
		{false, false, 0, store.PhaseNormal}, // never triggered
	}
	for _, c := range cases {
		if got := store.AlarmPhase(c.active, c.unack, c.cycle); got != c.want {
			t.Errorf("AlarmPhase(%v,%v,%d) = %s, want %s", c.active, c.unack, c.cycle, got, c.want)
		}
	}
}
