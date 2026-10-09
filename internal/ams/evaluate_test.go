package ams

import (
	"testing"
	"time"

	"PLC_Monitoring/internal/store"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// sim drives Evaluate the way the engine does: one sample per second, the
// returned state fed back into the next call.
type sim struct {
	t       *testing.T
	alarm   store.EngineAlarm
	now     time.Time
	events  []store.NewAlarmEvent
	health  []string
	changes int
}

func newSim(t *testing.T, a store.EngineAlarm) *sim {
	a.Enabled = true
	a.ScanIntervalMS = 1000
	a.Version = 3
	return &sim{t: t, alarm: a, now: t0}
}

// hiAlarm: HI at 8.0 with 0.2 deadband (the example in the AMS briefing).
func hiAlarm() store.EngineAlarm {
	sp := 8.0
	return store.EngineAlarm{AlarmID: 1, PVID: 1, Condition: CondHI, Setpoint: &sp, Deadband: 0.2}
}

// feed gives one good sample taken `now`, then advances the clock 1 s.
func (s *sim) feed(v float64) Outcome {
	q := store.QualityGood
	return s.feedQ(&v, &q)
}

func (s *sim) feedQ(v *float64, q *string) Outcome {
	ts := s.now
	s.alarm.Value, s.alarm.Quality, s.alarm.SampleTS = v, q, &ts
	out := Evaluate(s.alarm, s.now)
	s.alarm.State = out.State
	s.alarm.State.Exists = s.alarm.State.Exists || out.Changed
	if out.Changed {
		s.changes++
	}
	if out.Event != nil {
		s.events = append(s.events, *out.Event)
	}
	if out.DataEvent != "" {
		s.health = append(s.health, out.DataEvent)
	}
	s.now = s.now.Add(time.Second)
	return out
}

func (s *sim) feedAll(vals ...float64) {
	for _, v := range vals {
		s.feed(v)
	}
}

func (s *sim) types() []string {
	var out []string
	for _, e := range s.events {
		out = append(out, e.Type)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCrossingLimitCreatesExactlyOneActivation(t *testing.T) {
	s := newSim(t, hiAlarm())
	s.feedAll(7.0, 7.9, 8.0, 8.5, 9.0)
	if got := s.types(); !equal(got, []string{store.EventActivated}) {
		t.Fatalf("events = %v, want one ACTIVATED", got)
	}
	e := s.events[0]
	if e.Version != 3 || e.CycleNo != 1 || e.Value == nil || *e.Value != 8.0 || e.Limit == nil || *e.Limit != 8.0 {
		t.Errorf("event content wrong: %+v", e)
	}
	if !e.EventTS.Equal(t0.Add(2 * time.Second)) {
		t.Errorf("event time = %v, want the sample time of the crossing", e.EventTS)
	}
	if !s.alarm.State.ProcessActive || !s.alarm.State.Unacknowledged {
		t.Errorf("state after activation: %+v", s.alarm.State)
	}
}

func TestUnchangedActiveAlarmCreatesNoMoreEventsOrWrites(t *testing.T) {
	s := newSim(t, hiAlarm())
	s.feedAll(9.0) // activates
	changesAfterActivation := s.changes
	for i := 0; i < 50; i++ {
		s.feed(9.0)
	}
	if len(s.events) != 1 {
		t.Fatalf("%d events after 50 more polls, want 1", len(s.events))
	}
	if s.changes != changesAfterActivation {
		t.Errorf("state was written %d extra times for an unchanged alarm", s.changes-changesAfterActivation)
	}
}

func TestEvaluatingTheSameSampleTwiceChangesNothing(t *testing.T) {
	s := newSim(t, hiAlarm())
	s.feed(9.0)
	again := Evaluate(s.alarm, s.now) // same sample, evaluated again (e.g. after a restart)
	if again.Event != nil || again.Changed || again.DataEvent != "" {
		t.Errorf("re-evaluation produced %+v", again)
	}
}

func TestDeadbandStopsOscillationAroundTheLimit(t *testing.T) {
	s := newSim(t, hiAlarm())
	// Hovers 7.85…8.05 around the 8.0 limit: activates once, never returns
	// (clears only at <= 7.8).
	s.feedAll(8.05, 7.95, 8.05, 7.85, 8.05, 7.9, 8.01, 7.85)
	if got := s.types(); !equal(got, []string{store.EventActivated}) {
		t.Fatalf("events = %v, want a single ACTIVATED", got)
	}
	s.feed(7.8) // at limit - deadband
	if got := s.types(); !equal(got, []string{store.EventActivated, store.EventReturnedToNormal}) {
		t.Fatalf("events = %v, want return at 7.8", got)
	}
	if s.alarm.State.ProcessActive {
		t.Error("alarm should be inactive after return")
	}
	if !s.alarm.State.Unacknowledged {
		t.Error("returning to normal must not acknowledge the alarm")
	}
}

func TestLowAlarmHysteresis(t *testing.T) {
	sp := 2.0
	s := newSim(t, store.EngineAlarm{AlarmID: 2, Condition: CondLO, Setpoint: &sp, Deadband: 0.5})
	s.feedAll(3.0, 2.0, 2.3, 2.49)
	if got := s.types(); !equal(got, []string{store.EventActivated}) {
		t.Fatalf("events = %v", got)
	}
	s.feed(2.5) // limit + deadband
	if got := s.types(); !equal(got, []string{store.EventActivated, store.EventReturnedToNormal}) {
		t.Fatalf("events = %v", got)
	}
}

func TestOnDelayFiltersShortExcursionsAndCountsThem(t *testing.T) {
	a := hiAlarm()
	a.OnDelayS = 3
	s := newSim(t, a)
	s.feedAll(9, 9, 7) // 2 s above the limit, then gone: no alarm
	if len(s.events) != 0 {
		t.Fatalf("events = %v, want none", s.types())
	}
	if s.alarm.State.DelayFiltered != 1 {
		t.Errorf("filtered excursions = %d, want 1 (they must stay visible)", s.alarm.State.DelayFiltered)
	}
	s.feedAll(9, 9, 9) // 2 s elapsed
	if len(s.events) != 0 {
		t.Fatal("activated before the on-delay ended")
	}
	s.feed(9) // 3 s elapsed
	if got := s.types(); !equal(got, []string{store.EventActivated}) {
		t.Fatalf("events = %v, want ACTIVATED after the on-delay", got)
	}
}

func TestOffDelayHoldsTheAlarmUntilNormalLongEnough(t *testing.T) {
	a := hiAlarm()
	a.OffDelayS = 3
	s := newSim(t, a)
	s.feed(9)          // active
	s.feedAll(7, 7, 9) // normal for 1 s, then abnormal again: still active, timer reset
	s.feedAll(7, 7, 7) // 2 s normal
	if got := s.types(); !equal(got, []string{store.EventActivated}) {
		t.Fatalf("events = %v, want only ACTIVATED so far", got)
	}
	s.feed(7) // 3 s normal
	if got := s.types(); !equal(got, []string{store.EventActivated, store.EventReturnedToNormal}) {
		t.Fatalf("events = %v", got)
	}
}

func TestSecondActivationStartsANewCycle(t *testing.T) {
	s := newSim(t, hiAlarm())
	s.feedAll(9, 7, 9)
	if len(s.events) != 3 || s.events[0].CycleNo != 1 || s.events[1].CycleNo != 1 || s.events[2].CycleNo != 2 {
		t.Fatalf("cycles wrong: %+v", s.events)
	}
}

func TestDiscreteAlarm(t *testing.T) {
	trip := 1
	s := newSim(t, store.EngineAlarm{AlarmID: 3, Condition: CondDiscrete, TriggerState: &trip})
	s.feedAll(0, 0, 1, 1, 1)
	if got := s.types(); !equal(got, []string{store.EventActivated}) {
		t.Fatalf("events = %v", got)
	}
	s.feed(0)
	if got := s.types(); !equal(got, []string{store.EventActivated, store.EventReturnedToNormal}) {
		t.Fatalf("events = %v", got)
	}
	if s.events[0].Limit == nil || *s.events[0].Limit != 1 {
		t.Errorf("discrete limit should be the trigger state: %+v", s.events[0])
	}
}

// --- data quality: a lost value is never "normal" -----------------------

func TestBadDataKeepsActiveAlarmActiveAndIsReported(t *testing.T) {
	s := newSim(t, hiAlarm())
	s.feed(9) // active
	bad := store.QualityCommFail
	s.feedQ(nil, &bad) // PLC lost
	s.feedQ(nil, &bad)
	if !s.alarm.State.ProcessActive {
		t.Fatal("communication loss must not clear an active alarm")
	}
	if s.alarm.State.DataValid {
		t.Error("data should be flagged invalid")
	}
	if !equal(s.health, []string{store.HealthDataBad}) {
		t.Fatalf("health events = %v, want one DATA_BAD (not one per poll)", s.health)
	}
	s.feed(7) // data back and normal
	if !equal(s.health, []string{store.HealthDataBad, store.HealthDataRestored}) {
		t.Fatalf("health events = %v", s.health)
	}
	if got := s.types(); !equal(got, []string{store.EventActivated, store.EventReturnedToNormal}) {
		t.Fatalf("events = %v, want return once data is back and normal", got)
	}
}

func TestBadDataDoesNotCreateOrCompleteAnAlarm(t *testing.T) {
	a := hiAlarm()
	a.OnDelayS = 2
	s := newSim(t, a)
	s.feedAll(9, 9) // on-delay running
	bad := store.QualityReadFail
	s.feedQ(nil, &bad)
	s.feedAll(9, 9) // restarts the delay: the lost time cannot count
	if len(s.events) != 0 {
		t.Fatalf("activated across a data gap: %v", s.types())
	}
}

func TestStaleValueIsTreatedAsBadData(t *testing.T) {
	s := newSim(t, hiAlarm())
	s.feed(9)
	// Same old sample, evaluated 30 s later: the PLC stopped updating.
	late := Evaluate(s.alarm, s.now.Add(30*time.Second))
	if late.State.DataValid || late.DataEvent != store.HealthDataBad {
		t.Errorf("stale value not flagged: %+v", late)
	}
	if !late.State.ProcessActive {
		t.Error("stale data must not clear the alarm")
	}
}

func TestMissingValueIsBadData(t *testing.T) {
	a := hiAlarm()
	a.Enabled, a.ScanIntervalMS = true, 1000
	out := Evaluate(a, t0) // no pv_latest row at all
	if out.DataEvent != store.HealthDataBad || out.Event != nil {
		t.Errorf("no value: %+v", out)
	}
}

func TestDisabledAlarmIsNotEvaluatedAndClosesIfActive(t *testing.T) {
	s := newSim(t, hiAlarm())
	s.feed(9) // active
	s.alarm.Enabled = false
	out := s.feed(9)
	if out.Event == nil || out.Event.Type != store.EventReturnedToNormal || out.Event.Note == "" {
		t.Fatalf("disabling an active alarm must close it with a note: %+v", out.Event)
	}
	if again := s.feed(9); again.Event != nil || again.Changed {
		t.Errorf("a disabled alarm must stay quiet: %+v", again)
	}
}

func TestNewAlarmWithNormalValueWritesNothing(t *testing.T) {
	s := newSim(t, hiAlarm())
	for i := 0; i < 20; i++ {
		s.feed(5)
	}
	if s.changes != 0 || len(s.events) != 0 {
		t.Errorf("a normal, never-triggered alarm wrote %d changes / %d events", s.changes, len(s.events))
	}
}
