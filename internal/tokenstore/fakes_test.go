package tokenstore

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
)

// ---------------------------------------------------------------------
// Fake Vault transit: a derived aes256-gcm96 key. The per-context key is
// HMAC-SHA256(master[version], context), so a ciphertext only decrypts
// under the context it was made with, exactly the property the store
// relies on. A wrong context or a forged ciphertext answers 400, like Vault.

type fakeTransit struct {
	srv   *httptest.Server
	token string

	mu      sync.Mutex
	masters [][]byte // index = version-1

	encrypts, decrypts, rewraps atomic.Int64
	fail500                     atomic.Bool // every op answers 500 with an echo body
}

func newFakeTransit(t *testing.T) *fakeTransit {
	t.Helper()
	f := &fakeTransit{token: "test-vault-token"}
	f.rotate()
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTransit) rotate() {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	f.mu.Lock()
	f.masters = append(f.masters, k)
	f.mu.Unlock()
}

func (f *fakeTransit) aead(version int, context []byte) (cipher.AEAD, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if version < 1 || version > len(f.masters) {
		return nil, errors.New("no such version")
	}
	m := hmac.New(sha256.New, f.masters[version-1])
	m.Write(context)
	block, err := aes.NewCipher(m.Sum(nil))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (f *fakeTransit) latest() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.masters)
}

func (f *fakeTransit) seal(context, pt []byte) (string, error) {
	v := f.latest()
	a, err := f.aead(v, context)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, a.NonceSize())
	_, _ = rand.Read(nonce)
	return fmt.Sprintf("vault:v%d:%s", v, base64.StdEncoding.EncodeToString(a.Seal(nonce, nonce, pt, nil))), nil
}

func (f *fakeTransit) open(context []byte, ct string) ([]byte, error) {
	v, err := keyVersionOf(ct)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(ct[strings.Index(ct[len("vault:v"):], ":")+len("vault:v")+1:])
	if err != nil {
		return nil, err
	}
	a, err := f.aead(v, context)
	if err != nil {
		return nil, err
	}
	if len(raw) < a.NonceSize() {
		return nil, errors.New("short")
	}
	return a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], nil)
}

func (f *fakeTransit) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	if f.fail500.Load() {
		// Echo the request, so a client that logged bodies would leak.
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"internal"}, "echo": body})
		return
	}
	if r.Method != http.MethodPost || r.Header.Get("X-Vault-Token") != f.token {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	ctxb, err := base64.StdEncoding.DecodeString(body["context"])
	if err != nil || len(ctxb) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	reply := func(k, v string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{k: v}})
	}
	bad := func() {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"errors": []string{"cipher: message authentication failed"}, "echo": body})
	}
	switch r.URL.Path {
	case "/v1/glovo-transit/encrypt/glovo-session":
		f.encrypts.Add(1)
		pt, err := base64.StdEncoding.DecodeString(body["plaintext"])
		if err != nil {
			bad()
			return
		}
		ct, _ := f.seal(ctxb, pt)
		reply("ciphertext", ct)
	case "/v1/glovo-transit/decrypt/glovo-session":
		f.decrypts.Add(1)
		pt, err := f.open(ctxb, body["ciphertext"])
		if err != nil {
			bad()
			return
		}
		reply("plaintext", base64.StdEncoding.EncodeToString(pt))
	case "/v1/glovo-transit/rewrap/glovo-session":
		f.rewraps.Add(1)
		pt, err := f.open(ctxb, body["ciphertext"])
		if err != nil {
			bad()
			return
		}
		ct, _ := f.seal(ctxb, pt)
		reply("ciphertext", ct)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeTransit) client(t *testing.T) *VaultTransit {
	t.Helper()
	vt, err := newVaultTransit(TransitConfig{
		Addr:  f.srv.URL,
		Token: func(context.Context) (string, error) { return f.token, nil },
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	return vt
}

func (f *fakeTransit) vaultCalls() int64 {
	return f.encrypts.Load() + f.decrypts.Load() + f.rewraps.Load()
}

// ---------------------------------------------------------------------
// memDB: DB in memory, with the semantics of the SQL in postgres.go.

type memDB struct {
	mu       sync.Mutex
	killed   bool
	noCtl    bool // the control row is missing
	links    map[string]*memRow
	codes    map[string]*memCode
	seq      int64
	subjects map[string]*memSubject // by subject id
	audit    []memAudit
}

type memRow struct {
	LinkRow
	writeID, consent string
}

type memCode struct {
	pid, consent  string
	expires, used time.Time
}

type memSubject struct {
	id, pid string
	erased  bool
}

type memAudit struct {
	AuditEvent
	subject string
}

func newMemDB() *memDB {
	return &memDB{links: map[string]*memRow{}, codes: map[string]*memCode{}, subjects: map[string]*memSubject{}}
}

func (m *memDB) ReadControl(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.noCtl {
		return true, unavailable("read control row")
	}
	return m.killed, nil
}

func (m *memDB) SetKilled(_ context.Context, killed bool, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.noCtl {
		return unavailable("write control row")
	}
	m.killed = killed
	return nil
}

func (m *memDB) ReadLink(_ context.Context, pid string) (LinkRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.links[pid]
	if !ok {
		return LinkRow{}, errNoRow
	}
	return r.LinkRow, nil
}

func (m *memDB) AcquireLease(_ context.Context, pid, linkID, holder string, ttl time.Duration) (LinkRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.links[pid]
	now := time.Now()
	if !ok || (linkID != "" && r.LinkID != linkID) || r.State != StateActive || (r.LeaseHolder != "" && r.LeaseUntil.After(now)) {
		return LinkRow{}, errNoRow
	}
	r.LeaseHolder, r.LeaseUntil = holder, now.Add(ttl)
	return r.LinkRow, nil
}

func (m *memDB) CommitLease(_ context.Context, w LeaseCommit) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.links[w.PID]
	if !ok || r.LinkID != w.LinkID || r.Version != w.BaseVersion {
		return 0, errFenceLost
	}
	m.seq++
	r.Ciphertext, r.KeyVersion, r.writeID, r.Version = w.Ciphertext, w.KeyVersion, w.WriteID, m.seq
	r.LeaseHolder, r.LeaseUntil, r.RefreshFailures = "", time.Time{}, 0
	return r.Version, nil
}

func (m *memDB) ReleaseLease(_ context.Context, pid, linkID, holder string, addFailure bool, newState string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.links[pid]
	if !ok || r.LinkID != linkID || r.LeaseHolder != holder {
		return nil
	}
	r.LeaseHolder, r.LeaseUntil = "", time.Time{}
	if addFailure {
		r.RefreshFailures++
	}
	if newState != "" {
		r.State = newState
	}
	return nil
}

func (m *memDB) FindWrite(_ context.Context, pid, writeID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.links[pid]
	if !ok || r.writeID != writeID {
		return 0, errNoRow
	}
	return r.Version, nil
}

func (m *memDB) ListLinks(_ context.Context, after string, limit int) ([]LinkRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var pids []string
	for p := range m.links {
		if p > after {
			pids = append(pids, p)
		}
	}
	sort.Strings(pids)
	out := []LinkRow{}
	for _, p := range pids {
		if len(out) == limit {
			break
		}
		out = append(out, m.links[p].LinkRow)
	}
	return out, nil
}

func (m *memDB) CommitRewrap(_ context.Context, pid, linkID string, base int64, ct string, kv int, writeID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.links[pid]
	if !ok || r.LinkID != linkID || r.Version != base || (r.LeaseHolder != "" && r.LeaseUntil.After(time.Now())) {
		return 0, errFenceLost
	}
	m.seq++
	r.Ciphertext, r.KeyVersion, r.writeID, r.Version = ct, kv, writeID, m.seq
	return r.Version, nil
}

func (m *memDB) SetState(_ context.Context, pid, linkID, state string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.links[pid]
	if !ok || r.LinkID != linkID {
		return errNoRow
	}
	r.State = state
	return nil
}

func (m *memDB) InsertLinkCode(_ context.Context, hash []byte, pid, consent string, expires time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.codes {
		if c.pid == pid && c.used.IsZero() {
			c.used = time.Now()
		}
	}
	m.codes[string(hash)] = &memCode{pid: pid, consent: consent, expires: expires}
	return nil
}

func (m *memDB) live(hash []byte) *memCode {
	c, ok := m.codes[string(hash)]
	if !ok || !c.used.IsZero() || !c.expires.After(time.Now()) {
		return nil
	}
	return c
}

func (m *memDB) PeekLinkCode(_ context.Context, hash []byte) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.live(hash)
	if c == nil {
		return "", errNoRow
	}
	return c.pid, nil
}

func (m *memDB) RedeemLinkCode(_ context.Context, hash []byte) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.live(hash)
	if c == nil {
		return "", "", errNoRow
	}
	c.used = time.Now()
	return c.pid, c.consent, nil
}

func (m *memDB) UpsertLink(_ context.Context, w LinkWrite) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.links[w.PID] = &memRow{
		LinkRow: LinkRow{PID: w.PID, LinkID: w.LinkID, State: StateActive, Ciphertext: w.Ciphertext, KeyVersion: w.KeyVersion, Version: m.seq},
		writeID: w.WriteID, consent: w.ConsentVersion,
	}
	return m.seq, nil
}

func (m *memDB) DeleteLink(ctx context.Context, pid string, ev AuditEvent) error {
	m.mu.Lock()
	delete(m.links, pid)
	for _, c := range m.codes {
		if c.pid == pid && c.used.IsZero() {
			c.used = time.Now()
		}
	}
	m.mu.Unlock()
	return m.InsertAudit(ctx, ev)
}

func (m *memDB) InsertAudit(_ context.Context, ev AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	subject := ""
	if ev.PID != "" {
		for _, s := range m.subjects {
			if s.pid == ev.PID {
				subject = s.id
			}
		}
		if subject == "" {
			subject = newID("gas_")
			m.subjects[subject] = &memSubject{id: subject, pid: ev.PID}
		}
	}
	ev.PID = "" // the table stores the subject, not the pid
	m.audit = append(m.audit, memAudit{AuditEvent: ev, subject: subject})
	return nil
}

func (m *memDB) EraseLink(ctx context.Context, pid string, unlink, erasure AuditEvent) error {
	if err := m.DeleteLink(ctx, pid, unlink); err != nil {
		return err
	}
	if err := m.InsertAudit(ctx, erasure); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.subjects {
		if s.pid == pid {
			s.pid, s.erased = "", true
		}
	}
	return nil
}

func (m *memDB) events(event string) []memAudit {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []memAudit
	for i := range m.audit {
		if m.audit[i].Event == event {
			out = append(out, m.audit[i])
		}
	}
	return out
}

// ---------------------------------------------------------------------
// faultDB wraps any DB and fails chosen methods, so the same fault tests
// run against memDB and Postgres.

type faultDB struct {
	DB
	mu    sync.Mutex
	fails map[string]int // method -> remaining failures (-1 = forever)
}

func newFaultDB(inner DB) *faultDB { return &faultDB{DB: inner, fails: map[string]int{}} }

func (f *faultDB) set(method string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[method] = n
}

func (f *faultDB) hit(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.fails[method]
	if !ok || n == 0 {
		return false
	}
	if n > 0 {
		f.fails[method] = n - 1
	}
	return true
}

var errInjected = errors.New("injected connection reset")

func (f *faultDB) ReadControl(ctx context.Context) (bool, error) {
	if f.hit("ReadControl") {
		// The worst shape: "false" together with the error.
		return false, errInjected
	}
	return f.DB.ReadControl(ctx)
}

func (f *faultDB) ReadLink(ctx context.Context, pid string) (LinkRow, error) {
	if f.hit("ReadLink") {
		return LinkRow{}, errInjected
	}
	return f.DB.ReadLink(ctx, pid)
}

func (f *faultDB) CommitLease(ctx context.Context, w LeaseCommit) (int64, error) {
	if f.hit("CommitLease") {
		return 0, errInjected
	}
	if f.hit("CommitLeaseLost") {
		// The write lands but its answer is lost.
		_, _ = f.DB.CommitLease(ctx, w)
		return 0, errInjected
	}
	return f.DB.CommitLease(ctx, w)
}

func (f *faultDB) FindWrite(ctx context.Context, pid, writeID string) (int64, error) {
	if f.hit("FindWrite") {
		return 0, errInjected
	}
	return f.DB.FindWrite(ctx, pid, writeID)
}

func (f *faultDB) InsertAudit(ctx context.Context, ev AuditEvent) error {
	if f.hit("InsertAudit") {
		return errInjected
	}
	return f.DB.InsertAudit(ctx, ev)
}

func (f *faultDB) ListLinks(ctx context.Context, after string, limit int) ([]LinkRow, error) {
	if f.hit("ListLinks") {
		return nil, errInjected
	}
	return f.DB.ListLinks(ctx, after, limit)
}

// ---------------------------------------------------------------------
// Fake Glovo: /oauth/refresh rotates both tokens and counts; the basket
// read requires the current access token.

type fakeGlovo struct {
	srv       *httptest.Server
	refreshes atomic.Int64
	reads     atomic.Int64
	delay     time.Duration
	status    atomic.Int64 // non-zero: /oauth/refresh answers this status

	mu      sync.Mutex
	valid   map[string]string // refresh token -> access token it was issued with
	access  map[string]bool
	counter int
}

func newFakeGlovo(t *testing.T) *fakeGlovo {
	t.Helper()
	g := &fakeGlovo{valid: map[string]string{}, access: map[string]bool{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

// issue registers a session's tokens as live.
func (g *fakeGlovo) issue(s glovoapi.Session) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.valid[s["refreshToken"].(string)] = s["accessToken"].(string)
	g.access[s["accessToken"].(string)] = true
}

func (g *fakeGlovo) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/oauth/refresh":
		g.refreshes.Add(1)
		if g.delay > 0 {
			time.Sleep(g.delay)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if st := g.status.Load(); st != 0 {
			// Echo the refresh token, as a hostile upstream might.
			w.WriteHeader(int(st))
			_, _ = w.Write([]byte(`{"error":"bad","token":"` + body["refreshToken"] + `"}`))
			return
		}
		g.mu.Lock()
		_, ok := g.valid[body["refreshToken"]]
		if ok {
			delete(g.valid, body["refreshToken"]) // worst case: one use only
		}
		g.counter++
		n := g.counter
		next := map[string]any{"accessToken": "access-r" + strconv.Itoa(n), "refreshToken": "refresh-r" + strconv.Itoa(n), "expiresIn": 3600}
		if ok {
			g.valid[next["refreshToken"].(string)] = next["accessToken"].(string)
			g.access[next["accessToken"].(string)] = true
		}
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(next)
	case strings.HasPrefix(r.URL.Path, "/v1/authenticated/customers/"):
		g.reads.Add(1)
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		g.mu.Lock()
		ok := g.access[tok]
		g.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// ---------------------------------------------------------------------
// Harness.

type harness struct {
	t       *testing.T
	db      *faultDB
	raw     DB
	mem     *memDB // nil for Postgres
	transit *fakeTransit
	glovo   *fakeGlovo
	kill    *KillSwitch
	st      *store
	logs    *syncBuffer
	swap    func(a, b string) // swap two rows' ciphertexts
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const (
	pidA = "prn_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	pidB = "prn_bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func testSession(tag string) glovoapi.Session {
	return glovoapi.Session{
		"accessToken":  "access-" + tag,
		"refreshToken": "refresh-" + tag,
		"deviceUrn":    "urn:device:" + tag,
		"customerId":   "cust-" + tag,
		"createdAt":    json.Number(strconv.FormatInt(time.Now().UnixMilli(), 10)),
		"expiresIn":    json.Number("3600"),
	}
}
