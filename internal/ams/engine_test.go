package ams

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"PLC_Monitoring/internal/store"
)

// memRepo is an in-memory EngineRepo with the same rules as the database:
// states are replaced as a whole, an activation/return is stored once per
// alarm and cycle, and a round is applied all-or-nothing.
type memRepo struct {
	mu sync.Mutex

	alarms []store.EngineAlarm // definitions + latest observation (State is ignored)
	states map[int]store.AlarmStateRow
	events []store.NewAlarmEvent
	health []store.HealthEvent

	failApply  bool
	applyCalls int

	lockBusy   bool
	lockTries  int
	prevBeat   time.Time
	beats      int
	superseded bool
}

func newMemRepo(alarms ...store.EngineAlarm) *memRepo {
	return &memRepo{alarms: alarms, states: map[int]store.AlarmStateRow{}}
}

func (m *memRepo) LoadEngineInputs(context.Context) ([]store.EngineAlarm, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.EngineAlarm, len(m.alarms))
	for i, a := range m.alarms {
		if st, ok := m.states[a.AlarmID]; ok {
			a.State = st
			a.State.Exists = true
		} else {
			a.State = store.AlarmStateRow{}
		}
		out[i] = a
	}
	return out, nil
}

func (m *memRepo) ApplyEngineResults(_ context.Context, states []store.StateUpdate, events []store.NewAlarmEvent, health []store.HealthEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.applyCalls++
	if m.failApply {
		return errors.New("database unavailable")
	}
	for _, e := range events {
		dup := false
		for _, x := range m.events {
			if x.AlarmID == e.AlarmID && x.CycleNo == e.CycleNo && x.Type == e.Type {
				dup = true
			}
		}
		if !dup {
			m.events = append(m.events, e)
		}
	}
	for _, u := range states {
		m.states[u.AlarmID] = u.State
	}
	m.health = append(m.health, health...)
	return nil
}

func (m *memRepo) TryEngineLock(context.Context) (func(), bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lockTries++
	if m.lockBusy {
		return nil, false, nil
	}
	return func() {}, true, nil
}

func (m *memRepo) EngineHeartbeat(_ context.Context, _ string, first bool) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if first {
		return m.prevBeat, nil
	}
	m.beats++
	if m.superseded {
		return time.Time{}, store.ErrEngineSuperseded
	}
	return time.Time{}, nil
}

func (m *memRepo) RecordHealth(_ context.Context, h store.HealthEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.health = append(m.health, h)
	return nil
}

func (m *memRepo) setValue(alarmID int, v float64, q string, ts time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.alarms {
		if m.alarms[i].AlarmID == alarmID {
			val := v
			qq := q
			tt := ts
			m.alarms[i].Value, m.alarms[i].Quality, m.alarms[i].SampleTS = &val, &qq, &tt
		}
	}
}

func (m *memRepo) kinds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, h := range m.health {
		out = append(out, h.Kind)
	}
	return out
}

// engineWithClock builds an engine whose clock the test controls.
func engineWithClock(repo EngineRepo, now *time.Time) *Engine {
	return NewEngine(repo, EngineOptions{
		Now:  func() time.Time { return *now },
		Logf: func(string, ...any) {},
	})
}

func testAlarm(id, pv int) store.EngineAlarm {
	a := hiAlarm()
	a.AlarmID, a.PVID, a.Enabled, a.ScanIntervalMS, a.Version = id, pv, true, 1000, 1
	a.PLCName, a.PVName = "PLC-1", "PT101"
	return a
}

func TestRoundActivatesOnceAndPollingDoesNotRepeatIt(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	now := t0
	eng := engineWithClock(repo, &now)
	ctx := context.Background()

	repo.setValue(1, 9, store.QualityGood, now)
	if err := eng.Round(ctx); err != nil {
		t.Fatal(err)
	}
	callsAfterActivation := repo.applyCalls
	for i := 0; i < 20; i++ {
		now = now.Add(time.Second)
		repo.setValue(1, 9, store.QualityGood, now)
		if err := eng.Round(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.events) != 1 || repo.events[0].Type != store.EventActivated {
		t.Fatalf("events = %+v, want exactly one ACTIVATED", repo.events)
	}
	if repo.applyCalls != callsAfterActivation {
		t.Errorf("database written %d more times while nothing changed", repo.applyCalls-callsAfterActivation)
	}
}

func TestRestartDoesNotDuplicateOrLoseState(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	now := t0
	ctx := context.Background()

	first := engineWithClock(repo, &now)
	repo.setValue(1, 9, store.QualityGood, now)
	if err := first.Round(ctx); err != nil {
		t.Fatal(err)
	}

	// "Restart": a brand-new engine over the same stored state.
	now = now.Add(5 * time.Second)
	second := engineWithClock(repo, &now)
	repo.setValue(1, 9, store.QualityGood, now)
	if err := second.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repo.events) != 1 {
		t.Fatalf("restart produced %d events, want the original 1", len(repo.events))
	}
	if !repo.states[1].ProcessActive || !repo.states[1].Unacknowledged {
		t.Errorf("active state not recovered: %+v", repo.states[1])
	}

	// …and the alarm still returns to normal normally afterwards.
	now = now.Add(time.Second)
	repo.setValue(1, 7, store.QualityGood, now)
	if err := second.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repo.events) != 2 || repo.events[1].Type != store.EventReturnedToNormal || repo.events[1].CycleNo != 1 {
		t.Fatalf("events = %+v", repo.events)
	}
}

func TestDatabaseFailureLosesNothingAndRecoversTheEvent(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	now := t0
	ctx := context.Background()
	eng := engineWithClock(repo, &now)

	repo.failApply = true
	for i := 0; i < 3; i++ {
		repo.setValue(1, 9, store.QualityGood, now)
		if err := eng.Round(ctx); err == nil {
			t.Fatal("expected the storage error to be reported")
		}
		now = now.Add(time.Second)
	}
	if len(repo.events) != 0 || len(repo.states) != 0 {
		t.Fatal("a failed transaction must leave nothing behind")
	}

	repo.failApply = false
	repo.setValue(1, 9, store.QualityGood, now)
	if err := eng.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repo.events) != 1 || repo.events[0].Type != store.EventActivated {
		t.Fatalf("event not recovered after the outage: %+v", repo.events)
	}
}

func TestOutageIsRecordedOnceStorageIsBack(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	now := t0
	eng := engineWithClock(repo, &now)
	ctx := context.Background()

	repo.failApply = true
	repo.setValue(1, 9, store.QualityGood, now)
	eng.step(ctx)
	now = now.Add(10 * time.Second)
	repo.setValue(1, 9, store.QualityGood, now)
	eng.step(ctx)
	if len(repo.health) != 0 {
		t.Fatal("nothing can be stored during the outage")
	}

	repo.failApply = false
	now = now.Add(time.Second)
	repo.setValue(1, 9, store.QualityGood, now)
	eng.step(ctx)
	got := repo.kinds()
	hasErr, hasRec := false, false
	for _, k := range got {
		hasErr = hasErr || k == store.HealthEngineDBError
		hasRec = hasRec || k == store.HealthEngineDBRecovery
	}
	if !hasErr || !hasRec {
		t.Errorf("health events = %v, want ENGINE_DB_ERROR and ENGINE_DB_RECOVERED", got)
	}
}

func TestCommunicationLossIsReportedOncePerPVNotPerAlarm(t *testing.T) {
	hi := testAlarm(1, 1)
	lo := testAlarm(2, 1) // second alarm on the same PV
	lo.Condition = CondLO
	two := 2.0
	lo.Setpoint = &two
	repo := newMemRepo(hi, lo)
	now := t0
	eng := engineWithClock(repo, &now)
	ctx := context.Background()

	repo.setValue(1, 5, store.QualityGood, now)
	repo.setValue(2, 5, store.QualityGood, now)
	if err := eng.Round(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	repo.setValue(1, 0, store.QualityCommFail, now)
	repo.setValue(2, 0, store.QualityCommFail, now)
	for i := 0; i < 3; i++ {
		if err := eng.Round(ctx); err != nil {
			t.Fatal(err)
		}
	}
	bad := 0
	for _, k := range repo.kinds() {
		if k == store.HealthDataBad {
			bad++
		}
	}
	if bad != 1 {
		t.Errorf("%d DATA_BAD events for one PV with two alarms, want 1", bad)
	}
	if repo.states[1].DataValid || repo.states[2].DataValid {
		t.Error("both alarms must be marked as having untrusted data")
	}
}

func TestWarmUpDoesNotReportOldValuesAsDataFaults(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	now := t0
	eng := engineWithClock(repo, &now)
	ctx := context.Background()

	eng.start(ctx)                                              // engine (re)started now
	repo.setValue(1, 5, store.QualityGood, now.Add(-time.Hour)) // value from before the restart
	if err := eng.Round(ctx); err != nil {
		t.Fatal(err)
	}
	for _, k := range repo.kinds() {
		if k == store.HealthDataBad {
			t.Fatal("a stale value right after a restart is not a data fault yet")
		}
	}
	if len(repo.states) != 0 {
		t.Error("nothing should be stored for it")
	}

	// A failure acquisition has just recorded is real and is reported at once.
	repo.setValue(1, 0, store.QualityCommFail, now)
	if err := eng.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if got := repo.kinds(); got[len(got)-1] != store.HealthDataBad {
		t.Fatalf("fresh COMM_FAIL during warm-up not reported: %v", got)
	}
}

func TestStaleValueIsReportedOnceWarmUpIsOver(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	now := t0
	eng := engineWithClock(repo, &now)
	ctx := context.Background()
	eng.start(ctx)
	repo.setValue(1, 5, store.QualityGood, now.Add(-time.Hour))

	now = now.Add(11 * time.Second) // warm-up (10 s) is over and still no fresh data
	if err := eng.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if got := repo.kinds(); got[len(got)-1] != store.HealthDataBad {
		t.Fatalf("stale data after warm-up must be reported: %v", got)
	}
}

func TestDelayTimerFromBeforeARestartDoesNotCountTheDowntime(t *testing.T) {
	a := testAlarm(1, 1)
	a.OnDelayS = 30
	repo := newMemRepo(a)
	// Stored before the restart: condition has been true "since" an hour ago.
	hourAgo := t0.Add(-time.Hour)
	repo.states[1] = store.AlarmStateRow{Exists: true, DataValid: true, OnPendingSince: &hourAgo}

	now := t0
	eng := engineWithClock(repo, &now)
	ctx := context.Background()
	repo.setValue(1, 9, store.QualityGood, now)
	if err := eng.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repo.events) != 0 {
		t.Fatal("activated immediately because the engine was down for an hour")
	}
	if p := repo.states[1].OnPendingSince; p == nil || !p.Equal(now) {
		t.Fatalf("restarted timer not stored: %v", p)
	}

	// The restarted timer survives the next rounds and the alarm activates 30 s later.
	for i := 1; i <= 29; i++ {
		now = now.Add(time.Second)
		repo.setValue(1, 9, store.QualityGood, now)
		if err := eng.Round(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.events) != 0 {
		t.Fatal("activated before the on-delay ended")
	}
	now = now.Add(time.Second)
	repo.setValue(1, 9, store.QualityGood, now)
	if err := eng.Round(ctx); err != nil {
		t.Fatal(err)
	}
	if len(repo.events) != 1 {
		t.Fatalf("events = %+v, want ACTIVATED after 30 s of continuous alarm", repo.events)
	}
}

func TestRestartReportsTheGapWhenAlarmsWereNotEvaluated(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	repo.prevBeat = t0.Add(-10 * time.Minute)
	now := t0
	eng := engineWithClock(repo, &now)
	eng.start(context.Background())
	kinds := repo.kinds()
	if len(kinds) != 2 || kinds[0] != store.HealthEngineGap || kinds[1] != store.HealthEngineStarted {
		t.Fatalf("health events = %v, want ENGINE_GAP then ENGINE_STARTED", kinds)
	}
	if repo.health[0].Severity != "critical" {
		t.Error("a gap in alarm processing must be critical")
	}
}

func TestShortRestartIsNotReportedAsAGap(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	repo.prevBeat = t0.Add(-4 * time.Second)
	now := t0
	engineWithClock(repo, &now).start(context.Background())
	if kinds := repo.kinds(); len(kinds) != 1 || kinds[0] != store.HealthEngineStarted {
		t.Fatalf("health events = %v, want only ENGINE_STARTED", kinds)
	}
}

func TestSecondInstanceWaitsAndDoesNotEvaluate(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	repo.lockBusy = true
	repo.setValue(1, 9, store.QualityGood, time.Now())
	eng := NewEngine(repo, EngineOptions{Interval: 5 * time.Millisecond, LockRetry: 5 * time.Millisecond, Logf: func(string, ...any) {}})

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	eng.Run(ctx)

	if repo.lockTries < 2 {
		t.Errorf("lock tried %d times, want repeated attempts", repo.lockTries)
	}
	if repo.applyCalls != 0 || len(repo.events) != 0 {
		t.Error("an instance without the lock must not evaluate alarms")
	}
}

func TestRunEvaluatesAndStopsCleanly(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	eng := NewEngine(repo, EngineOptions{Interval: 5 * time.Millisecond, Logf: func(string, ...any) {}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { eng.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		repo.setValue(1, 9, store.QualityGood, time.Now())
		repo.mu.Lock()
		n := len(repo.events)
		repo.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not stop")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.events) != 1 {
		t.Fatalf("events = %+v", repo.events)
	}
	k := repo.health
	if len(k) < 2 || k[0].Kind != store.HealthEngineStarted || k[len(k)-1].Kind != store.HealthEngineStopped {
		t.Errorf("health events should start with ENGINE_STARTED and end with ENGINE_STOPPED: %+v", k)
	}
}

func TestEngineStopsWhenSuperseded(t *testing.T) {
	repo := newMemRepo(testAlarm(1, 1))
	repo.superseded = true
	eng := NewEngine(repo, EngineOptions{
		Interval: 2 * time.Millisecond, HeartbeatEvery: 4 * time.Millisecond, LockRetry: time.Hour,
		Logf: func(string, ...any) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	eng.runLocked(ctx) // returns by itself when the heartbeat says another instance took over
	if ctx.Err() != nil {
		t.Fatal("engine kept running after being superseded")
	}
}
