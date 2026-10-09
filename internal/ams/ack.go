package ams

import (
	"context"
	"errors"
	"fmt"

	"PLC_Monitoring/internal/auth"
	"PLC_Monitoring/internal/store"
)

// PermAck is the permission to acknowledge alarms (seeded by migration 003).
const PermAck = "alarm.ack"

// ErrBadRequest: the request names no valid alarm or activation.
var ErrBadRequest = errors.New("an alarm id and the activation (cycle) number are required")

// AckRepo is the part of the store the acknowledgement service needs.
type AckRepo interface {
	AcknowledgeAlarm(ctx context.Context, alarmID, cycleNo int, actor store.Actor) (store.AckResult, error)
	RecordAudit(ctx context.Context, e store.AuditEntry) error
}

type AckService struct{ repo AckRepo }

func NewAckService(repo AckRepo) *AckService { return &AckService{repo: repo} }

// Acknowledge acknowledges the activation `cycleNo` of an alarm on behalf of
// the logged-in user. The user comes from the server-side session; nothing
// the client sends is trusted as identity.
//
// Acknowledging is not clearing: it changes who has seen the alarm, never the
// process condition, and it says nothing about whether the problem was fixed.
func (s *AckService) Acknowledge(ctx context.Context, user *auth.User, ip string, alarmID, cycleNo int) (store.AckResult, error) {
	actor := actorOf(user, ip)
	if !user.Can(PermAck) {
		_ = s.repo.RecordAudit(ctx, store.AuditEntry{
			Actor: actor, Action: "alarm.acknowledge", TargetType: "alarm", TargetID: fmt.Sprint(alarmID),
			Outcome: "denied", Reason: "missing permission " + PermAck,
		})
		return store.AckResult{}, ErrForbidden
	}
	if alarmID <= 0 || cycleNo <= 0 {
		return store.AckResult{}, ErrBadRequest
	}
	return s.repo.AcknowledgeAlarm(ctx, alarmID, cycleNo, actor)
}
