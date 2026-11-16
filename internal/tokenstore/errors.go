package tokenstore

import "errors"

// Errors returned by the store. Their texts are fixed: no error from this
// package carries session data, ciphertext or a dependency's response body.
var (
	// ErrNotLinked is the documented absence: the pid has no link row.
	ErrNotLinked = errors.New("glovo account not linked")
	// ErrUnavailable means a dependency (Postgres, Vault) failed or could
	// not be read. Callers answer 503. It is never conflated with
	// ErrNotLinked.
	ErrUnavailable = errors.New("token store unavailable")
	// ErrKilled means the C2 kill switch is on, or could not be read.
	ErrKilled = errors.New("glovo c2 is switched off")
	// ErrSuspended means the link is suspended (operator or decrypt cap).
	ErrSuspended = errors.New("glovo link suspended")
	// ErrNeedsRelink means the Glovo session was rejected; link again.
	ErrNeedsRelink = errors.New("glovo session expired; link again")
	// ErrBusy means another holder has the refresh lease; retry.
	ErrBusy = errors.New("glovo link busy; retry")
	// ErrSuperseded means the (link_id, version) fence was lost: a relink,
	// unlink or another writer won, and the result was discarded.
	ErrSuperseded = errors.New("glovo link changed during the write")
	// ErrIntegrity means a row failed its integrity checks: wrong
	// context, forged ciphertext, or a key version mismatch.
	ErrIntegrity = errors.New("glovo link failed integrity check")
	// ErrCodeRefused is the one answer for an unknown, used or expired
	// link code.
	ErrCodeRefused = errors.New("link code invalid, used or expired")
	// ErrRateLimited means a cap from section 6 refused the request.
	ErrRateLimited = errors.New("glovo request rate limited")
	// ErrInvalidPID means the caller passed the zero PID.
	ErrInvalidPID = errors.New("tokenstore: no verified principal")
	// ErrSessionRefused means an uploaded or refreshed session failed
	// validation (shape or size).
	ErrSessionRefused = errors.New("glovo session refused")
	// ErrConsentRefused means the principal no longer holds the glovo:c2
	// consent when its link code is redeemed: nothing is stored.
	ErrConsentRefused = errors.New("glovo link consent not held")
)
