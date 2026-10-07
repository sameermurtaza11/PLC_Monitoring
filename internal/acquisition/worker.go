package acquisition

import (
	"context"
	"log"
	"math"
	"time"

	"PLC_Monitoring/internal/plc"
	"PLC_Monitoring/internal/store"
)

const (
	maxBackoff = 30 * time.Second
	// History samples kept in memory while PostgreSQL is unreachable.
	// ~40 bytes each → 200k ≈ 8 MB per PLC. Oldest are dropped beyond this.
	maxPending = 200_000
	// With a deadband, still store at least one history row per PV this often.
	historyHeartbeat = 60 * time.Second
)

type lastStored struct {
	value float64
	ts    time.Time
}

type worker struct {
	cfg    store.PLC
	store  *store.Store
	client *plc.Client
	blocks []plc.Block

	backoff  time.Duration
	nextDial time.Time

	pending    []store.Sample     // history not yet written (DB was down)
	lastStored map[int]lastStored // per pv_id, for deadband filtering
	commDown   bool               // to log connection loss once, not every scan
	lastDBLog  time.Time          // throttles "database write failed" messages
}

func newWorker(cfg store.PLC, s *store.Store) *worker {
	w := &worker{
		cfg:        cfg,
		store:      s,
		client:     plc.NewClient(cfg.IP, cfg.Port, cfg.UnitID, time.Duration(cfg.TimeoutMS)*time.Millisecond),
		backoff:    time.Second,
		lastStored: map[int]lastStored{},
	}

	addrs := make([]int, len(cfg.PVs))
	for i, pv := range cfg.PVs {
		addrs[i] = pv.RegisterAddress
	}
	blocks, invalid := plc.BuildBlocks(addrs)
	for i, err := range invalid {
		log.Printf("acquisition: [%s] PV %s skipped: %v", cfg.Name, cfg.PVs[i].Name, err)
	}
	w.blocks = blocks
	return w
}

func (w *worker) logf(format string, args ...any) {
	log.Printf("acquisition: ["+w.cfg.Name+"] "+format, args...)
}

func (w *worker) run(ctx context.Context) {
	defer w.client.Close()

	if len(w.cfg.PVs) == 0 {
		w.logf("no enabled PVs configured, idle")
		<-ctx.Done()
		return
	}

	interval := time.Duration(w.cfg.ScanIntervalMS) * time.Millisecond
	w.logf("started: %s:%d unit %d, %d PVs in %d read(s), every %v",
		w.cfg.IP, w.cfg.Port, w.cfg.UnitID, len(w.cfg.PVs), len(w.blocks), interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		w.scan(ctx)
		select {
		case <-ctx.Done():
			w.flushOnStop()
			return
		case <-ticker.C:
		}
	}
}

// scan performs one read of all blocks and writes the result.
func (w *worker) scan(ctx context.Context) {
	var (
		latest   []store.Sample
		history  []store.Sample
		failures []store.Failure
	)

	if !w.ensureConnected() {
		// Still unreachable: mark every PV as COMM_FAIL (dashboard shows it)
		// and try to flush any buffered history.
		now := time.Now().UTC()
		for _, pv := range w.cfg.PVs {
			failures = append(failures, store.Failure{PVID: pv.ID, TS: now, Quality: store.QualityCommFail, Error: "PLC not reachable"})
		}
		w.write(ctx, nil, nil, failures)
		return
	}

	for bi, b := range w.blocks {
		values, err := w.client.ReadBlock(b)
		ts := time.Now().UTC() // timestamp taken when the PLC answered

		if err != nil {
			quality := store.QualityReadFail
			if !plc.IsModbusException(err) {
				// Network-level failure: drop the socket and reconnect next scan.
				quality = store.QualityCommFail
				w.client.Close()
				w.logf("connection lost: %v", err)
				w.commDown = true
			} else {
				w.logf("read %s %d+%d failed: %v", b.Kind, b.Start, b.Count, err)
			}
			for _, rest := range w.blocks[bi:] { // this block and (if comm lost) the rest
				for _, idx := range rest.Items {
					failures = append(failures, store.Failure{PVID: w.cfg.PVs[idx].ID, TS: ts, Quality: quality, Error: err.Error()})
				}
				if quality == store.QualityReadFail {
					break // only this block failed; continue with the next
				}
			}
			if quality == store.QualityCommFail {
				break
			}
			continue
		}

		for _, idx := range b.Items {
			pv := w.cfg.PVs[idx]
			_, off, _ := plc.ParseAddress(pv.RegisterAddress)
			raw := values[off-b.Start]
			value := float64(raw) // digital input: 0/1, no scaling
			if !pv.IsDigital() {
				value = Scale(raw, pv.Min, pv.Max)
			}
			s := store.Sample{PVID: pv.ID, TS: ts, Raw: raw, Value: value}
			latest = append(latest, s)
			if w.keepHistory(pv, s) {
				history = append(history, s)
			}
		}
	}

	w.write(ctx, latest, history, failures)
}

// ensureConnected returns true if the client is connected, dialling with
// exponential backoff (1s, 2s, 4s … 30s) while the PLC is unreachable.
func (w *worker) ensureConnected() bool {
	if w.client.Connected() {
		return true
	}
	if time.Now().Before(w.nextDial) {
		return false
	}
	if err := w.client.Connect(); err != nil {
		if !w.commDown {
			w.logf("cannot connect to %s:%d: %v", w.cfg.IP, w.cfg.Port, err)
		}
		w.commDown = true
		w.nextDial = time.Now().Add(w.backoff)
		w.backoff = min(w.backoff*2, maxBackoff)
		return false
	}
	if w.commDown {
		w.logf("connection restored")
	} else {
		w.logf("connected")
	}
	w.commDown = false
	w.backoff = time.Second
	return true
}

// keepHistory decides whether a sample goes into pv_history.
//   - Digital input: only when the state changes (+ heartbeat). Nothing is
//     lost: every transition is stored with the timestamp of the scan that saw it.
//   - Analog, deadband = 0: every scan.
//   - Analog, deadband > 0: only changes >= deadband (+ heartbeat).
//
// Heartbeat = one row per minute even without change, so a flat line
// is visibly "still alive" in the trend.
func (w *worker) keepHistory(pv store.PV, s store.Sample) bool {
	last, ok := w.lastStored[pv.ID]
	if pv.IsDigital() && ok && s.Value == last.value && s.TS.Sub(last.ts) < historyHeartbeat {
		return false
	}
	if !pv.IsDigital() && pv.Deadband > 0 && ok &&
		math.Abs(s.Value-last.value) < pv.Deadband &&
		s.TS.Sub(last.ts) < historyHeartbeat {
		return false
	}
	w.lastStored[pv.ID] = lastStored{s.Value, s.TS}
	return true
}

// write stores one scan. If PostgreSQL fails, history is buffered in memory
// and retried with the next scan, so a short DB outage loses no samples.
func (w *worker) write(ctx context.Context, latest, history []store.Sample, failures []store.Failure) {
	all := append(w.pending, history...)

	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := w.store.WriteScan(dbCtx, latest, all, failures); err != nil {
		if ctx.Err() != nil {
			w.pending = all // shutting down; flushOnStop will retry
			return
		}
		if store.IsPermanent(err) {
			// Retrying would fail forever and block all later writes.
			w.logf("database rejected %d samples, dropped: %v", len(all), err)
			w.pending = nil
			return
		}
		if dropped := len(all) - maxPending; dropped > 0 {
			all = all[dropped:]
			w.logf("buffer full: dropped %d oldest history samples", dropped)
		}
		w.pending = all
		if time.Since(w.lastDBLog) >= 30*time.Second {
			w.logf("database write failed (%d samples buffered): %v", len(w.pending), err)
			w.lastDBLog = time.Now()
		}
		return
	}
	w.lastDBLog = time.Time{}
	if len(w.pending) > 0 {
		w.logf("database recovered: %d buffered samples written", len(w.pending))
	}
	w.pending = nil
}

func (w *worker) flushOnStop() {
	if len(w.pending) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.store.WriteScan(ctx, nil, w.pending, nil); err != nil {
		w.logf("shutdown: %d buffered samples lost: %v", len(w.pending), err)
	}
}
