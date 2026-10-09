package ams

import (
	"fmt"
	"time"

	"PLC_Monitoring/internal/store"
)

// Outcome is the result of evaluating one alarm once.
type Outcome struct {
	State     store.AlarmStateRow // state after this evaluation
	Changed   bool                // State differs in a way that must be stored
	Event     *store.NewAlarmEvent
	DataEvent string // store.HealthDataBad / HealthDataRestored on a data-quality change, else ""
}

// StaleAfter is how old a value may be before it is no longer trusted: three
// scan intervals, at least 5 s (the same rule the dashboard uses).
func StaleAfter(scanIntervalMS int) time.Duration {
	return max(3*time.Duration(scanIntervalMS)*time.Millisecond, 5*time.Second)
}

// Evaluate is the whole alarm logic for one alarm and one observation. It
// performs no I/O and reads no clock except the `now` it is given, so every
// rule can be tested exactly.
//
// Rules:
//   - A value that is not GOOD, missing, or stale is never treated as normal:
//     the alarm keeps its process state, timers stop, and the data fault is
//     reported. It resumes when good data returns.
//   - Activation: the condition (HI/HIHI >= limit, LO/LOLO <= limit, DISCRETE
//     == trigger state) must hold for the on-delay, measured on the PLC sample
//     timestamps, not on the polling rate.
//   - Return to normal: the value must be back past limit -/+ deadband for
//     the off-delay. Inside the deadband the alarm stays active.
//   - Time moves only with new samples, so evaluating the same sample again
//     changes nothing (idempotent): no duplicate events.
func Evaluate(a store.EngineAlarm, now time.Time) Outcome {
	st := a.State
	st.DataValid = a.State.DataValid || !a.State.Exists // a new alarm starts with valid data
	old := st
	out := Outcome{}

	// A disabled alarm is not evaluated. If it was active, close it so it
	// does not stay on the operator's list forever.
	if !a.Enabled {
		if st.ProcessActive {
			ts := now
			if a.SampleTS != nil {
				ts = *a.SampleTS
			}
			out.Event = returnEvent(a, &st, ts, "alarm disabled by configuration")
		}
		st.OnPendingSince, st.OffPendingSince = nil, nil
		return finish(out, old, st)
	}

	if !SampleValid(a, now) {
		if st.DataValid {
			st.DataValid = false
			out.DataEvent = store.HealthDataBad
		}
		// Cannot confirm anything: a running on-delay or off-delay is abandoned,
		// an active alarm stays active.
		st.OnPendingSince, st.OffPendingSince = nil, nil
		return finish(out, old, st)
	}

	if !st.DataValid {
		st.DataValid = true
		out.DataEvent = store.HealthDataRestored
	}

	ts, v := *a.SampleTS, *a.Value
	st.LastValue, st.LastSampleTS = &v, &ts
	inAlarm := inAlarmBand(a, v)
	cleared := clearedBand(a, v)

	if !st.ProcessActive {
		switch {
		case inAlarm:
			if st.OnPendingSince == nil {
				st.OnPendingSince = &ts
			}
			if ts.Sub(*st.OnPendingSince) >= time.Duration(a.OnDelayS)*time.Second {
				out.Event = activateEvent(a, &st, ts, v)
			}
		case st.OnPendingSince != nil:
			// Went back to normal before the on-delay ended: no alarm, but count it.
			st.OnPendingSince = nil
			st.DelayFiltered++
		}
		return finish(out, old, st)
	}

	// Alarm is active.
	switch {
	case cleared:
		if st.OffPendingSince == nil {
			st.OffPendingSince = &ts
		}
		if ts.Sub(*st.OffPendingSince) >= time.Duration(a.OffDelayS)*time.Second {
			out.Event = returnEvent(a, &st, ts, "")
		}
	default:
		// Still abnormal or inside the deadband: not returned.
		st.OffPendingSince = nil
	}
	return finish(out, old, st)
}

// SampleValid: the observation is a good, present and recent value.
func SampleValid(a store.EngineAlarm, now time.Time) bool {
	return a.Quality != nil && *a.Quality == store.QualityGood &&
		a.Value != nil && a.SampleTS != nil &&
		now.Sub(*a.SampleTS) <= StaleAfter(a.ScanIntervalMS)
}

// limitOf returns the limit as shown in events: the setpoint, or the trigger state.
func limitOf(a store.EngineAlarm) *float64 {
	switch {
	case a.Setpoint != nil:
		sp := *a.Setpoint
		return &sp
	case a.TriggerState != nil:
		t := float64(*a.TriggerState)
		return &t
	}
	return nil
}

// inAlarmBand: the raw alarm condition, before delay and hysteresis.
func inAlarmBand(a store.EngineAlarm, v float64) bool {
	switch a.Condition {
	case CondHI, CondHIHI:
		return a.Setpoint != nil && v >= *a.Setpoint
	case CondLO, CondLOLO:
		return a.Setpoint != nil && v <= *a.Setpoint
	case CondDiscrete:
		return a.TriggerState != nil && stateOf(v) == *a.TriggerState
	}
	return false
}

// clearedBand: the value is far enough back from the limit (by the deadband)
// to count as normal. For DISCRETE there is no deadband.
func clearedBand(a store.EngineAlarm, v float64) bool {
	switch a.Condition {
	case CondHI, CondHIHI:
		return a.Setpoint != nil && v <= *a.Setpoint-a.Deadband
	case CondLO, CondLOLO:
		return a.Setpoint != nil && v >= *a.Setpoint+a.Deadband
	case CondDiscrete:
		return a.TriggerState != nil && stateOf(v) != *a.TriggerState
	}
	return false
}

func stateOf(v float64) int {
	if v >= 0.5 {
		return 1
	}
	return 0
}

func activateEvent(a store.EngineAlarm, st *store.AlarmStateRow, ts time.Time, v float64) *store.NewAlarmEvent {
	st.CycleNo++
	st.ProcessActive, st.Unacknowledged = true, true
	st.OnPendingSince, st.OffPendingSince = nil, nil
	ver := a.Version
	st.ActiveVersion, st.ActivatedAt, st.ActivationValue, st.ReturnedAt = &ver, &ts, &v, nil
	return &store.NewAlarmEvent{
		AlarmID: a.AlarmID, Version: a.Version, CycleNo: st.CycleNo, Type: store.EventActivated,
		EventTS: ts, Value: &v, Limit: limitOf(a),
	}
}

func returnEvent(a store.EngineAlarm, st *store.AlarmStateRow, ts time.Time, note string) *store.NewAlarmEvent {
	st.ProcessActive = false
	st.OnPendingSince, st.OffPendingSince = nil, nil
	st.ReturnedAt = &ts
	var v *float64
	if a.Value != nil {
		x := *a.Value
		v = &x
	}
	return &store.NewAlarmEvent{
		AlarmID: a.AlarmID, Version: a.Version, CycleNo: st.CycleNo, Type: store.EventReturnedToNormal,
		EventTS: ts, Value: v, Limit: limitOf(a), Note: note,
	}
}

// finish decides whether the new state must be stored. The latest value and
// sample time alone do not count (that would write every alarm every second);
// they are saved together with the next real change.
func finish(out Outcome, old, st store.AlarmStateRow) Outcome {
	out.State = st
	out.Changed = out.Event != nil || out.DataEvent != "" || meaningfulChange(old, st)
	return out
}

func meaningfulChange(a, b store.AlarmStateRow) bool {
	return !a.Exists && (b.ProcessActive || b.OnPendingSince != nil || !b.DataValid || b.DelayFiltered > 0) ||
		a.ProcessActive != b.ProcessActive || a.Unacknowledged != b.Unacknowledged ||
		a.CycleNo != b.CycleNo || a.DataValid != b.DataValid || a.DelayFiltered != b.DelayFiltered ||
		!sameTime(a.OnPendingSince, b.OnPendingSince) || !sameTime(a.OffPendingSince, b.OffPendingSince)
}

func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// describe is used in log lines.
func describe(a store.EngineAlarm) string {
	return fmt.Sprintf("%s/%s %s", a.PLCName, a.PVName, a.Condition)
}
