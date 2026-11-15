package tokenstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// Store is the token store (design 10.1).
type Store interface {
	// CreateLinkCode mints a one-time code for pid and returns it once.
	// consentVersion is the /link text version the user agreed to; it is
	// stored with the code and lands on the link row.
	CreateLinkCode(ctx context.Context, pid PID, consentVersion string) (code string, expires time.Time, err error)
	// PreviewLink returns the masked identity a code belongs to, without
	// burning it.
	PreviewLink(ctx context.Context, code string) (maskedIdentity string, err error)
	// LinkAttempt charges one /link/preview or /link/upload attempt from ip
	// against the link caps. The /link handlers call it first.
	LinkAttempt(ip string) error
	// Link burns code and stores sess for the code's pid, replacing any
	// previous link.
	Link(ctx context.Context, code string, sess glovoapi.Session) error
	// Get decrypts pid's session. ErrNotLinked only for a documented
	// absence (no row).
	Get(ctx context.Context, pid PID) (*Lease, error)
	// Update takes pid's refresh lease (ErrBusy if another holder has it),
	// decrypts, runs fn with no DB transaction open, and writes the result
	// fenced on (link_id, version). A lost fence is ErrSuperseded and the
	// result is discarded.
	Update(ctx context.Context, pid PID, fn func(context.Context, glovoapi.Session) (glovoapi.Session, error)) error
	// Unlink deletes pid's link. It works with the kill switch on and
	// needs no Vault.
	Unlink(ctx context.Context, pid PID, reason string) error
	// Erase is an erasure request (Art. 17): Unlink, then pseudonymise pid
	// in the audit (Q8).
	Erase(ctx context.Context, pid PID) error
	// Status reports link state without decrypting.
	Status(ctx context.Context, pid PID) (Status, error)
	// Running returns nil unless the kill switch is on or unreadable.
	Running(ctx context.Context) error
	// Rewrap rewraps every row to the latest key version (design 2.4).
	Rewrap(ctx context.Context) (RewrapReport, error)
}

// Status is a link's state, read without a decrypt.
type Status struct {
	// Linked is false only when the row is absent (a read that ran).
	Linked  bool
	State   string
	LinkID  string
	Version int64
	// Killed is true when the kill switch is on or could not be read.
	Killed bool
}

// RewrapReport counts one sweep. It is only returned with a nil error: a
// sweep whose listing failed is an error, never "zero rows to rewrap".
type RewrapReport struct {
	Rewrapped int
	Skipped   int // fence lost (concurrent write or live lease)
	Failed    int // transit refused the row (integrity)
}

// Config configures New. DB, Transit, Kill and ValidateLink are required.
type Config struct {
	DB      DB
	Transit Transit
	// Kill is required: the store cannot be built without it.
	Kill *KillSwitch
	// Caps zero means DefaultCaps. A partially set Caps is refused.
	Caps   Caps
	Logger *slog.Logger
	// ValidateLink makes the one validating Glovo call of design 4.1 with
	// an uploaded session, through the proxy. Required: no link without it.
	ValidateLink func(ctx context.Context, sess glovoapi.Session) error
	// MaskIdentity returns the masked MCTL identity of a pid, from
	// mctl-api. Nil makes PreviewLink refuse.
	MaskIdentity func(ctx context.Context, pid string) (string, error)
	// ClientOptions apply to every client a Lease builds (proxy, base URL).
	ClientOptions []glovoapi.Option

	LeaseTTL        time.Duration // zero: 30 s
	RefreshDeadline time.Duration // zero: 15 s; must be below LeaseTTL
	BusyWait        time.Duration // zero: 5 s
}

type store struct {
	db           DB
	transit      Transit
	kill         *KillSwitch
	caps         Caps
	lim          *limiter
	log          *slog.Logger
	validateLink func(context.Context, glovoapi.Session) error
	maskIdentity func(context.Context, string) (string, error)
	clientOpts   []glovoapi.Option

	leaseTTL, refreshDeadline, busyWait time.Duration
	commitRetries                       int

	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

// New builds the store. It does no I/O.
func New(cfg Config) (Store, error) {
	switch {
	case cfg.Kill == nil:
		return nil, errors.New("tokenstore: a kill switch is required")
	case cfg.DB == nil:
		return nil, errors.New("tokenstore: a DB is required")
	case cfg.Transit == nil:
		return nil, errors.New("tokenstore: a transit client is required")
	case cfg.ValidateLink == nil:
		return nil, errors.New("tokenstore: a link validator is required")
	}
	caps := cfg.Caps
	if caps == (Caps{}) {
		caps = DefaultCaps()
	}
	if !caps.valid() {
		return nil, errors.New("tokenstore: every cap must be positive")
	}
	s := &store{
		db:              cfg.DB,
		transit:         cfg.Transit,
		kill:            cfg.Kill,
		caps:            caps,
		lim:             newLimiter(caps),
		log:             cfg.Logger,
		validateLink:    cfg.ValidateLink,
		maskIdentity:    cfg.MaskIdentity,
		clientOpts:      append([]glovoapi.Option(nil), cfg.ClientOptions...),
		leaseTTL:        orDur(cfg.LeaseTTL, 30*time.Second),
		refreshDeadline: orDur(cfg.RefreshDeadline, 15*time.Second),
		busyWait:        orDur(cfg.BusyWait, 5*time.Second),
		commitRetries:   3,
		now:             time.Now,
		sleep:           sleepCtx,
	}
	if s.refreshDeadline >= s.leaseTTL {
		return nil, errors.New("tokenstore: the refresh deadline must be shorter than the lease TTL")
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s, nil
}

func orDur(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- sessions as bytes -------------------------------------------------

const (
	maxSessionBytes = 16 << 10
	maxTokenBytes   = 8 << 10
	maxDeviceBytes  = 512
)

// checkSession is the shape check of design 4.1 step 4, applied to every
// session before it is encrypted.
func checkSession(sess glovoapi.Session) ([]byte, error) {
	str := func(k string, max int) bool {
		v, ok := sess[k].(string)
		return ok && v != "" && len(v) <= max
	}
	if sess == nil || !str("accessToken", maxTokenBytes) || !str("refreshToken", maxTokenBytes) || !str("deviceUrn", maxDeviceBytes) {
		return nil, ErrSessionRefused
	}
	raw, err := json.Marshal(map[string]any(sess))
	if err != nil || len(raw) > maxSessionBytes {
		return nil, ErrSessionRefused
	}
	return raw, nil
}

func decodeSession(pt []byte) (glovoapi.Session, error) {
	dec := json.NewDecoder(bytes.NewReader(pt))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, ErrIntegrity
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrIntegrity
	}
	return glovoapi.Session(m), nil
}

func copySession(s glovoapi.Session) glovoapi.Session {
	out := make(glovoapi.Session, len(s))
	for k, v := range s {
		out[k] = v
	}
	return out
}

// --- decrypt and encrypt -----------------------------------------------

func (s *store) refuseState(state string) error {
	switch state {
	case StateActive:
		return nil
	case StateSuspended:
		return ErrSuspended
	case StateNeedsRelink:
		return ErrNeedsRelink
	default:
		return ErrIntegrity
	}
}

// decryptRow is the only path to Vault decrypt. It checks the kill switch,
// the decrypt caps and the key version, writes the audit row, and only then
// calls Vault.
func (s *store) decryptRow(ctx context.Context, pid PID, row LinkRow) (glovoapi.Session, error) {
	if err := s.kill.Check(ctx); err != nil {
		return nil, ErrKilled
	}
	now := s.now()
	if !s.lim.globalDecrypt.allow("", now) {
		// The exfiltration pattern of threat row R: stop everything.
		_ = s.kill.Trip(ctx, "tokenstore:global_decrypt_cap")
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "cap_exceeded", Outcome: outcomeRefused, Reason: "global_decrypt_cap"})
		_ = s.audit(ctx, AuditEvent{Event: "kill_on", Outcome: outcomeOK, Reason: "global_decrypt_cap"})
		return nil, ErrKilled
	}
	if !s.lim.pidDecrypt.allow(pid.id, now) {
		_ = s.db.SetState(ctx, pid.id, row.LinkID, StateSuspended)
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "cap_exceeded", Outcome: outcomeRefused, Reason: "pid_decrypt_cap"})
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "suspend", Outcome: outcomeOK, Reason: "pid_decrypt_cap"})
		return nil, ErrSuspended
	}
	if kv, err := keyVersionOf(row.Ciphertext); err != nil || kv != row.KeyVersion {
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "integrity_error", Outcome: outcomeRefused, Reason: "key_version_mismatch"})
		return nil, ErrIntegrity
	}
	// The audit row comes first: a decrypt that could not be recorded
	// does not happen.
	if err := s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "decrypt", Outcome: outcomeOK, Reason: "issued"}); err != nil {
		return nil, ErrUnavailable
	}
	pt, err := s.transit.Decrypt(ctx, keyContext(pid, row.LinkID), row.Ciphertext)
	if errors.Is(err, errTransitRejected) {
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "integrity_error", Outcome: outcomeRefused, Reason: "transit_rejected"})
		return nil, ErrIntegrity
	}
	if err != nil {
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "decrypt", Outcome: outcomeError, Reason: "vault_unavailable"})
		return nil, ErrUnavailable
	}
	sess, err := decodeSession(pt)
	if err != nil {
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "integrity_error", Outcome: outcomeRefused, Reason: "plaintext_malformed"})
		return nil, ErrIntegrity
	}
	return sess, nil
}

func (s *store) encrypt(ctx context.Context, pid PID, linkID string, sess glovoapi.Session) (string, int, error) {
	raw, err := checkSession(sess)
	if err != nil {
		return "", 0, err
	}
	ct, err := s.transit.Encrypt(ctx, keyContext(pid, linkID), raw)
	if err != nil {
		return "", 0, ErrUnavailable
	}
	kv, err := keyVersionOf(ct)
	if err != nil {
		return "", 0, ErrUnavailable
	}
	return ct, kv, nil
}

// --- Get and Status ----------------------------------------------------

func (s *store) readActive(ctx context.Context, pid PID) (LinkRow, error) {
	row, err := s.db.ReadLink(ctx, pid.id)
	if errors.Is(err, errNoRow) {
		return LinkRow{}, ErrNotLinked
	}
	if err != nil {
		return LinkRow{}, ErrUnavailable
	}
	if err := s.refuseState(row.State); err != nil {
		return LinkRow{}, err
	}
	return row, nil
}

// Get implements Store.
func (s *store) Get(ctx context.Context, pid PID) (*Lease, error) {
	if !pid.valid() {
		return nil, ErrInvalidPID
	}
	if err := s.kill.Check(ctx); err != nil {
		return nil, ErrKilled
	}
	row, err := s.readActive(ctx, pid)
	if err != nil {
		return nil, err
	}
	sess, err := s.decryptRow(ctx, pid, row)
	if err != nil {
		return nil, err
	}
	return &Lease{store: s, pid: pid, linkID: row.LinkID, version: row.Version, session: sess}, nil
}

// Status implements Store.
func (s *store) Status(ctx context.Context, pid PID) (Status, error) {
	if !pid.valid() {
		return Status{}, ErrInvalidPID
	}
	killed := s.kill.Check(ctx) != nil
	row, err := s.db.ReadLink(ctx, pid.id)
	if errors.Is(err, errNoRow) {
		return Status{Linked: false, Killed: killed}, nil
	}
	if err != nil {
		return Status{}, ErrUnavailable
	}
	return Status{Linked: true, State: row.State, LinkID: row.LinkID, Version: row.Version, Killed: killed}, nil
}

// Running implements Store.
func (s *store) Running(ctx context.Context) error { return s.kill.Check(ctx) }

// --- the refresh path: lease + CAS fence (Q7) --------------------------

// pendingWrite is a refreshed session that Glovo accepted but Postgres did
// not confirm. It lives in the Lease, in memory only.
type pendingWrite struct {
	linkID, writeID string
	base            int64
	session         glovoapi.Session
}

// updateFn returns the next session and whether it differs from cur.
type updateFn func(ctx context.Context, cur glovoapi.Session) (glovoapi.Session, bool, error)

// acquire takes pid's refresh lease, waiting up to busyWait for another
// holder. linkID "" accepts the current link.
func (s *store) acquire(ctx context.Context, pid PID, linkID, holder string) (LinkRow, error) {
	deadline := s.now().Add(s.busyWait)
	for {
		row, err := s.db.AcquireLease(ctx, pid.id, linkID, holder, s.leaseTTL)
		if err == nil {
			return row, nil
		}
		if !errors.Is(err, errNoRow) {
			return LinkRow{}, ErrUnavailable
		}
		cur, err := s.readActive(ctx, pid)
		if err != nil {
			return LinkRow{}, err
		}
		if linkID != "" && cur.LinkID != linkID {
			// Relinked: the caller's session belongs to a link that is gone.
			return LinkRow{}, ErrNotLinked
		}
		if !s.now().Before(deadline) {
			return LinkRow{}, ErrBusy
		}
		if err := s.sleep(ctx, 100*time.Millisecond); err != nil {
			return LinkRow{}, ErrBusy
		}
	}
}

// classifyRefreshErr maps a failed fn to (addFailure, newState).
func (s *store) classifyRefreshErr(err error, failures int) (bool, string, string) {
	var ae *glovoapi.AuthError
	if errors.As(err, &ae) {
		switch {
		case ae.Status == 401 || ae.Status == 403:
			return true, StateNeedsRelink, "glovo_401"
		case ae.Status >= 500:
			return false, "", "glovo_5xx"
		}
		if failures+1 >= s.caps.RefreshFailuresInARow {
			return true, StateNeedsRelink, "refresh_failures"
		}
		return true, "", "glovo_refused"
	}
	if errors.Is(err, ErrSessionRefused) {
		if failures+1 >= s.caps.RefreshFailuresInARow {
			return true, StateNeedsRelink, "refresh_failures"
		}
		return true, "", "session_refused"
	}
	// Proxy, network, deadline: transient, no state change.
	return false, "", "transient"
}

// update is the refresh path. It returns the session to use, its version
// in Postgres, and a pendingWrite when Glovo rotated the session but the
// write could not be confirmed.
func (s *store) update(ctx context.Context, pid PID, linkID string, actor string, fn updateFn) (glovoapi.Session, int64, *pendingWrite, error) {
	if !pid.valid() {
		return nil, 0, nil, ErrInvalidPID
	}
	if err := s.kill.Check(ctx); err != nil {
		return nil, 0, nil, ErrKilled
	}
	holder := newID("glh_")
	row, err := s.acquire(ctx, pid, linkID, holder)
	if err != nil {
		if errors.Is(err, ErrBusy) {
			_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: linkID, Event: "refresh", Outcome: outcomeRefused, Reason: "busy", Actor: actor})
		}
		return nil, 0, nil, err
	}
	release := func(addFailure bool, state string) {
		_ = s.db.ReleaseLease(ctx, pid.id, row.LinkID, holder, addFailure, state)
	}
	cur, err := s.decryptRow(ctx, pid, row)
	if err != nil {
		release(false, "")
		return nil, 0, nil, err
	}

	fctx, cancel := context.WithTimeout(ctx, s.refreshDeadline)
	next, changed, ferr := fn(fctx, copySession(cur))
	cancel()
	if ferr == nil && changed {
		if _, err := checkSession(next); err != nil {
			ferr = err
		}
	}
	if ferr != nil {
		add, state, reason := s.classifyRefreshErr(ferr, row.RefreshFailures)
		release(add, state)
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "refresh", Outcome: outcomeError, Reason: reason, Actor: actor})
		if state == StateNeedsRelink {
			return nil, 0, nil, ErrNeedsRelink
		}
		return nil, 0, nil, ferr
	}
	if !changed {
		// Another holder already rotated it; nothing to write.
		release(false, "")
		return cur, row.Version, nil, nil
	}

	p := &pendingWrite{linkID: row.LinkID, writeID: newID("glw_"), base: row.Version, session: next}
	v, err := s.commit(ctx, pid, p)
	switch {
	case err == nil:
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "refresh", Outcome: outcomeOK, Reason: "rotated", Actor: actor})
		return next, v, nil, nil
	case errors.Is(err, errFenceLost):
		// A relink, an unlink or another writer won. The rotated session
		// belongs to a (link_id, version) that is no longer current, so it
		// is discarded, never kept (2.5).
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "persist_discarded", Outcome: outcomeRefused, Reason: "fence_lost", Actor: actor})
		return nil, 0, nil, ErrSuperseded
	default:
		_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "persist_retry", Outcome: outcomeError, Reason: "unpersisted", Actor: actor})
		s.log.LogAttrs(ctx, slog.LevelError, "tokenstore", slog.String("alert", "GlovoSessionUnpersisted"), slog.String("link_id", row.LinkID))
		return next, row.Version, p, nil
	}
}

// commit writes p with the fence, retrying a write whose outcome is
// unknown. errFenceLost is final; any other error means "not confirmed".
func (s *store) commit(ctx context.Context, pid PID, p *pendingWrite) (int64, error) {
	ct, kv, err := s.encrypt(ctx, pid, p.linkID, p.session)
	if err != nil {
		return 0, err
	}
	w := LeaseCommit{PID: pid.id, LinkID: p.linkID, BaseVersion: p.base, Ciphertext: ct, KeyVersion: kv, WriteID: p.writeID}
	var last error
	for attempt := 0; attempt < s.commitRetries; attempt++ {
		if attempt > 0 {
			if err := s.sleep(ctx, time.Duration(attempt)*200*time.Millisecond); err != nil {
				return 0, err
			}
		}
		v, err := s.db.CommitLease(ctx, w)
		if err == nil {
			return v, nil
		}
		// An earlier attempt may have landed with its answer lost.
		if fv, ferr := s.db.FindWrite(ctx, pid.id, p.writeID); ferr == nil {
			return fv, nil
		} else if !errors.Is(ferr, errNoRow) {
			last = ErrUnavailable
			continue
		}
		if errors.Is(err, errFenceLost) {
			return 0, errFenceLost
		}
		last = err
	}
	return 0, last
}

// Update implements Store. A write that could not be confirmed is
// ErrUnavailable here: Update has no client to keep the session in, so
// refreshes go through a Lease's client instead.
func (s *store) Update(ctx context.Context, pid PID, fn func(context.Context, glovoapi.Session) (glovoapi.Session, error)) error {
	if fn == nil {
		return errors.New("tokenstore: nil update function")
	}
	_, _, p, err := s.update(ctx, pid, "", actorService, func(ctx context.Context, cur glovoapi.Session) (glovoapi.Session, bool, error) {
		next, err := fn(ctx, cur)
		return next, err == nil, err
	})
	if err != nil {
		return err
	}
	if p != nil {
		return ErrUnavailable
	}
	return nil
}

// --- link codes and linking --------------------------------------------

const linkCodeTTL = 10 * time.Minute

var consentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func hashCode(code string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != code {
		return nil, false
	}
	h := sha256.Sum256(raw)
	return h[:], true
}

// CreateLinkCode implements Store.
func (s *store) CreateLinkCode(ctx context.Context, pid PID, consentVersion string) (string, time.Time, error) {
	if !pid.valid() {
		return "", time.Time{}, ErrInvalidPID
	}
	if err := s.kill.Check(ctx); err != nil {
		return "", time.Time{}, ErrKilled
	}
	if !consentPattern.MatchString(consentVersion) {
		return "", time.Time{}, errors.New("tokenstore: invalid consent version")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, ErrUnavailable
	}
	code := base64.RawURLEncoding.EncodeToString(raw[:])
	h := sha256.Sum256(raw[:])
	expires := s.now().Add(linkCodeTTL)
	if err := s.db.InsertLinkCode(ctx, h[:], pid.id, consentVersion, expires); err != nil {
		return "", time.Time{}, ErrUnavailable
	}
	_ = s.audit(ctx, AuditEvent{PID: pid.id, Event: "link_code_issued", Outcome: outcomeOK, Reason: "issued", Actor: actorUser})
	return code, expires, nil
}

// LinkAttempt implements Store.
func (s *store) LinkAttempt(ip string) error {
	now := s.now()
	if ip == "" || !s.lim.linkIP.allow(ip, now) || !s.lim.linkGlobal.allow("", now) {
		return ErrRateLimited
	}
	return nil
}

// PreviewLink implements Store.
func (s *store) PreviewLink(ctx context.Context, code string) (string, error) {
	if err := s.kill.Check(ctx); err != nil {
		return "", ErrKilled
	}
	h, ok := hashCode(code)
	if !ok {
		return "", ErrCodeRefused
	}
	pid, err := s.db.PeekLinkCode(ctx, h)
	if errors.Is(err, errNoRow) {
		return "", ErrCodeRefused
	}
	if err != nil || s.maskIdentity == nil {
		return "", ErrUnavailable
	}
	masked, err := s.maskIdentity(ctx, pid)
	if err != nil || masked == "" {
		return "", ErrUnavailable
	}
	_ = s.audit(ctx, AuditEvent{PID: pid, Event: "link_preview", Outcome: outcomeOK, Reason: "previewed", Actor: actorUser})
	return masked, nil
}

// Link implements Store, in the order of design 4.1.
func (s *store) Link(ctx context.Context, code string, sess glovoapi.Session) error {
	if err := s.kill.Check(ctx); err != nil {
		return ErrKilled
	}
	h, ok := hashCode(code)
	if !ok {
		_ = s.audit(ctx, AuditEvent{Event: "link_refused", Outcome: outcomeRefused, Reason: "code_invalid", Actor: actorUser})
		return ErrCodeRefused
	}
	pidStr, consent, err := s.db.RedeemLinkCode(ctx, h)
	if errors.Is(err, errNoRow) {
		_ = s.audit(ctx, AuditEvent{Event: "link_refused", Outcome: outcomeRefused, Reason: "code_invalid", Actor: actorUser})
		return ErrCodeRefused
	}
	if err != nil {
		return ErrUnavailable
	}
	// The code is burned from here on, whatever happens next.
	pid := pidFromString(pidStr)
	refuse := func(reason string, e error) error {
		_ = s.audit(ctx, AuditEvent{PID: pid.id, Event: "link_refused", Outcome: outcomeRefused, Reason: reason, Actor: actorUser})
		return e
	}
	if !pid.valid() {
		return refuse("pid_malformed", ErrIntegrity)
	}
	if _, err := checkSession(sess); err != nil {
		return refuse("session_refused", ErrSessionRefused)
	}
	if err := s.validateLink(ctx, copySession(sess)); err != nil {
		return refuse("glovo_validation_failed", ErrSessionRefused)
	}
	linkID := newID("gln_")
	ct, kv, err := s.encrypt(ctx, pid, linkID, sess)
	if err != nil {
		return refuse("vault_unavailable", ErrUnavailable)
	}
	if _, err := s.db.UpsertLink(ctx, LinkWrite{PID: pid.id, LinkID: linkID, ConsentVersion: consent, Ciphertext: ct, KeyVersion: kv, WriteID: newID("glw_")}); err != nil {
		return refuse("db_unavailable", ErrUnavailable)
	}
	_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: linkID, Event: "link", Outcome: outcomeOK, Reason: "linked", Actor: actorUser})
	return nil
}

// --- unlink and erasure ------------------------------------------------

var reasonPattern = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// Unlink implements Store. No kill check and no Vault: deletion never
// depends on the thing that might be broken.
func (s *store) Unlink(ctx context.Context, pid PID, reason string) error {
	if !pid.valid() {
		return ErrInvalidPID
	}
	if !reasonPattern.MatchString(reason) {
		reason = "unspecified"
	}
	ev := AuditEvent{PID: pid.id, Event: "unlink", Outcome: outcomeOK, Reason: reason, Actor: actorUser, RequestID: requestID(ctx)}
	err := s.db.DeleteLink(ctx, pid.id, ev)
	s.logEvent(ctx, ev, err)
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

// Erase implements Store: unlink, the erasure event and the subject's
// pseudonymisation commit together or not at all (5).
func (s *store) Erase(ctx context.Context, pid PID) error {
	if !pid.valid() {
		return ErrInvalidPID
	}
	rid := requestID(ctx)
	unlink := AuditEvent{PID: pid.id, Event: "unlink", Outcome: outcomeOK, Reason: "erasure", Actor: actorOperator, RequestID: rid}
	erasure := AuditEvent{PID: pid.id, Event: "erasure", Outcome: outcomeOK, Reason: "erasure", Actor: actorOperator, RequestID: rid}
	err := s.db.EraseLink(ctx, pid.id, unlink, erasure)
	s.logEvent(ctx, erasure, err)
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

// --- rewrap sweep ------------------------------------------------------

// Rewrap implements Store.
func (s *store) Rewrap(ctx context.Context) (RewrapReport, error) {
	var rep RewrapReport
	after := ""
	for {
		if err := s.kill.Check(ctx); err != nil {
			return RewrapReport{}, ErrKilled
		}
		rows, err := s.db.ListLinks(ctx, after, 50)
		if err != nil {
			return RewrapReport{}, ErrUnavailable
		}
		if len(rows) == 0 {
			return rep, nil
		}
		for i := range rows {
			row := &rows[i]
			after = row.PID
			if err := s.kill.Check(ctx); err != nil {
				return RewrapReport{}, ErrKilled
			}
			pid := pidFromString(row.PID)
			if !pid.valid() {
				rep.Failed++
				continue
			}
			ct, err := s.transit.Rewrap(ctx, keyContext(pid, row.LinkID), row.Ciphertext)
			if errors.Is(err, errTransitRejected) {
				rep.Failed++
				_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "integrity_error", Outcome: outcomeRefused, Reason: "transit_rejected"})
				continue
			}
			if err != nil {
				return RewrapReport{}, ErrUnavailable
			}
			kv, err := keyVersionOf(ct)
			if err != nil {
				return RewrapReport{}, ErrUnavailable
			}
			_, err = s.db.CommitRewrap(ctx, pid.id, row.LinkID, row.Version, ct, kv, newID("glw_"))
			switch {
			case err == nil:
				rep.Rewrapped++
				_ = s.audit(ctx, AuditEvent{PID: pid.id, LinkID: row.LinkID, Event: "rewrap", Outcome: outcomeOK, Reason: fmt.Sprintf("key_v%d", kv)})
			case errors.Is(err, errFenceLost):
				rep.Skipped++
			default:
				return RewrapReport{}, ErrUnavailable
			}
		}
	}
}
