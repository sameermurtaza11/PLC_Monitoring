package ams

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"PLC_Monitoring/internal/store"
)

// EngineRepo is the part of the store the alarm engine needs.
type EngineRepo interface {
	LoadEngineInputs(ctx context.Context) ([]store.EngineAlarm, error)
	ApplyEngineResults(ctx context.Context, states []store.StateUpdate, events []store.NewAlarmEvent, health []store.HealthEvent) error
	TryEngineLock(ctx context.Context) (release func(), ok bool, err error)
	EngineHeartbeat(ctx context.Context, instance string, first bool) (previous time.Time, err error)
	RecordHealth(ctx context.Context, h store.HealthEvent) error
}

// EngineOptions are the engine's timings. Zero values get defaults.
type EngineOptions struct {
	Interval       time.Duration // between evaluation rounds (default 1 s)
	HeartbeatEvery time.Duration // how often liveness is stored (default 5 s)
	GapTolerance   time.Duration // downtime above this is reported as a gap (default 3 x heartbeat)
	LockRetry      time.Duration // retry while another instance holds the engine lock (default 10 s)
	// WarmUp: right after a start the stored values are from before the
	// restart, and acquisition has not scanned yet. For this long a stale or
	// missing value is not reported as a data fault. A fresh failure reported
	// by acquisition (COMM_FAIL / READ_FAIL) is. Default 10 s.
	WarmUp   time.Duration
	Instance string // unique per process
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// Engine continuously evaluates every alarm. It runs on its own goroutine,
// reads the latest PV values the acquisition workers store, and never
// depends on a browser being open or on any HTTP request.
type Engine struct {
	repo EngineRepo
	opt  EngineOptions

	dbDownSince time.Time    // zero while the database is reachable
	startedAt   time.Time    // set by start(); zero = no warm-up (direct Round calls)
	seen        map[int]bool // alarms with a good sample since this engine started
}

func NewEngine(repo EngineRepo, opt EngineOptions) *Engine {
	if opt.Interval <= 0 {
		opt.Interval = time.Second
	}
	if opt.HeartbeatEvery <= 0 {
		opt.HeartbeatEvery = 5 * time.Second
	}
	if opt.GapTolerance <= 0 {
		opt.GapTolerance = 3 * opt.HeartbeatEvery
	}
	if opt.LockRetry <= 0 {
		opt.LockRetry = 10 * time.Second
	}
	if opt.WarmUp <= 0 {
		opt.WarmUp = 10 * time.Second
	}
	if opt.Instance == "" {
		host, _ := os.Hostname()
		opt.Instance = fmt.Sprintf("%s-%d", host, os.Getpid())
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Logf == nil {
		opt.Logf = func(f string, a ...any) { log.Printf("ams: "+f, a...) }
	}
	return &Engine{repo: repo, opt: opt, seen: map[int]bool{}}
}

// Run evaluates alarms until ctx is cancelled. Only one engine may run per
// database: while another instance holds the lock this one waits.
func (e *Engine) Run(ctx context.Context) {
	for ctx.Err() == nil {
		release, ok := e.acquireLock(ctx)
		if !ok {
			return
		}
		e.runLocked(ctx)
		release()
	}
}

func (e *Engine) acquireLock(ctx context.Context) (release func(), ok bool) {
	warned := false
	for ctx.Err() == nil {
		release, got, err := e.repo.TryEngineLock(ctx)
		switch {
		case err != nil:
			e.opt.Logf("engine lock: %v", err)
		case got:
			return release, true
		case !warned:
			e.opt.Logf("another alarm engine instance is running against this database; waiting")
			warned = true
		}
		select {
		case <-ctx.Done():
		case <-time.After(e.opt.LockRetry):
		}
	}
	return nil, false
}

// runLocked is the engine proper. It returns when ctx ends or another
// instance takes over.
func (e *Engine) runLocked(ctx context.Context) {
	e.start(ctx)
	e.opt.Logf("alarm engine started (every %v)", e.opt.Interval)

	ticker := time.NewTicker(e.opt.Interval)
	defer ticker.Stop()
	lastBeat := e.opt.Now()

	for {
		e.step(ctx)

		if now := e.opt.Now(); now.Sub(lastBeat) >= e.opt.HeartbeatEvery {
			lastBeat = now
			if _, err := e.repo.EngineHeartbeat(ctx, e.opt.Instance, false); errors.Is(err, store.ErrEngineSuperseded) {
				e.opt.Logf("another engine instance took over; stopping this one")
				return
			}
		}

		select {
		case <-ctx.Done():
			e.stop()
			return
		case <-ticker.C:
		}
	}
}

// start records the start and reports any time alarms were not evaluated.
func (e *Engine) start(ctx context.Context) {
	now := e.opt.Now()
	e.startedAt = now
	prev, err := e.repo.EngineHeartbeat(ctx, e.opt.Instance, true)
	if err != nil {
		e.opt.Logf("heartbeat: %v", err)
	}
	if !prev.IsZero() && now.Sub(prev) > e.opt.GapTolerance {
		e.health(ctx, store.HealthEvent{
			TS: now, Kind: store.HealthEngineGap, Severity: "critical", Component: "alarm engine",
			Detail: fmt.Sprintf("alarm processing was not running from %s to %s (%s); alarms in that period were not evaluated",
				prev.Format(time.RFC3339), now.Format(time.RFC3339), now.Sub(prev).Round(time.Second)),
		})
	}
	e.health(ctx, store.HealthEvent{
		TS: now, Kind: store.HealthEngineStarted, Severity: "info", Component: "alarm engine",
		Detail: "instance " + e.opt.Instance,
	})
}

func (e *Engine) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e.health(ctx, store.HealthEvent{
		TS: e.opt.Now(), Kind: store.HealthEngineStopped, Severity: "info", Component: "alarm engine",
		Detail: "instance " + e.opt.Instance,
	})
}

func (e *Engine) health(ctx context.Context, h store.HealthEvent) {
	if err := e.repo.RecordHealth(ctx, h); err != nil {
		e.opt.Logf("could not record %s: %v", h.Kind, err)
	}
}

// step runs one round and tracks database outages.
func (e *Engine) step(ctx context.Context) {
	err := e.Round(ctx)
	if ctx.Err() != nil {
		return
	}
	now := e.opt.Now()
	switch {
	case err != nil && e.dbDownSince.IsZero():
		e.dbDownSince = now
		e.opt.Logf("evaluation failed, will retry every round: %v", err)
	case err == nil && !e.dbDownSince.IsZero():
		// Nothing was lost: unsaved transitions are re-derived from the next
		// samples. The outage itself is recorded.
		since := e.dbDownSince
		e.dbDownSince = time.Time{}
		e.opt.Logf("database recovered after %s", now.Sub(since).Round(time.Second))
		e.health(ctx, store.HealthEvent{
			TS: since, Kind: store.HealthEngineDBError, Severity: "critical", Component: "alarm engine",
			Detail: "alarm state could not be stored; alarm timing in this period may be less precise",
		})
		e.health(ctx, store.HealthEvent{
			TS: now, Kind: store.HealthEngineDBRecovery, Severity: "info", Component: "alarm engine",
			Detail: fmt.Sprintf("storage available again after %s", now.Sub(since).Round(time.Second)),
		})
	}
}

// Round evaluates every alarm once and stores the results in one transaction.
// If storing fails nothing is kept and the next round derives the same
// transitions again, so no event is lost while the process runs.
func (e *Engine) Round(ctx context.Context) error {
	alarms, err := e.repo.LoadEngineInputs(ctx)
	if err != nil {
		return fmt.Errorf("load alarms: %w", err)
	}
	now := e.opt.Now()

	var (
		states []store.StateUpdate
		events []store.NewAlarmEvent
		health []store.HealthEvent
		seen   = map[string]bool{} // one data-quality event per PV and kind, not one per alarm
	)
	warming := !e.startedAt.IsZero() && now.Sub(e.startedAt) < e.opt.WarmUp
	for _, a := range alarms {
		// A delay timer stored before a restart must not count the time the
		// engine was not running: on the first good sample it starts over.
		timersRestarted := false
		if !e.seen[a.AlarmID] && SampleValid(a, now) {
			e.seen[a.AlarmID] = true
			before := a.State
			a.State = restartTimers(a.State, *a.SampleTS)
			timersRestarted = !sameTime(before.OnPendingSince, a.State.OnPendingSince) ||
				!sameTime(before.OffPendingSince, a.State.OffPendingSince)
		}
		out := Evaluate(a, now)
		if timersRestarted && !out.Changed {
			out.Changed = true // store the restarted timer, or the next round reloads the old one
		}
		if warming && out.DataEvent == store.HealthDataBad && !freshFailure(a, now) {
			continue // old value from before the restart; acquisition has not scanned yet
		}
		if out.Changed {
			states = append(states, store.StateUpdate{AlarmID: a.AlarmID, State: out.State})
		}
		if out.Event != nil {
			events = append(events, *out.Event)
			e.opt.Logf("%s %s: %s", out.Event.Type, describe(a), valueText(out.Event.Value))
		}
		if out.DataEvent != "" {
			key := fmt.Sprintf("%s/%d", out.DataEvent, a.PVID)
			if seen[key] {
				continue
			}
			seen[key] = true
			pv, al := a.PVID, a.AlarmID
			h := store.HealthEvent{
				TS: now, Kind: out.DataEvent, Severity: "warning",
				Component: a.PLCName + "/" + a.PVName, PVID: &pv, AlarmID: &al,
			}
			if out.DataEvent == store.HealthDataBad {
				h.Detail = "value cannot be trusted: " + dataFault(a, now) + "; alarms on this PV are not being evaluated"
			} else {
				h.Severity, h.Detail = "info", "value is good again; alarm evaluation resumed"
			}
			health = append(health, h)
			e.opt.Logf("%s %s/%s: %s", out.DataEvent, a.PLCName, a.PVName, h.Detail)
		}
	}
	if len(states)+len(events)+len(health) == 0 {
		return nil
	}
	return e.repo.ApplyEngineResults(ctx, states, events, health)
}

func restartTimers(st store.AlarmStateRow, ts time.Time) store.AlarmStateRow {
	if st.OnPendingSince != nil {
		t := ts
		st.OnPendingSince = &t
	}
	if st.OffPendingSince != nil {
		t := ts
		st.OffPendingSince = &t
	}
	return st
}

// freshFailure: acquisition itself recorded a failed read just now.
func freshFailure(a store.EngineAlarm, now time.Time) bool {
	return a.Quality != nil && *a.Quality != store.QualityGood &&
		a.SampleTS != nil && now.Sub(*a.SampleTS) <= StaleAfter(a.ScanIntervalMS)
}

// dataFault says why a value is not trusted.
func dataFault(a store.EngineAlarm, now time.Time) string {
	switch {
	case a.Quality == nil || a.SampleTS == nil:
		return "no data received yet"
	case *a.Quality != store.QualityGood:
		return *a.Quality
	default:
		return fmt.Sprintf("stale, last update %s ago", now.Sub(*a.SampleTS).Round(time.Second))
	}
}

func valueText(v *float64) string {
	if v == nil {
		return "no value"
	}
	return fmt.Sprintf("value %g", *v)
}
