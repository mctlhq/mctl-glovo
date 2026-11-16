package glovotools

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mctlhq/mctl-glovo/internal/childmcp"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// errUnauthenticated is returned for a hosted call with no verified
// principal. The gateway authenticates before any tool runs, so this is a
// wiring error; the call is refused rather than run unattributed.
var errUnauthenticated = errors.New("unauthenticated")

const linkHint = "https://glovo.mctl.ai/link"

// hostedRefusal turns a per-pid failure into the answer for the caller. It
// serves both the client lookup and errors that come back from inside a
// handler (the request gate and the refresh coordinator return tokenstore
// sentinels unwrapped), so a store outage reads the same before and during
// a call. Known states become a fixed isError text that says what to do.
// Everything else, including a failed read of the token store, is an
// unavailable error: a dependency that could not be read is never reported
// as "not linked" or as an empty result. The texts are fixed and carry no
// session data, ciphertext or dependency detail.
func hostedRefusal(ctx context.Context, err error) (*mcp.CallToolResult, error) {
	if cerr := ctx.Err(); cerr != nil {
		return nil, errors.Join(cerr, err)
	}
	if res, ok := hostedState(err); ok {
		return res, nil
	}
	return nil, childmcp.ErrUnavailable
}

// hostedState maps the sentinels that describe a user-visible state.
func hostedState(err error) (*mcp.CallToolResult, bool) {
	var text string
	switch {
	case errors.Is(err, tokenstore.ErrNotLinked):
		text = "Your Glovo account is not linked. Link it at " + linkHint + "."
	case errors.Is(err, tokenstore.ErrNeedsRelink):
		text = "Your Glovo session expired. Link your account again at " + linkHint + "."
	case errors.Is(err, tokenstore.ErrSessionRefused):
		// The refresh coordinator can return this mid-call: Glovo refused
		// the session, so only a new link helps.
		text = "Glovo refused your session. Link your account again at " + linkHint + "."
	case errors.Is(err, tokenstore.ErrSuspended):
		text = "Your Glovo link is suspended."
	case errors.Is(err, tokenstore.ErrKilled):
		text = "Glovo account access is switched off."
	case errors.Is(err, tokenstore.ErrRateLimited):
		text = "Glovo request rate limit reached. Try again later."
	case errors.Is(err, tokenstore.ErrBusy):
		text = "Your Glovo link is busy. Retry in a moment."
	case errors.Is(err, tokenstore.ErrSessionRefused):
		// A refreshed session failed validation mid-call (the refresh
		// coordinator returns it unwrapped). Fixed text, no detail.
		text = "Glovo did not accept the session refresh. Try again later; if it keeps failing, link your account again at " + linkHint + "."
	case errors.Is(err, tokenstore.ErrLocationRefused):
		text = "That location was not accepted."
	case errors.Is(err, glovoapi.ErrHostedNoFile):
		// A hosted client built without a session writer cannot store a
		// location change. The store wires one, so this is a fallback.
		text = "Setting the location is not available for this account."
	default:
		return nil, false
	}
	return mcp.NewToolResultError(text), true
}

// isStoreFailure reports a token store error that names no user-visible
// state: it is answered as unavailable, never with its own text.
func isStoreFailure(err error) bool {
	return errors.Is(err, tokenstore.ErrUnavailable) ||
		errors.Is(err, tokenstore.ErrIntegrity) ||
		errors.Is(err, tokenstore.ErrSuperseded) ||
		errors.Is(err, tokenstore.ErrInvalidPID)
}
