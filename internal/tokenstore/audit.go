package tokenstore

import (
	"context"
	"log/slog"
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

// audit writes the row and the log line. The log line carries the link id
// but not the pid: after an erasure (Q8) the pid must not survive in the
// audit trail, and Loki lines cannot be rewritten.
func (s *store) audit(ctx context.Context, ev AuditEvent) error {
	ev.RequestID = requestID(ctx)
	if ev.Actor == "" {
		ev.Actor = actorService
	}
	err := s.db.InsertAudit(ctx, ev)
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
