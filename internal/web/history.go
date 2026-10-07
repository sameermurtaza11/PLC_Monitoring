package web

import (
	"errors"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"PLC_Monitoring/internal/store"
)

// Time ranges offered in the trend. Keys are what the <select> sends.
var ranges = map[string]time.Duration{
	"15m": 15 * time.Minute,
	"1h":  time.Hour,
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
}

var rangeOrder = []string{"15m", "1h", "6h", "24h", "7d"}

const (
	maxPointsPerSeries = 1000 // more than this → averaged buckets
	maxCompare         = 4    // selected PV + 4 others = 5 lines
)

// Series colours, fixed order: the selected PV is always slot 1 (blue),
// compared PVs take the next slots in the order they were added.
var seriesColors = []string{"#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4"}

// chartSeries is the JSON the ECharts trend is built from.
type chartSeries struct {
	ID      int          `json:"id"`
	Label   string       `json:"label"`
	Unit    string       `json:"unit"`
	Min     float64      `json:"min"` // pv_min / pv_max: used to plot "% of range"
	Max     float64      `json:"max"` // when compared PVs have different units
	Color   string       `json:"color"`
	Bucket  int          `json:"bucket"`
	Digital bool         `json:"digital"` // draw as a step line
	State0  string       `json:"state0"`
	State1  string       `json:"state1"`
	Points  [][2]float64 `json:"points"` // [unix ms, value]
}

// tableRow is one row of the table under the chart.
type tableRow struct {
	TS        time.Time
	Label     string
	Color     string
	Unit      string
	Digital   bool
	StateText string // digital: text of the value (raw samples only)
	store.HistoryPoint
}

// pvHistory returns the trend partial: chart JSON + table, from ONE query
// per PV, so the chart and the table always show the same data.
//
//	GET /pv/:id/history?range=1h&compare=9&compare=10
func (s *Server) pvHistory(c *gin.Context) {
	mainID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.String(http.StatusBadRequest, "invalid PV id")
		return
	}

	rangeKey := c.DefaultQuery("range", "1h")
	span, ok := ranges[rangeKey]
	if !ok {
		rangeKey, span = "1h", time.Hour
	}

	ids := []int{mainID}
	for _, v := range c.QueryArray("compare") {
		id, err := strconv.Atoi(v)
		if err != nil || slices.Contains(ids, id) || len(ids) > maxCompare {
			continue
		}
		ids = append(ids, id)
	}

	to := time.Now()
	from := to.Add(-span)

	var (
		chart    []chartSeries
		table    []tableRow
		bucketed bool
	)
	for i, id := range ids {
		series, err := s.store.History(c.Request.Context(), id, from, to, maxPointsPerSeries)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // compared PV was deleted meanwhile
		}
		if err != nil {
			log.Printf("web: history pv %d: %v", id, err)
			c.String(http.StatusInternalServerError, "database error")
			return
		}

		pv := series.PV
		cs := chartSeries{
			ID: pv.ID, Label: pv.PLCName + " / " + pv.Name, Unit: pv.Unit,
			Min: pv.Min, Max: pv.Max, Color: seriesColors[i%len(seriesColors)],
			Bucket: series.BucketSeconds, Points: make([][2]float64, len(series.Points)),
			Digital: pv.IsDigital(), State0: pv.State0Text, State1: pv.State1Text,
		}
		for j, p := range series.Points {
			cs.Points[j] = [2]float64{float64(p.TS.UnixMilli()), p.Value}
			table = append(table, tableRow{TS: p.TS, Label: cs.Label, Color: cs.Color, Unit: pv.Unit,
				Digital: cs.Digital, StateText: pv.StateText(p.Value), HistoryPoint: p})
		}
		chart = append(chart, cs)
		bucketed = bucketed || series.BucketSeconds > 0
	}

	// Newest first, like a log.
	sort.SliceStable(table, func(a, b int) bool { return table[a].TS.After(table[b].TS) })

	c.HTML(http.StatusOK, "pv_history.html", gin.H{
		"Chart":    chart,
		"Table":    table,
		"Bucketed": bucketed,
		"Range":    rangeKey,
		"From":     from,
		"To":       to,
	})
}
