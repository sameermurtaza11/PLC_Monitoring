package store

import "time"

// PLC is one row of plc_metadata, plus the enabled PVs that belong to it.
type PLC struct {
	ID             int
	Name           string
	IP             string
	Port           int
	UnitID         int
	ScanIntervalMS int
	TimeoutMS      int
	PVs            []PV
}

// PV is one row of pv_metadata.
type PV struct {
	ID              int
	PLCID           int
	RegisterAddress int
	Name            string
	SystemName      string
	Equipment       string
	Description     string
	Unit            string
	Min             float64
	Max             float64
	Priority        int
	AlarmLow        *float64
	AlarmHigh       *float64
	Deadband        float64
	State0Text      string // digital only: text for 0, e.g. "STOPPED"
	State1Text      string // digital only: text for 1, e.g. "RUNNING"
	AlarmState      *int   // digital only: nil = no alarm, 0/1 = alarm in that state
}

// IsDigital: 10001-19999 = Modbus discrete input, 1 bit, value 0/1.
func (pv PV) IsDigital() bool { return pv.RegisterAddress >= 10001 && pv.RegisterAddress <= 19999 }

// StateText returns the configured text for a digital value.
func (pv PV) StateText(v float64) string {
	if v >= 0.5 {
		return pv.State1Text
	}
	return pv.State0Text
}

// Sample is one successful reading of one PV.
type Sample struct {
	PVID  int
	TS    time.Time
	Raw   uint16
	Value float64
}

// Failure records that a PV could not be read in a scan.
type Failure struct {
	PVID    int
	TS      time.Time
	Quality string // COMM_FAIL | READ_FAIL
	Error   string
}

// Quality codes stored in pv_latest.quality.
const (
	QualityGood     = "GOOD"
	QualityCommFail = "COMM_FAIL" // PLC unreachable / connection dropped
	QualityReadFail = "READ_FAIL" // PLC answered with a Modbus exception
)

// LatestRow is what the dashboard shows for one PV.
type LatestRow struct {
	PV
	PLCName        string
	ScanIntervalMS int
	Raw            *int
	Value          *float64
	Quality        *string
	Error          string
	TS             *time.Time
	LastGoodTS     *time.Time
}

// PVOption is one entry in the "compare with" drop-down.
type PVOption struct {
	ID      int
	PLCName string
	Name    string
	Unit    string
}

// HistoryPoint is one point of a trend. When the range holds more samples
// than the chart can use, points are time buckets: Value = average,
// Min/Max = extremes inside the bucket, Count = samples in it.
type HistoryPoint struct {
	TS    time.Time
	Value float64
	Min   float64
	Max   float64
	Count int
}

// Series is the history of one PV over a time range.
type Series struct {
	PV            LatestRow
	BucketSeconds int // 0 = raw samples, >0 = averaged buckets
	Points        []HistoryPoint
}
