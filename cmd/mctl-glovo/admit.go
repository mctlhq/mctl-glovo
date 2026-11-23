package main

import (
	"context"
	"errors"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/consentgate"
	"github.com/mctlhq/mctl-glovo/internal/linkflow"
)

const (
	// admitTTL is how long a positive per-call decision is trusted. A
	// consent withdrawn in mctl-api (which frees the slot) stops serving
	// within this time. It is deliberately shorter than the token
	// verifier's gate (consentgate.DefaultTTL), which only guards entry.
	admitTTL = 5 * time.Second
	// maxAdmitTTL is the hard ceiling newCallAdmitter enforces.
	maxAdmitTTL = 10 * time.Second
	// admitTimeout bounds one decision. The tool-call deadline starts only
	// after admission, so without this a slow mctl-api could hold a call
	// for as long as the HTTP client allows.
	admitTimeout = 4 * time.Second
)

// errAdmitRefused is what the middleware sees for every refusal; it carries
// no cause, because the caller only ever learns "access refused".
var errAdmitRefused = errors.New("admit: refused")

// newCallAdmitter builds the mcpDeps.Admit decision from mctl-api's consent
// read (the glovo:c2 consent, whose recording is also the slot claim).
// src is the same source the link flow uses. The pid it receives comes from
// the verified principal (auditMiddleware), never from a request argument.
//
// Only an explicit active consent to linkflow.ConsentVersion admits, and with
// GLOVO_C2_REQUIRE_ACCESS only together with access "approved" in the same
// answer (src carries the flag: c2Access). A "no", an access state that is
// not approved, absent or unknown,
// an unreadable token file, a transport error, a non-200, a malformed body,
// an answer for another principal and a timeout all refuse. Positive
// answers are cached per pid for admitTTL; a refusal or a failed read is
// never cached and evicts the pid's earlier positive.
func newCallAdmitter(src consentgate.Source, now func() time.Time) (callAdmitter, error) {
	return newCallAdmitterTTL(src, admitTTL, now)
}

func newCallAdmitterTTL(src consentgate.Source, ttl time.Duration, now func() time.Time) (callAdmitter, error) {
	if ttl <= 0 || ttl > maxAdmitTTL {
		return nil, errors.New("admit: ttl outside (0, 10s]")
	}
	gate, err := consentgate.New(consentgate.Config{Source: src, Version: linkflow.ConsentVersion, TTL: ttl, Now: now})
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, pid string) error {
		ctx, cancel := context.WithTimeout(ctx, admitTimeout)
		defer cancel()
		if gate.Admit(ctx, pid) != nil {
			return errAdmitRefused
		}
		return nil
	}, nil
}

// admitterFor is the mcpDeps.Admit of a C2 access mode: the per-call
// decision in consent mode, nil in allowlist-only mode (where the consent
// source never consents and the verifier's allowlist is the gate).
func admitterFor(src linkflow.ConsentSource, consentMode bool) (callAdmitter, error) {
	if !consentMode {
		return nil, nil
	}
	return newCallAdmitter(src, nil)
}
