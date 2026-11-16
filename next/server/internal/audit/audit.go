// Package audit writes audit_logs rows (CONTRACTS §21.2). It is shared by
// every module that records console actions: plugin lifecycle, publishers,
// accounts and proxies.
package audit

import (
	"context"
	"encoding/json"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/Sub2API-Devs/sup2api/next/server/internal/store"
	"github.com/Sub2API-Devs/sup2api/next/server/internal/store/model"
)

type ipKey struct{}
type sourceKey struct{}

// WithSource marks host-originated operations, never input from an HTTP body.
func WithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}
func Source(ctx context.Context) string { value, _ := ctx.Value(sourceKey{}).(string); return value }

// WithClientIP stores the client IP for audit records.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ipKey{}, ip)
}

// ClientIP returns the IP stored by WithClientIP ("" when absent).
func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(ipKey{}).(string)
	return ip
}

// Context returns the request context carrying the client IP, for handlers
// that audit: pass it to Audit (and to everything else in the handler, so
// nested audits see the IP too).
func Context(c *gin.Context) context.Context {
	return WithClientIP(c.Request.Context(), c.ClientIP())
}

// Audit writes one audit_logs row. actorID 0 means system. detail nil is
// written as {}. q may be the pool or a transaction.
func Audit(ctx context.Context, q *gorm.DB, actorID int64, action, targetType, targetID string, detail any) error {
	var uid *int64
	if actorID > 0 {
		uid = &actorID
	}
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	ip := ClientIP(ctx)
	if len(ip) > 64 {
		ip = ip[:64]
	}
	return store.G(ctx, q).Create(&auditRow{UserID: uid, Action: action, TargetType: targetType, TargetID: targetID, Detail: b, IP: ip}).Error
}

// auditRow is the audit_logs row Audit writes: every column as given, the
// id and created_at their defaults, so the INSERT reads nothing back (no
// RETURNING) and stays one Exec.
type auditRow struct {
	UserID     *int64 `gorm:"column:user_id"`
	Action     string `gorm:"column:action"`
	TargetType string `gorm:"column:target_type"`
	TargetID   string `gorm:"column:target_id"`
	Detail     []byte `gorm:"column:detail"`
	IP         string `gorm:"column:ip"`
}

func (auditRow) TableName() string { return model.AuditLog{}.TableName() }
