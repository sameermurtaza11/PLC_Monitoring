package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"PLC_Monitoring/internal/plc"
	"PLC_Monitoring/internal/store"
)

// Flow of every form (HTMX):
//
//	GET  …/new or …/:id/edit → form HTML swapped into the form dialog
//	POST …                   → invalid: same form + field errors (200)
//	                           valid:   204 + HX-Trigger "configSaved"
//	                                    (dialog closes, lists reload)
//	DELETE …                 → message HTML into #config-msg
//
// Validation happens twice on purpose: here (friendly per-field messages)
// and in PostgreSQL constraints (the guarantee).

type fieldErrors map[string]string

// form keeps what the user typed, so an invalid form is re-shown unchanged.
type form struct {
	ID     int               // 0 = new
	Values map[string]string // field name → typed value
	Errors fieldErrors
	PLCs   []store.PLCRecord // for the PV form's PLC select
}

func (f form) V(name string) string     { return f.Values[name] }
func (f form) Checked(name string) bool { return f.Values[name] == "on" }

func (s *Server) configPage(c *gin.Context) {
	plcs, err := s.store.ListPLCs(c.Request.Context())
	if err != nil {
		log.Printf("web: list plcs: %v", err)
	}
	c.HTML(http.StatusOK, "config.html", s.pageData(c, "config", gin.H{"PLCs": plcs}))
}

// saved closes the dialog, refreshes the lists and reloads acquisition.
func (s *Server) saved(c *gin.Context, what string) {
	s.onConfigChange()
	payload, _ := json.Marshal(map[string]string{"configSaved": what})
	c.Header("HX-Trigger", string(payload))
	c.Status(http.StatusNoContent)
}

func (s *Server) message(c *gin.Context, class, text string) {
	c.HTML(http.StatusOK, "config_msg.html", gin.H{"Class": class, "Text": text})
}

// ---------------------------------------------------------------------
// PLC
// ---------------------------------------------------------------------

func (s *Server) plcList(c *gin.Context) {
	plcs, err := s.store.ListPLCs(c.Request.Context())
	if err != nil {
		log.Printf("web: list plcs: %v", err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(http.StatusOK, "plc_rows.html", plcs)
}

func (s *Server) plcForm(c *gin.Context) {
	f := form{Values: map[string]string{
		"port": "502", "unit_id": "1", "scan_interval_ms": "1000", "timeout_ms": "3000", "enabled": "on",
	}}
	if idStr := c.Param("id"); idStr != "" {
		id, _ := strconv.Atoi(idStr)
		p, err := s.store.GetPLC(c.Request.Context(), id)
		if err != nil {
			c.String(http.StatusNotFound, "PLC not found")
			return
		}
		f.ID = p.ID
		f.Values = map[string]string{
			"plc_name": p.Name, "ip_address": p.IP, "port": strconv.Itoa(p.Port),
			"unit_id": strconv.Itoa(p.UnitID), "scan_interval_ms": strconv.Itoa(p.ScanIntervalMS),
			"timeout_ms": strconv.Itoa(p.TimeoutMS), "description": p.Description,
			"enabled": onOff(p.Enabled),
		}
	}
	c.HTML(http.StatusOK, "plc_form.html", f)
}

func (s *Server) plcSave(c *gin.Context) {
	f := readForm(c, "plc_name", "ip_address", "port", "unit_id", "scan_interval_ms", "timeout_ms", "description", "enabled")
	f.ID, _ = strconv.Atoi(c.Param("id"))

	p := store.PLCRecord{ID: f.ID, Enabled: f.Checked("enabled"), Description: f.V("description")}
	p.Name = requiredText(f, "plc_name", 64)
	p.IP = f.V("ip_address")
	if ip := net.ParseIP(p.IP); ip == nil || ip.To4() == nil {
		f.Errors["ip_address"] = "Enter a valid IPv4 address, e.g. 192.168.1.10"
	}
	p.Port = intField(f, "port", 1, 65535)
	p.UnitID = intField(f, "unit_id", 0, 255)
	p.ScanIntervalMS = intField(f, "scan_interval_ms", 100, 3_600_000)
	p.TimeoutMS = intField(f, "timeout_ms", 100, 60_000)

	if len(f.Errors) == 0 {
		_, err := s.store.SavePLC(c.Request.Context(), p)
		if err == nil {
			s.saved(c, "PLC "+p.Name+" saved")
			return
		}
		if !mapConstraint(err, f.Errors) {
			log.Printf("web: save plc: %v", err)
			f.Errors["_"] = "Could not save: " + err.Error()
		}
	}
	c.HTML(http.StatusOK, "plc_form.html", f)
}

func (s *Server) plcDelete(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	err := s.store.DeletePLC(c.Request.Context(), id)
	switch {
	case errors.Is(err, store.ErrInUse):
		s.message(c, "error", "This PLC still has PVs. Delete or move its PVs first, or just disable the PLC.")
	case errors.Is(err, pgx.ErrNoRows):
		s.message(c, "error", "PLC not found (already deleted?).")
	case err != nil:
		log.Printf("web: delete plc %d: %v", id, err)
		s.message(c, "error", "Delete failed: "+err.Error())
	default:
		s.onConfigChange()
		c.Header("HX-Trigger", "configSaved")
		s.message(c, "ok", "PLC deleted.")
	}
}

// ---------------------------------------------------------------------
// PV
// ---------------------------------------------------------------------

func (s *Server) pvList(c *gin.Context) {
	plcID, _ := strconv.Atoi(c.Query("plc"))
	pvs, err := s.store.ListPVs(c.Request.Context(), plcID)
	if err != nil {
		log.Printf("web: list pvs: %v", err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(http.StatusOK, "pv_config_rows.html", pvs)
}

func (s *Server) pvForm(c *gin.Context) {
	f := form{Values: map[string]string{
		"pv_min": "0", "pv_max": "100", "priority": "3", "deadband": "0", "enabled": "on",
		"state0_text": "OFF", "state1_text": "ON",
		"plc_id": c.Query("plc"),
	}}
	if idStr := c.Param("id"); idStr != "" {
		id, _ := strconv.Atoi(idStr)
		r, err := s.store.GetPV(c.Request.Context(), id)
		if err != nil {
			c.String(http.StatusNotFound, "PV not found")
			return
		}
		f.ID = r.ID
		f.Values = map[string]string{
			"plc_id": strconv.Itoa(r.PLCID), "register_address": strconv.Itoa(r.RegisterAddress),
			"pv_name": r.Name, "system_name": r.SystemName, "equipment": r.Equipment,
			"description": r.Description, "unit": r.Unit,
			"pv_min": ftoa(r.Min), "pv_max": ftoa(r.Max), "priority": strconv.Itoa(r.Priority),
			"alarm_low": optFtoa(r.AlarmLow), "alarm_high": optFtoa(r.AlarmHigh),
			"deadband": ftoa(r.Deadband), "enabled": onOff(r.Enabled),
			"state0_text": r.State0Text, "state1_text": r.State1Text, "alarm_state": optItoa(r.AlarmState),
		}
	}
	s.renderPVForm(c, f)
}

func (s *Server) renderPVForm(c *gin.Context, f form) {
	plcs, err := s.store.ListPLCs(c.Request.Context())
	if err != nil {
		log.Printf("web: list plcs: %v", err)
	}
	f.PLCs = plcs
	c.HTML(http.StatusOK, "pv_form.html", f)
}

func (s *Server) pvSave(c *gin.Context) {
	f := readForm(c, "plc_id", "register_address", "pv_name", "system_name", "equipment",
		"description", "unit", "pv_min", "pv_max", "priority", "alarm_low", "alarm_high", "deadband", "enabled",
		"state0_text", "state1_text", "alarm_state")
	f.ID, _ = strconv.Atoi(c.Param("id"))

	r := store.PVRecord{Enabled: f.Checked("enabled")}
	r.ID = f.ID
	r.PLCID = intField(f, "plc_id", 1, 1<<31-1)
	if _, ok := f.Errors["plc_id"]; ok {
		f.Errors["plc_id"] = "Select a PLC"
	}
	r.RegisterAddress = intField(f, "register_address", 0, 1<<31-1)
	if _, ok := f.Errors["register_address"]; !ok {
		if _, _, err := plc.ParseAddress(r.RegisterAddress); err != nil {
			f.Errors["register_address"] = "Use 10001–19999 (digital input), 30001–39999 (input register) or 40001–49999 (holding register)"
		}
	}
	r.Name = requiredText(f, "pv_name", 64)
	r.SystemName, r.Equipment, r.Description, r.Unit =
		f.V("system_name"), f.V("equipment"), f.V("description"), f.V("unit")
	r.Priority = intField(f, "priority", 1, 5)
	if r.IsDigital() {
		// Digital input: value is 0/1. Scaling is fixed to 0…1 so trends and
		// "% of range" comparisons work; analog-only fields are cleared.
		r.Min, r.Max, r.Deadband, r.Unit = 0, 1, 0, ""
		r.State0Text = requiredText(f, "state0_text", 32)
		r.State1Text = requiredText(f, "state1_text", 32)
		switch f.V("alarm_state") {
		case "":
		case "0", "1":
			v, _ := strconv.Atoi(f.V("alarm_state"))
			r.AlarmState = &v
		default:
			f.Errors["alarm_state"] = "Choose no alarm, 0 or 1"
		}
	} else {
		r.State0Text, r.State1Text = "OFF", "ON"
		r.Min = floatField(f, "pv_min")
		r.Max = floatField(f, "pv_max")
		if f.Errors["pv_min"] == "" && f.Errors["pv_max"] == "" && r.Max <= r.Min {
			f.Errors["pv_max"] = "PV max must be greater than PV min"
		}
		r.AlarmLow = optFloatField(f, "alarm_low")
		r.AlarmHigh = optFloatField(f, "alarm_high")
		if r.AlarmLow != nil && r.AlarmHigh != nil && *r.AlarmLow >= *r.AlarmHigh {
			f.Errors["alarm_high"] = "Alarm high must be greater than alarm low"
		}
		r.Deadband = floatField(f, "deadband")
		if f.Errors["deadband"] == "" && r.Deadband < 0 {
			f.Errors["deadband"] = "Deadband cannot be negative"
		}
	}

	if len(f.Errors) == 0 {
		_, err := s.store.SavePV(c.Request.Context(), r)
		if err == nil {
			s.saved(c, "PV "+r.Name+" saved")
			return
		}
		if !mapConstraint(err, f.Errors) {
			log.Printf("web: save pv: %v", err)
			f.Errors["_"] = "Could not save: " + err.Error()
		}
	}
	s.renderPVForm(c, f)
}

func (s *Server) pvDelete(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	withHistory := c.Query("with_history") == "1"

	err := s.store.DeletePV(c.Request.Context(), id, withHistory)
	switch {
	case errors.Is(err, store.ErrHasAlarms):
		s.message(c, "error", "This PV has alarm definitions, which are kept for alarm history. Disable the PV instead of deleting it.")
	case errors.Is(err, store.ErrInUse):
		n, _ := s.store.HistoryCount(c.Request.Context(), id)
		c.HTML(http.StatusOK, "pv_delete_blocked.html", gin.H{"ID": id, "Count": n})
	case errors.Is(err, pgx.ErrNoRows):
		s.message(c, "error", "PV not found (already deleted?).")
	case err != nil:
		log.Printf("web: delete pv %d: %v", id, err)
		s.message(c, "error", "Delete failed: "+err.Error())
	default:
		s.onConfigChange()
		c.Header("HX-Trigger", "configSaved")
		s.message(c, "ok", "PV deleted.")
	}
}

// ---------------------------------------------------------------------
// Form helpers
// ---------------------------------------------------------------------

func readForm(c *gin.Context, names ...string) form {
	f := form{Values: map[string]string{}, Errors: fieldErrors{}}
	for _, n := range names {
		f.Values[n] = strings.TrimSpace(c.PostForm(n))
	}
	return f
}

func requiredText(f form, name string, max int) string {
	v := f.V(name)
	switch {
	case v == "":
		f.Errors[name] = "Required"
	case len(v) > max:
		f.Errors[name] = fmt.Sprintf("Maximum %d characters", max)
	}
	return v
}

func intField(f form, name string, min, max int) int {
	n, err := strconv.Atoi(f.V(name))
	if err != nil {
		f.Errors[name] = "Enter a whole number"
		return 0
	}
	if n < min || n > max {
		f.Errors[name] = fmt.Sprintf("Must be between %d and %d", min, max)
	}
	return n
}

func floatField(f form, name string) float64 {
	v, err := strconv.ParseFloat(f.V(name), 64)
	if err != nil {
		f.Errors[name] = "Enter a number"
	}
	return v
}

// optFloatField: empty = not configured (NULL in the database).
func optFloatField(f form, name string) *float64 {
	if f.V(name) == "" {
		return nil
	}
	v := floatField(f, name)
	return &v
}

// mapConstraint puts a PostgreSQL constraint violation next to its field.
func mapConstraint(err error, errs fieldErrors) bool {
	var ce *store.ConstraintError
	if !errors.As(err, &ce) {
		return false
	}
	switch ce.Constraint {
	case "plc_metadata_plc_name_key":
		errs["plc_name"] = "A PLC with this name already exists"
	case "pv_unique_register":
		errs["register_address"] = "This register is already used by another PV on this PLC"
	case "pv_unique_name":
		errs["pv_name"] = "This PV name already exists on this PLC"
	case "pv_metadata_plc_id_fkey":
		errs["plc_id"] = "Selected PLC no longer exists"
	default:
		errs["_"] = "Rejected by database: " + ce.Message
	}
	return true
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return ""
}

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func optItoa(v *int) string {
	if v == nil {
		return ""
	}
	return strconv.Itoa(*v)
}

func optFtoa(v *float64) string {
	if v == nil {
		return ""
	}
	return ftoa(*v)
}
