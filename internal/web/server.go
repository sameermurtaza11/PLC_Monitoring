// Package web is the Gin HTTP server: pages, HTMX partials and static files.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"PLC_Monitoring/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	store          *store.Store
	onConfigChange func() // tells acquisition to reload PLC/PV config now
}

func NewRouter(s *store.Store, onConfigChange func()) *gin.Engine {
	srv := &Server{store: s, onConfigChange: onConfigChange}

	r := gin.New()
	r.Use(gin.Recovery(), requestLogger())

	tmpl := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html"))
	r.SetHTMLTemplate(tmpl)

	static, _ := fs.Sub(staticFS, "static")
	r.StaticFS("/static", http.FS(static))

	r.GET("/", srv.dashboard)
	r.GET("/partials/pv-rows", srv.pvRows)
	r.GET("/pv/:id", srv.pvDetail)
	r.GET("/pv/:id/live", srv.pvLive)
	r.GET("/pv/:id/history", srv.pvHistory)
	// Configuration (CRUD for plc_metadata / pv_metadata)
	r.GET("/config", srv.configPage)
	r.GET("/config/plcs", srv.plcList)
	r.GET("/config/plcs/new", srv.plcForm)
	r.GET("/config/plcs/:id/edit", srv.plcForm)
	r.POST("/config/plcs", srv.plcSave)
	r.POST("/config/plcs/:id", srv.plcSave)
	r.DELETE("/config/plcs/:id", srv.plcDelete)
	r.GET("/config/pvs", srv.pvList)
	r.GET("/config/pvs/new", srv.pvForm)
	r.GET("/config/pvs/:id/edit", srv.pvForm)
	r.POST("/config/pvs", srv.pvSave)
	r.POST("/config/pvs/:id", srv.pvSave)
	r.DELETE("/config/pvs/:id", srv.pvDelete)

	r.GET("/favicon.ico", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	r.GET("/healthz", func(c *gin.Context) { c.String(http.StatusOK, "ok") })

	return r
}

// ---------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------

func (s *Server) dashboard(c *gin.Context) {
	c.HTML(http.StatusOK, "dashboard.html", gin.H{"Page": "dashboard"})
}

// pvRows is polled by HTMX every 2 s and returns only <tr> rows.
func (s *Server) pvRows(c *gin.Context) {
	rows, err := s.store.LatestValues(c.Request.Context())
	if err != nil {
		log.Printf("web: latest values: %v", err)
		c.HTML(http.StatusOK, "pv_rows_error.html", err.Error())
		return
	}
	c.HTML(http.StatusOK, "pv_rows.html", rows)
}

func (s *Server) pvDetail(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.String(http.StatusBadRequest, "invalid PV id")
		return
	}
	// Opened directly (not by HTMX from the dashboard): show it inside the
	// styled dashboard instead of a bare HTML fragment.
	if c.GetHeader("HX-Request") == "" {
		c.Redirect(http.StatusFound, "/?pv="+strconv.Itoa(id))
		return
	}
	row, err := s.store.LatestValue(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "PV not found")
		return
	}
	if err != nil {
		log.Printf("web: pv %d: %v", id, err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	options, err := s.store.PVOptions(c.Request.Context())
	if err != nil {
		log.Printf("web: pv options: %v", err)
	}
	c.HTML(http.StatusOK, "pv_detail.html", gin.H{
		"PV":      row,
		"Options": options,
		"Ranges":  rangeOrder,
	})
}

// pvLive returns only the live-value block of the modal (polled every 2 s;
// the gauge reads its data-value after each swap).
func (s *Server) pvLive(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.String(http.StatusBadRequest, "invalid PV id")
		return
	}
	row, err := s.store.LatestValue(c.Request.Context(), id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "PV not found")
		return
	}
	if err != nil {
		log.Printf("web: pv %d live: %v", id, err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(http.StatusOK, "pv_live.html", row)
}

// ---------------------------------------------------------------------
// Template helpers
// ---------------------------------------------------------------------

// Status is what the "Status" column shows.
type Status struct {
	Text  string
	Class string // CSS class: ok | warn | bad | none
}

// statusOf decides a PV's status. Order matters: comm problems first,
// then stale data, then alarms.
func statusOf(r store.LatestRow) Status {
	if r.TS == nil || r.Quality == nil {
		return Status{"No data", "none"}
	}
	if *r.Quality != store.QualityGood {
		return Status{*r.Quality, "bad"}
	}
	// No update for 3 scan intervals (min 5 s) → the value can't be trusted.
	staleAfter := max(3*time.Duration(r.ScanIntervalMS)*time.Millisecond, 5*time.Second)
	if time.Since(*r.TS) > staleAfter {
		return Status{"STALE", "bad"}
	}
	if r.IsDigital() {
		if r.Value != nil && r.AlarmState != nil && int(*r.Value) == *r.AlarmState {
			return Status{"ALARM", "warn"}
		}
		return Status{"Normal", "ok"}
	}
	if r.Value != nil && r.AlarmHigh != nil && *r.Value >= *r.AlarmHigh {
		return Status{"HIGH", "warn"}
	}
	if r.Value != nil && r.AlarmLow != nil && *r.Value <= *r.AlarmLow {
		return Status{"LOW", "warn"}
	}
	return Status{"Normal", "ok"}
}

// fieldRef lets a template pass (form, field name) to the "field_error" block.
type fieldRef struct {
	Errors fieldErrors
	Name   string
}

var funcs = template.FuncMap{
	"fe":     func(f form, name string) fieldRef { return fieldRef{f.Errors, name} },
	"status": statusOf,
	// display: the value as an operator reads it — state text for digital
	// inputs, 2 decimals for analog values.
	"display": func(r store.LatestRow) string {
		if r.Value == nil {
			return "—"
		}
		if r.IsDigital() {
			return r.StateText(*r.Value)
		}
		return strconv.FormatFloat(*r.Value, 'f', 2, 64)
	},
	// kind: short type tag for tables.
	"kind": func(addr int) string {
		switch {
		case addr >= 10001 && addr <= 19999:
			return "DI"
		case addr >= 30001 && addr <= 39999:
			return "AI"
		default:
			return "HR"
		}
	},
	"deref":  func(v *float64) float64 { return *v },
	"deref2": func(v *int) int { return *v },
	"alarmStateText": func(pv store.PV) string {
		if pv.AlarmState == nil {
			return "—"
		}
		return "when " + pv.StateText(float64(*pv.AlarmState))
	},
	"num": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return strconv.FormatFloat(*v, 'f', 2, 64)
	},
	"int": func(v *int) string {
		if v == nil {
			return "—"
		}
		return strconv.Itoa(*v)
	},
	"f": func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) },
	"time": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		return t.Local().Format("2006-01-02 15:04:05.000")
	},
	"ts":   func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05.000") },
	"fmt2": func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) },
	"rawValue": func(v *float64) string { // for data-* attributes: "" when no value
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'f', -1, 64)
	},
	"orDash": func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	},
	"optNum": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return fmt.Sprint(*v)
	},
}

func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		// Skip the 2-second polling noise unless it fails.
		p := c.Request.URL.Path
		if c.Writer.Status() < 400 && (p == "/partials/pv-rows" || strings.HasSuffix(p, "/live") || strings.HasSuffix(p, "/history")) {
			return
		}
		log.Printf("web: %d %s %s (%v)", c.Writer.Status(), c.Request.Method, c.Request.URL.Path, time.Since(start).Round(time.Millisecond))
	}
}
