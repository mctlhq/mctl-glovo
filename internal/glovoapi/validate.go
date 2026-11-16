package glovoapi

import (
	"context"
	"errors"
)

// ErrValidationRefresh is what a validating client answers instead of
// refreshing. An uploaded session is stored exactly as it was uploaded, so a
// refresh during validation would rotate the refresh token Glovo keeps and
// leave a dead one in the store.
var ErrValidationRefresh = errors.New("glovoapi: the uploaded session needs a refresh; capture a fresh session")

// ErrValidationFailed is the one error a validator returns for a session
// Glovo did not accept, whatever the cause. Its text is fixed: the cause may
// carry a piece of Glovo's response, and the caller only needs "no".
var ErrValidationFailed = errors.New("glovoapi: the uploaded session was not accepted")

// NewSessionValidator returns the validating call of the link flow
// (docs/design/token-store.md 4.1 step 4): one authenticated read, the basket
// GET the keepalive uses, made with the uploaded session and nothing else.
//
// Every validating client is built with WithRequireProxy on top of opts, so
// a validator without a usable WithProxy is a constructor error, never a
// direct call to Glovo. The client is hosted: it never reads or writes a
// session file, and it never refreshes (ErrValidationRefresh).
func NewSessionValidator(opts ...Option) (func(context.Context, Session) error, error) {
	build := func(s Session) (*Client, error) {
		all := append(append([]Option(nil), opts...),
			WithSession(s),
			WithRefreshCoordinator(func(context.Context, Session, func(context.Context, Session) (Session, error)) (Session, error) {
				return nil, ErrValidationRefresh
			}),
			WithRequireProxy())
		return New("", all...)
	}
	// Fail at construction, not on the first upload: a missing proxy is a
	// deployment error.
	if _, err := build(Session{}); err != nil {
		return nil, err
	}
	return func(ctx context.Context, s Session) error {
		if s == nil {
			return ErrValidationFailed
		}
		c, err := build(copyForValidation(s))
		if err != nil {
			return ErrValidationFailed
		}
		if _, err := c.GetBaskets(ctx); err != nil {
			return ErrValidationFailed
		}
		return nil
	}, nil
}

// copyForValidation keeps the caller's map out of the client: the client
// may rewrite fields of the session it holds.
func copyForValidation(s Session) Session {
	out := make(Session, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}
