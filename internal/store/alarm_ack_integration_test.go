package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// Integration test against a REAL PostgreSQL database. It is skipped unless
// PLC_TEST_DATABASE_URL is set, and it refuses to run on any database whose
// name does not contain "scratch" or "test", because it writes alarm history
// that can never be deleted (the history tables are append-only).
//
//	$env:PLC_TEST_DATABASE_URL = "postgres://postgres@localhost:5432/plc_ams_scratch"
//	go test ./internal/store/ -run Integration -v
func openTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	url := os.Getenv("PLC_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PLC_TEST_DATABASE_URL not set; skipping database integration test")
	}
	ctx := context.Background()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)

	var name string
	if err := s.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if !containsAny(name, "scratch", "test") {
		t.Fatalf("refusing to run on database %q: name must contain 'scratch' or 'test'", name)
	}
	return s, ctx
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		for i := 0; i+len(p) <= len(s); i++ {
			if s[i:i+len(p)] == p {
				return true
			}
		}
	}
	return false
}

// activate stores an activation the way the alarm engine does.
func activate(t *testing.T, s *Store, ctx context.Context, a EngineAlarm, cycle int, value float64) {
	t.Helper()
	now := time.Now().UTC()
	ver := a.Version
	st := AlarmStateRow{
		ProcessActive: true, Unacknowledged: true, CycleNo: cycle, DataValid: true,
		ActiveVersion: &ver, ActivatedAt: &now, ActivationValue: &value, LastValue: &value, LastSampleTS: &now,
	}
	ev := NewAlarmEvent{AlarmID: a.AlarmID, Version: a.Version, CycleNo: cycle, Type: EventActivated, EventTS: now, Value: &value}
	if err := s.ApplyEngineResults(ctx, []StateUpdate{{AlarmID: a.AlarmID, State: st}}, []NewAlarmEvent{ev}, nil); err != nil {
		t.Fatalf("activate cycle %d: %v", cycle, err)
	}
}

func current(t *testing.T, s *Store, ctx context.Context, alarmID int) AlarmCurrent {
	t.Helper()
	all, err := s.ListAlarmCurrent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.AlarmID == alarmID {
			return a
		}
	}
	t.Fatalf("alarm %d not found", alarmID)
	return AlarmCurrent{}
}

func countEvents(t *testing.T, s *Store, ctx context.Context, alarmID, cycle int, typ string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_events WHERE alarm_id=$1 AND cycle_no=$2 AND event_type=$3`,
		alarmID, cycle, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIntegrationAcknowledgement(t *testing.T) {
	s, ctx := openTestStore(t)

	alarms, err := s.LoadEngineInputs(ctx)
	if err != nil || len(alarms) < 2 {
		t.Fatalf("need at least two approved alarms in the test database: %v (%d)", err, len(alarms))
	}
	var a, untouched EngineAlarm
	for _, x := range alarms {
		if !x.State.Exists {
			if a.AlarmID == 0 {
				a = x
			} else if untouched.AlarmID == 0 {
				untouched = x
			}
		}
	}
	if a.AlarmID == 0 || untouched.AlarmID == 0 {
		t.Fatal("need two alarms without state in the test database (use a fresh copy)")
	}

	// A test user (the acknowledgement records who did it).
	username := fmt.Sprintf("it_%d", time.Now().UnixNano())
	if err := s.CreateUser(ctx, username, "IT", "$2a$04$unusedunusedunusedunuseduRrQ8UqQ8UqQ8UqQ8UqQ8UqQ8UqQ", "operator"); err != nil {
		t.Fatal(err)
	}
	cred, err := s.UserCredentialsByName(ctx, username)
	if err != nil {
		t.Fatal(err)
	}
	actor := Actor{ID: cred.ID, Username: username, IP: "127.0.0.1"}

	t.Run("nothing to acknowledge before the first activation", func(t *testing.T) {
		if _, err := s.AcknowledgeAlarm(ctx, untouched.AlarmID, 1, actor); !errors.Is(err, ErrNothingToAck) {
			t.Fatalf("got %v, want ErrNothingToAck", err)
		}
	})

	activate(t, s, ctx, a, 1, 91)

	t.Run("acknowledging keeps the alarm active and records who and when", func(t *testing.T) {
		res, err := s.AcknowledgeAlarm(ctx, a.AlarmID, 1, actor)
		if err != nil || res.Result != AckAcknowledged || res.By != username {
			t.Fatalf("got %+v, %v", res, err)
		}
		c := current(t, s, ctx, a.AlarmID)
		if !c.ProcessActive {
			t.Error("acknowledging must not clear the process condition")
		}
		if c.Unacknowledged || c.Phase != PhaseActiveAck {
			t.Errorf("phase = %s unack=%v, want active_ack", c.Phase, c.Unacknowledged)
		}
		if c.AcknowledgedBy == nil || *c.AcknowledgedBy != username || c.AcknowledgedAt == nil {
			t.Errorf("who/when not recorded: %+v", c)
		}
		if n := countEvents(t, s, ctx, a.AlarmID, 1, "ACKNOWLEDGED"); n != 1 {
			t.Errorf("%d ACKNOWLEDGED events, want 1", n)
		}
	})

	t.Run("a repeated acknowledgement changes nothing", func(t *testing.T) {
		res, err := s.AcknowledgeAlarm(ctx, a.AlarmID, 1, actor)
		if err != nil || res.Result != AckAlreadyAcknowledged || res.By != username {
			t.Fatalf("got %+v, %v", res, err)
		}
		if n := countEvents(t, s, ctx, a.AlarmID, 1, "ACKNOWLEDGED"); n != 1 {
			t.Errorf("%d ACKNOWLEDGED events after a repeat, want 1", n)
		}
	})

	t.Run("an engine write based on an older read cannot undo the acknowledgement", func(t *testing.T) {
		// The engine loaded the state before the operator clicked (unack = true)
		// and now writes something unrelated back (a delay counter).
		now := time.Now().UTC()
		stale := AlarmStateRow{ProcessActive: true, Unacknowledged: true, CycleNo: 1, DataValid: true, DelayFiltered: 3, LastSampleTS: &now}
		if err := s.ApplyEngineResults(ctx, []StateUpdate{{AlarmID: a.AlarmID, State: stale}}, nil, nil); err != nil {
			t.Fatal(err)
		}
		c := current(t, s, ctx, a.AlarmID)
		if c.Unacknowledged || c.AcknowledgedBy == nil {
			t.Fatalf("acknowledgement was overwritten by the engine: %+v", c)
		}
	})

	t.Run("returning to normal does not acknowledge, and an unacknowledged return stays visible", func(t *testing.T) {
		activate(t, s, ctx, a, 2, 95) // new activation: acknowledgement starts over
		c := current(t, s, ctx, a.AlarmID)
		if !c.Unacknowledged || c.AcknowledgedBy != nil || c.Phase != PhaseActiveUnack {
			t.Fatalf("new activation must be unacknowledged: %+v", c)
		}
		now := time.Now().UTC()
		st := AlarmStateRow{ProcessActive: false, Unacknowledged: true, CycleNo: 2, DataValid: true, ReturnedAt: &now}
		ev := NewAlarmEvent{AlarmID: a.AlarmID, Version: a.Version, CycleNo: 2, Type: EventReturnedToNormal, EventTS: now}
		if err := s.ApplyEngineResults(ctx, []StateUpdate{{AlarmID: a.AlarmID, State: st}}, []NewAlarmEvent{ev}, nil); err != nil {
			t.Fatal(err)
		}
		c = current(t, s, ctx, a.AlarmID)
		if c.ProcessActive || !c.Unacknowledged || c.Phase != PhaseRTNUnack {
			t.Fatalf("phase = %s, want rtn_unack: %+v", c.Phase, c)
		}
		// Acknowledging the returned alarm completes its life cycle.
		if res, err := s.AcknowledgeAlarm(ctx, a.AlarmID, 2, actor); err != nil || res.Result != AckAcknowledged {
			t.Fatalf("got %+v, %v", res, err)
		}
		if c = current(t, s, ctx, a.AlarmID); c.Phase != PhaseNormal {
			t.Errorf("phase = %s, want normal", c.Phase)
		}
	})

	t.Run("an old activation cannot acknowledge a newer one", func(t *testing.T) {
		activate(t, s, ctx, a, 3, 97)
		_, err := s.AcknowledgeAlarm(ctx, a.AlarmID, 2, actor) // operator still looking at activation 2
		if !errors.Is(err, ErrAckStale) {
			t.Fatalf("got %v, want ErrAckStale", err)
		}
		if c := current(t, s, ctx, a.AlarmID); !c.Unacknowledged {
			t.Error("the newer activation must still be unacknowledged")
		}
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM alarm_audit_log WHERE action='alarm.acknowledge' AND outcome='rejected' AND target_id=$1`,
			fmt.Sprint(a.AlarmID)).Scan(&n); err != nil || n < 1 {
			t.Errorf("the refused acknowledgement should be audited (rows=%d, err=%v)", n, err)
		}
	})

	t.Run("simultaneous acknowledgements produce exactly one", func(t *testing.T) {
		const workers = 12
		var wg sync.WaitGroup
		results := make([]string, workers)
		start := make(chan struct{})
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				res, err := s.AcknowledgeAlarm(ctx, a.AlarmID, 3, actor)
				if err != nil {
					results[i] = "error: " + err.Error()
					return
				}
				results[i] = res.Result
			}(i)
		}
		close(start)
		wg.Wait()

		acked, already := 0, 0
		for _, r := range results {
			switch r {
			case AckAcknowledged:
				acked++
			case AckAlreadyAcknowledged:
				already++
			default:
				t.Errorf("unexpected result %q", r)
			}
		}
		if acked != 1 || already != workers-1 {
			t.Errorf("acknowledged=%d already=%d, want 1 and %d", acked, already, workers-1)
		}
		if n := countEvents(t, s, ctx, a.AlarmID, 3, "ACKNOWLEDGED"); n != 1 {
			t.Errorf("%d ACKNOWLEDGED events for one activation, want 1", n)
		}
	})
}
