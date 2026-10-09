package web

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"PLC_Monitoring/internal/ams"
	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// registerAMSAlarms adds the operator API (AMS step 4): the current alarm
// states and acknowledgement. The AMS page (step 5) is built on these.
// Permissions are checked on the server for every request.
func (s *Server) registerAMSAlarms(r *gin.Engine) {
	// The token a page must send back with state-changing requests. Another
	// site cannot read this response (same-origin policy), so it cannot forge it.
	r.GET("/ams/api/csrf", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"csrf_token": auth.CSRFToken(c)})
	})
	r.GET("/ams/api/alarms", s.alarmsJSON)
	r.POST("/ams/api/alarms/:id/ack", auth.RequireCSRF(), s.alarmAck)
}

// alarmsJSON returns the current state of every alarm. Filter with
// ?phase=active_unack,active_ack,rtn_unack,normal (comma separated).
func (s *Server) alarmsJSON(c *gin.Context) {
	all, err := s.store.ListAlarmCurrent(c.Request.Context())
	if err != nil {
		log.Printf("web: alarm states: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
		return
	}

	want := map[string]bool{}
	for _, p := range strings.Split(c.Query("phase"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			want[p] = true
		}
	}
	counts := map[string]int{
		store.PhaseActiveUnack: 0, store.PhaseActiveAck: 0, store.PhaseRTNUnack: 0, store.PhaseNormal: 0, "data_fault": 0,
	}
	list := make([]store.AlarmCurrent, 0, len(all))
	for _, a := range all {
		if !a.Enabled {
			continue
		}
		counts[a.Phase]++
		if !a.DataValid {
			counts["data_fault"]++
		}
		if len(want) == 0 || want[a.Phase] {
			list = append(list, a)
		}
	}
	c.JSON(http.StatusOK, gin.H{"alarms": list, "counts": counts, "server_time": time.Now().UTC()})
}

// alarmAck acknowledges one activation of an alarm. The form carries
// "cycle_no": the activation the operator saw. The acting user is the logged-in
// user; any user id the client might send is ignored.
func (s *Server) alarmAck(c *gin.Context) {
	id, err1 := strconv.Atoi(c.Param("id"))
	cycle, err2 := strconv.Atoi(c.PostForm("cycle_no"))
	if err1 != nil || err2 != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": ams.ErrBadRequest.Error()})
		return
	}
	res, err := s.ack.Acknowledge(c.Request.Context(), auth.FromContext(c), c.ClientIP(), id, cycle)
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"result": res.Result, "cycle_no": res.CycleNo, "acknowledged_by": res.By, "acknowledged_at": res.At})
	case errors.Is(err, ams.ErrForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "not permitted"})
	case errors.Is(err, ams.ErrBadRequest):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, store.ErrAckStale):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "stale"})
	case errors.Is(err, store.ErrNothingToAck):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "code": "nothing_to_acknowledge"})
	default:
		log.Printf("web: acknowledge alarm %d cycle %d: %v", id, cycle, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
	}
}
