package glovoapi

import (
	"context"
	"errors"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// RefreshCoordinator persists a token refresh for a hosted client, one whose
// session lives in the token store rather than in a file.
//
// The client calls it instead of refreshing and writing a session file.
// held is the session the client holds now. exchange performs the Glovo
// refresh for the session it is given and returns the rotated session
// without persisting anything. The coordinator decides whether a refresh
// is still needed (another replica may already have rotated the token),
// runs exchange under its own cross-replica fence, stores the result and
// returns the session the client must use from now on. On an error the
// client keeps its current session.
type RefreshCoordinator func(ctx context.Context, held Session, exchange func(context.Context, Session) (Session, error)) (Session, error)

// ErrHostedSessionPath is returned by New for a hosted client (one built
// with WithRefreshCoordinator) that was also given a session path. A hosted
// client never reads or writes a session file.
var ErrHostedSessionPath = errors.New("glovoapi: a hosted client must not have a session path")

// ErrHostedNoFile is returned by Reload and by location writes on a hosted
// client: those operations would read or write a session file.
var ErrHostedNoFile = errors.New("glovoapi: a hosted client has no session file")

// WithRefreshCoordinator makes the client hosted: refreshes go through rc
// and no code path reads or writes a session file. nil is refused.
func WithRefreshCoordinator(rc RefreshCoordinator) Option {
	return func(c *Client) {
		if rc == nil {
			c.setOptErr(errors.New("glovoapi: WithRefreshCoordinator: nil coordinator"))
			return
		}
		c.coordinator = rc
	}
}

// SessionWriter persists a change to a hosted client's session, other than a
// token refresh. apply receives the session as the store holds it now and
// returns the session to store; the writer runs it under the store's own
// cross-replica fence, so a concurrent refresh is never overwritten with
// stale tokens. It returns the session the client must use from now on. On
// an error nothing was stored and the client keeps its current session.
type SessionWriter func(ctx context.Context, apply func(Session) (Session, error)) (Session, error)

// WithSessionWriter lets a hosted client persist location changes through
// the token store. Without it a hosted client refuses them with
// ErrHostedNoFile: it never keeps an unsaved copy. nil is refused.
func WithSessionWriter(w SessionWriter) Option {
	return func(c *Client) {
		if w == nil {
			c.setOptErr(errors.New("glovoapi: WithSessionWriter: nil writer"))
			return
		}
		c.writer = w
	}
}

// WithRequestGate installs a check that runs before every request to Glovo,
// refreshes included. A non-nil error refuses the request and is returned
// unchanged. The token store uses it for the kill switch and the request
// caps. nil is refused.
func WithRequestGate(gate func(context.Context) error) Option {
	return func(c *Client) {
		if gate == nil {
			c.setOptErr(errors.New("glovoapi: WithRequestGate: nil gate"))
			return
		}
		c.gate = gate
	}
}

// ErrBasketWritesDisabled is returned for a basket mutation on a hosted
// client built without WithBasketWrites. The request is refused before the
// request gate, the kill switch bookkeeping and the network.
var ErrBasketWritesDisabled = errors.New("glovoapi: basket writes are disabled for this hosted client")

// WithBasketWrites lets a hosted client send basket mutations. Without it a
// hosted client is read-only for baskets: the default, so that a new way of
// building a hosted client cannot write a real basket by omission. Enabling
// it removes only this client-side refusal; it is not an approval.
func WithBasketWrites() Option { return func(c *Client) { c.basketWrites = true } }

// basketPathRe matches a baskets resource path, query excluded.
var basketPathRe = regexp.MustCompile(`(?i)(^|/)baskets(/|$)`)

// checkBasketWrite refuses a non-GET request to a baskets path on a hosted
// client without WithBasketWrites. The path is matched after path.Clean so
// that a dot segment cannot hide it; a request that is not a basket path is
// left to checkMutation.
func (c *Client) checkBasketWrite(method, p string) error {
	if !c.hosted() || c.basketWrites || method == http.MethodGet {
		return nil
	}
	raw := strings.SplitN(p, "?", 2)[0]
	if basketPathRe.MatchString(raw) || basketPathRe.MatchString(path.Clean(raw)) {
		return ErrBasketWritesDisabled
	}
	// By data: a request that matches a basket entry of the allowlist is a
	// basket mutation whatever its path looks like.
	cleaned := path.Clean(raw)
	for _, a := range mutationAllowlist {
		if a.basket && a.method == method && (a.path.MatchString(raw) || a.path.MatchString(cleaned)) {
			return ErrBasketWritesDisabled
		}
	}
	return nil
}

// hosted reports whether the client's session lives outside a file.
func (c *Client) hosted() bool { return c.coordinator != nil }

// exchangeRefresh performs the Glovo refresh for cur and returns the rotated
// session. It persists nothing and does not touch c.session.
func (c *Client) exchangeRefresh(ctx context.Context, cur Session) (Session, error) {
	return c.refreshSession(ctx, cur)
}
