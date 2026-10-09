package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"

	"PLC_Monitoring/internal/ams"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// registerAMSConfig adds the alarm configuration pages (AMS step 2).
// Who may open or use each route is decided by the access guard (access.Rules),
// on the server, for every request; CSRF is checked here.
func (s *Server) registerAMSConfig(r *gin.Engine) {
	csrf := auth.RequireCSRF()

	r.GET("/ams/config", s.alarmList)
	r.GET("/ams/config/new", s.alarmNewForm)
	r.POST("/ams/config", csrf, s.alarmCreate)
	r.GET("/ams/config/:id", s.alarmDetail)
	r.POST("/ams/config/:id", csrf, s.alarmNewVersion)
	r.POST("/ams/config/:id/review", csrf, s.alarmReview)
}

var flashMessages = map[string]string{
	"created":  "Alarm saved. It is waiting for approval and is not active until approved.",
	"saved":    "New version saved. It is waiting for approval; the approved version stays in force until then.",
	"approved": "Version approved. It is now the version in force.",
	"rejected": "Version rejected. The previous version stays in force.",
}

func (s *Server) amsPage(c *gin.Context, extra gin.H) gin.H {
	h := s.pageData(c, "ams-config", gin.H{"Msg": flashMessages[c.Query("ok")]})
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func (s *Server) alarmList(c *gin.Context) {
	alarms, err := s.store.ListAlarms(c.Request.Context())
	if err != nil {
		log.Printf("web: list alarms: %v", err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	pending := 0
	for _, a := range alarms {
		if a.PendingVersion != nil {
			pending++
		}
	}
	c.HTML(http.StatusOK, "ams_config.html", s.amsPage(c, gin.H{"Alarms": alarms, "PendingCount": pending}))
}

func (s *Server) alarmNewForm(c *gin.Context) {
	s.renderAlarmForm(c, http.StatusOK, map[string]string{
		"enabled": "1", "shelving_allowed": "1", "on_delay_s": "0", "off_delay_s": "0", "deadband": "0",
	}, nil)
}

func (s *Server) alarmCreate(c *gin.Context) {
	in, form, errs := parseAlarmForm(c)
	if pv, err := strconv.Atoi(c.PostForm("pv_id")); err == nil {
		in.PVID = pv
	} else {
		errs["pv_id"] = "choose a PV"
	}
	in.Condition = c.PostForm("condition")
	if len(errs) > 0 {
		s.renderAlarmForm(c, http.StatusUnprocessableEntity, form, errs)
		return
	}
	alarmID, _, err := s.ams.Submit(c.Request.Context(), auth.FromContext(c), c.ClientIP(), in)
	if err != nil {
		s.submitFailed(c, err, 0, form)
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/ams/config/%d?ok=created", alarmID))
}

func (s *Server) alarmDetail(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.String(http.StatusBadRequest, "invalid alarm id")
		return
	}
	s.renderAlarmDetail(c, http.StatusOK, id, nil, nil)
}

func (s *Server) alarmNewVersion(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.String(http.StatusBadRequest, "invalid alarm id")
		return
	}
	in, form, errs := parseAlarmForm(c)
	if len(errs) > 0 {
		s.renderAlarmDetail(c, http.StatusUnprocessableEntity, id, form, errs)
		return
	}
	in.AlarmID = id
	if _, _, err := s.ams.Submit(c.Request.Context(), auth.FromContext(c), c.ClientIP(), in); err != nil {
		s.submitFailed(c, err, id, form)
		return
	}
	c.Redirect(http.StatusSeeOther, fmt.Sprintf("/ams/config/%d?ok=saved", id))
}

func (s *Server) alarmReview(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	version, verr := strconv.Atoi(c.PostForm("version"))
	if err != nil || verr != nil {
		c.String(http.StatusBadRequest, "invalid alarm or version")
		return
	}
	approve := c.PostForm("decision") == "approve"
	err = s.ams.Review(c.Request.Context(), auth.FromContext(c), c.ClientIP(), id, version, approve, c.PostForm("comment"))
	var ve *ams.ValidationError
	switch {
	case err == nil:
		ok := "rejected"
		if approve {
			ok = "approved"
		}
		c.Redirect(http.StatusSeeOther, fmt.Sprintf("/ams/config/%d?ok=%s", id, ok))
	case errors.As(err, &ve):
		s.renderAlarmDetail(c, http.StatusUnprocessableEntity, id, nil, ve.Fields)
	case errors.Is(err, store.ErrSelfApproval):
		s.renderAlarmDetail(c, http.StatusForbidden, id, nil, map[string]string{"review": "You submitted this version, so someone else must approve it."})
	case errors.Is(err, store.ErrNotPending):
		s.renderAlarmDetail(c, http.StatusConflict, id, nil, map[string]string{"review": "This version is no longer waiting for approval (someone else may have reviewed it)."})
	case errors.Is(err, pgx.ErrNoRows):
		c.String(http.StatusNotFound, "alarm or version not found")
	case errors.Is(err, ams.ErrForbidden):
		c.String(http.StatusForbidden, "not permitted")
	default:
		log.Printf("web: review alarm %d v%d: %v", id, version, err)
		c.String(http.StatusInternalServerError, "database error")
	}
}

// submitFailed shows the right page for a failed submit. alarmID 0 = the
// "new alarm" form, otherwise the detail page of that alarm.
func (s *Server) submitFailed(c *gin.Context, err error, alarmID int, form map[string]string) {
	var ve *ams.ValidationError
	redisplay := func(status int, errs map[string]string) {
		if alarmID != 0 {
			s.renderAlarmDetail(c, status, alarmID, form, errs)
		} else {
			s.renderAlarmForm(c, status, form, errs)
		}
	}
	switch {
	case errors.As(err, &ve):
		redisplay(http.StatusUnprocessableEntity, ve.Fields)
	case errors.Is(err, store.ErrAlarmExists):
		redisplay(http.StatusConflict, map[string]string{"condition": "this PV already has an alarm with this condition — edit that one"})
	case errors.Is(err, store.ErrPendingExists):
		redisplay(http.StatusConflict, map[string]string{"form": "A version is already waiting for approval. Approve or reject it first."})
	case errors.Is(err, ams.ErrForbidden):
		c.String(http.StatusForbidden, "not permitted")
	case errors.Is(err, ams.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		c.String(http.StatusNotFound, "alarm not found")
	default:
		log.Printf("web: submit alarm: %v", err)
		c.String(http.StatusInternalServerError, "database error")
	}
}

func (s *Server) renderAlarmForm(c *gin.Context, status int, form, errs map[string]string) {
	ctx := c.Request.Context()
	pvs, err1 := s.store.ListAlarmPVs(ctx)
	prios, err2 := s.store.ListAlarmPriorities(ctx)
	classes, err3 := s.store.ListAlarmClasses(ctx)
	if err := errors.Join(err1, err2, err3); err != nil {
		log.Printf("web: alarm form: %v", err)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	c.HTML(status, "ams_alarm.html", s.amsPage(c, gin.H{
		"New": true, "PVs": pvs, "Priorities": prios, "Classes": classes, "F": form, "Errors": errs,
		"Conditions": []string{ams.CondHIHI, ams.CondHI, ams.CondLO, ams.CondLOLO, ams.CondDiscrete},
	}))
}

func (s *Server) renderAlarmDetail(c *gin.Context, status, id int, form, errs map[string]string) {
	ctx := c.Request.Context()
	alarm, err := s.store.GetAlarm(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		c.String(http.StatusNotFound, "alarm not found")
		return
	}
	versions, err2 := s.store.ListAlarmVersions(ctx, id)
	prios, err3 := s.store.ListAlarmPriorities(ctx)
	classes, err4 := s.store.ListAlarmClasses(ctx)
	if e := errors.Join(err, err2, err3, err4); e != nil {
		log.Printf("web: alarm %d detail: %v", id, e)
		c.String(http.StatusInternalServerError, "database error")
		return
	}
	var pending, shown *store.AlarmVersion
	for i := range versions {
		v := &versions[i]
		if v.Status == "pending" {
			pending = v
		}
		if shown == nil && (v.Version == alarm.ShownVersion) {
			shown = v
		}
	}
	if form == nil && shown != nil {
		form = formFromVersion(*shown) // prefill the edit form with the version in force
	}
	c.HTML(status, "ams_alarm.html", s.amsPage(c, gin.H{
		"Alarm": alarm, "Versions": versions, "Pending": pending, "Shown": shown,
		"Priorities": prios, "Classes": classes, "F": form, "Errors": errs,
		"CanEdit":    auth.FromContext(c).Can(ams.PermConfig),
		"CanApprove": auth.FromContext(c).Can(ams.PermApprove),
	}))
}

// parseAlarmForm reads the posted fields. It returns the input, the raw
// values (to show the form again on error) and parse errors; rule checks
// (ranges, ordering) happen in ams.Validate.
func parseAlarmForm(c *gin.Context) (store.AlarmInput, map[string]string, map[string]string) {
	form := map[string]string{}
	errs := map[string]string{}
	for _, k := range []string{
		"pv_id", "condition", "alarm_tag", "location", "setpoint", "trigger_state", "deadband",
		"on_delay_s", "off_delay_s", "priority_id", "class_id", "message", "description", "cause",
		"consequence", "operator_action", "owner", "response_time_s", "change_reason",
		"shelving_allowed", "suppression_allowed", "enabled",
	} {
		form[k] = strings.TrimSpace(c.PostForm(k))
	}

	in := store.AlarmInput{
		Tag: form["alarm_tag"], Location: form["location"], Message: form["message"],
		Description: form["description"], Cause: form["cause"], Consequence: form["consequence"],
		OperatorAction: form["operator_action"], Owner: form["owner"], ChangeReason: form["change_reason"],
		ShelvingAllowed: form["shelving_allowed"] != "", SuppressionAllowed: form["suppression_allowed"] != "",
		Enabled: form["enabled"] != "",
	}
	in.Setpoint = optFloat(form, "setpoint", errs)
	in.TriggerState = optInt(form, "trigger_state", errs)
	in.ResponseTimeS = optInt(form, "response_time_s", errs)
	in.Deadband = reqFloat(form, "deadband", errs)
	in.OnDelayS = reqInt(form, "on_delay_s", errs)
	in.OffDelayS = reqInt(form, "off_delay_s", errs)
	in.PriorityID = reqInt(form, "priority_id", errs)
	in.ClassID = reqInt(form, "class_id", errs)
	return in, form, errs
}

func optFloat(form map[string]string, k string, errs map[string]string) *float64 {
	if form[k] == "" {
		return nil
	}
	v, err := strconv.ParseFloat(form[k], 64)
	if err != nil {
		errs[k] = "must be a number"
		return nil
	}
	return &v
}

func reqFloat(form map[string]string, k string, errs map[string]string) float64 {
	if form[k] == "" {
		return 0
	}
	if v := optFloat(form, k, errs); v != nil {
		return *v
	}
	return 0
}

func optInt(form map[string]string, k string, errs map[string]string) *int {
	if form[k] == "" {
		return nil
	}
	v, err := strconv.Atoi(form[k])
	if err != nil {
		errs[k] = "must be a whole number"
		return nil
	}
	return &v
}

func reqInt(form map[string]string, k string, errs map[string]string) int {
	if form[k] == "" {
		return 0
	}
	if v := optInt(form, k, errs); v != nil {
		return *v
	}
	return 0
}

// formFromVersion turns a stored version into the raw form values.
func formFromVersion(v store.AlarmVersion) map[string]string {
	f := map[string]string{
		"alarm_tag": v.Tag, "location": v.Location,
		"deadband":   strconv.FormatFloat(v.Deadband, 'f', -1, 64),
		"on_delay_s": strconv.Itoa(v.OnDelayS), "off_delay_s": strconv.Itoa(v.OffDelayS),
		"priority_id": strconv.Itoa(v.PriorityID), "class_id": strconv.Itoa(v.ClassID),
		"message": v.Message, "description": v.Description, "cause": v.Cause,
		"consequence": v.Consequence, "operator_action": v.OperatorAction, "owner": v.Owner,
	}
	if v.Setpoint != nil {
		f["setpoint"] = strconv.FormatFloat(*v.Setpoint, 'f', -1, 64)
	}
	if v.TriggerState != nil {
		f["trigger_state"] = strconv.Itoa(*v.TriggerState)
	}
	if v.ResponseTimeS != nil {
		f["response_time_s"] = strconv.Itoa(*v.ResponseTimeS)
	}
	for k, on := range map[string]bool{
		"shelving_allowed": v.ShelvingAllowed, "suppression_allowed": v.SuppressionAllowed, "enabled": v.Enabled,
	} {
		if on {
			f[k] = "1"
		}
	}
	return f
}
