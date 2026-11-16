package glovotools

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mctlhq/mctl-glovo/internal/childmcp"
	"github.com/mctlhq/mctl-glovo/internal/tokenstore"
)

// errUnauthenticated is returned for a hosted call with no verified
// principal. The gateway authenticates before any tool runs, so this is a
// wiring error; the call is refused rather than run unattributed.
var errUnauthenticated = errors.New("unauthenticated")

const linkHint = "https://glovo.mctl.ai/link"

// hostedRefusal turns a per-pid client lookup failure into the answer for
// the caller. Known states become a fixed isError text that says what to
// do. Everything else, including a failed read of the token store, is an
// unavailable error: a dependency that could not be read is never reported
// as "not linked" or as an empty result. The texts are fixed and carry no
// session data, ciphertext or dependency detail.
func hostedRefusal(ctx context.Context, err error) (*mcp.CallToolResult, error) {
	if cerr := ctx.Err(); cerr != nil {
		return nil, errors.Join(cerr, err)
	}
	switch {
	case errors.Is(err, tokenstore.ErrNotLinked):
		return mcp.NewToolResultError("Your Glovo account is not linked. Link it at " + linkHint + "."), nil
	case errors.Is(err, tokenstore.ErrNeedsRelink):
		return mcp.NewToolResultError("Your Glovo session expired. Link your account again at " + linkHint + "."), nil
	case errors.Is(err, tokenstore.ErrSuspended):
		return mcp.NewToolResultError("Your Glovo link is suspended."), nil
	case errors.Is(err, tokenstore.ErrKilled):
		return mcp.NewToolResultError("Glovo account access is switched off."), nil
	case errors.Is(err, tokenstore.ErrRateLimited):
		return mcp.NewToolResultError("Glovo request rate limit reached. Try again later."), nil
	case errors.Is(err, tokenstore.ErrBusy):
		return mcp.NewToolResultError("Your Glovo link is busy. Retry in a moment."), nil
	}
	return nil, childmcp.ErrUnavailable
}
