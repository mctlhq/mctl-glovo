package tokenstore

import (
	"context"
	"log/slog"
	"time"
)

// AuditEvent is one glovo_store_audit row (design section 5). It never
// carries a token, a ciphertext, Glovo data or text from a dependency:
// Reason is a fixed code.
type AuditEvent struct {
	PID       string
	LinkID    string
	Event     string
	Outcome   string
	Reason    string
	Actor     string
	RequestID string
}

// Audit outcomes and actors.
const (
	outcomeOK      = "ok"
	outcomeRefused = "refused"
	outcomeError   = "error"

	actorUser     = "user"
	actorService  = "service"
	actorOperator = "operator"
)

type requestIDKey struct{}

// WithRequestID attaches the tool call's request id to ctx, for audit
// correlation.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// cleanupTimeout bounds the work that must finish even when the caller's
// context is already done: audit rows, lease releases, safety writes and
// the persisting of a refresh Glovo has already accepted.
const cleanupTimeout = 5 * time.Second

// detached returns a context that keeps ctx's values but not its
// cancellation, bounded by cleanupTimeout.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// audit writes the row and the log line. The log line carries the link id
// but not the pid: after an erasure (Q8) the pid must not survive in the
// audit trail, and Loki lines cannot be rewritten.
func (s *store) audit(ctx context.Context, ev AuditEvent) error {
	ev.RequestID = requestID(ctx)
	if ev.Actor == "" {
		ev.Actor = actorService
	}
	// The record of what happened must not depend on the caller still
	// waiting: a cancelled request is often exactly what needs auditing.
	dctx, cancel := detached(ctx)
	defer cancel()
	err := s.db.InsertAudit(dctx, ev)
	s.logEvent(ctx, ev, err)
	return err
}

func (s *store) logEvent(ctx context.Context, ev AuditEvent, writeErr error) {
	attrs := []slog.Attr{
		slog.String("event", ev.Event),
		slog.String("outcome", ev.Outcome),
		slog.String("reason", ev.Reason),
		slog.String("actor_kind", ev.Actor),
	}
	if ev.LinkID != "" {
		attrs = append(attrs, slog.String("link_id", ev.LinkID))
	}
	if ev.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", ev.RequestID))
	}
	level := slog.LevelInfo
	if writeErr != nil {
		level = slog.LevelError
		attrs = append(attrs, slog.Bool("audit_row_written", false))
	}
	s.log.LogAttrs(ctx, level, "tokenstore", attrs...)
}
