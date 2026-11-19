package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A caller-supplied test value: the helper has no default client.
const testClient = "test-client-id"

var testCode = strings.Repeat("Ab0-_", 8) + "xyz" // 43 base64url characters

type fakeServer struct {
	srv *httptest.Server
	idp *fakeIDP

	mu       sync.Mutex
	previews int
	uploads  int
	unlinks  int
	total    int // every hit on a /link route, authorized or not
	auths    []string
	lastBody map[string]any
	upload   func(w http.ResponseWriter)
	// freshRequired makes the link routes answer the server's stale-login
	// refusal, whatever the token looks like to the helper.
	freshRequired bool
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastBody = body
		if strings.HasPrefix(r.URL.Path, "/link/") {
			f.total++
			f.auths = append(f.auths, r.Header.Get("Authorization"))
			if f.freshRequired {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"fresh_login_required","message":"MARKER-hostile"}`))
				return
			}
			if want := f.idp.issuedToken(); want == "" || r.Header.Get("Authorization") != "Bearer "+want {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":"invalid_token","message":"x"}`))
				return
			}
		}
		switch r.URL.Path {
		case "/link/preview":
			f.previews++
			if body["schema"] != previewSchema || body["code"] != testCode {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"code_refused","message":"x"}`))
				return
			}
			_, _ = w.Write([]byte(`{"identity":"o***@e***.com"}`))
		case "/link/upload":
			f.uploads++
			if f.upload != nil {
				f.upload(w)
				return
			}
			_, _ = w.Write([]byte(`{"result":"linked"}`))
		case "/link/unlink":
			f.unlinks++
			if body["schema"] != unlinkSchema || len(body) != 1 {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"malformed_request","message":"x"}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":"unlinked"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	f.idp = newFakeIDP(t, f)
	return f
}

// client trusts both fake servers (the link server and the fake mctl-api).
func (f *fakeServer) client() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(f.srv.Certificate())
	pool.AddCert(f.idp.srv.Certificate())
	return newHTTPClient(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
}

func (f *fakeServer) env(t *testing.T) env {
	return env{
		hc:           f.client(),
		home:         t.TempDir(),
		issuer:       f.idp.srv.URL,
		open:         f.idp.browser,
		now:          time.Now,
		loginTimeout: 5 * time.Second,
	}
}

func (f *fakeServer) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

func (f *fakeServer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.previews, f.uploads
}

const sessionJSON = `{"accessToken":"MARKER-ACCESS","refreshToken":"MARKER-REFRESH","deviceUrn":"MARKER-DEVICE","customerId":4242}`

func writeSession(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

type result struct {
	code           int
	stdout, stderr string
}

func runHelper(t *testing.T, f *fakeServer, args []string, stdin string) result {
	t.Helper()
	t.Helper()
	all := append([]string{"-server", f.srv.URL, "-client-id", testClient}, args...)
	return runEnv(t, f, f.env(t), all, stdin)
}

func runEnv(t *testing.T, f *fakeServer, e env, args []string, stdin string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, strings.NewReader(stdin), &out, &errb, e)
	r := result{code, out.String(), errb.String()}
	// The login token is never shown, whatever the outcome.
	if tok := f.idp.issuedToken(); tok != "" && strings.Contains(r.stdout+r.stderr, tok) {
		t.Fatalf("output carries the login token:\n%s\n%s", r.stdout, r.stderr)
	}
	return r
}

func noMarkers(t *testing.T, r result) {
	t.Helper()
	if strings.Contains(r.stdout+r.stderr, "MARKER") {
		t.Fatalf("output carries session data:\n%s\n%s", r.stdout, r.stderr)
	}
}

func TestLinkFromFile(t *testing.T) {
	f := newFakeServer(t)
	p := writeSession(t, sessionJSON, 0o600)
	r := runHelper(t, f, []string{"-session", p}, testCode+"\ny\n")
	if r.code != 0 || r.stdout != "linked\n" {
		t.Fatalf("%d %q %q", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stderr, "o***@e***.com") {
		t.Fatal("masked identity not shown")
	}
	noMarkers(t, r)
	f.mu.Lock()
	sess, _ := f.lastBody["session"].(map[string]any)
	schema := f.lastBody["schema"]
	f.mu.Unlock()
	if schema != uploadSchema || sess["refreshToken"] != "MARKER-REFRESH" {
		t.Fatalf("upload body: %v", f.lastBody)
	}
	// Both link routes got the one login token, as a Bearer token.
	tok := f.idp.issuedToken()
	f.mu.Lock()
	auths := append([]string(nil), f.auths...)
	f.mu.Unlock()
	if tok == "" || len(auths) != 2 || auths[0] != "Bearer "+tok || auths[1] != "Bearer "+tok {
		t.Fatalf("link routes saw %d Authorization headers", len(auths))
	}
}

func TestLinkFromStdin(t *testing.T) {
	f := newFakeServer(t)
	r := runHelper(t, f, []string{"-session", "-"}, testCode+"\nyes\n"+sessionJSON)
	if r.code != 0 || r.stdout != "linked\n" {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	noMarkers(t, r)
}

// The code is never taken from argv: anything positional is refused before
// any network call, and the refusal does not echo it.
func TestCodeNeverFromArgv(t *testing.T) {
	f := newFakeServer(t)
	p := writeSession(t, sessionJSON, 0o600)
	for _, args := range [][]string{{testCode}, {"-session", p, testCode}, {"--", testCode}} {
		// stdin carries a valid code too: only the argv check stops the run.
		r := runHelper(t, f, args, testCode+"\ny\n")
		if r.code != 2 || r.stdout != msgArgs+"\n" || strings.Contains(r.stdout+r.stderr, testCode) {
			t.Fatalf("args %v: %d %q %q", args, r.code, r.stdout, r.stderr)
		}
	}
	// No flag takes it either.
	r := runHelper(t, f, []string{"-code", testCode}, "")
	if r.code == 0 {
		t.Fatal("-code accepted")
	}
	if pv, up := f.counts(); pv != 0 || up != 0 {
		t.Fatalf("network calls: %d previews, %d uploads", pv, up)
	}
}

// A code that starts with "-" parses as a flag. The flag package's own
// error text would echo it; none of it may reach stdout or stderr.
func TestDashCodeNeverEchoed(t *testing.T) {
	f := newFakeServer(t)
	p := writeSession(t, sessionJSON, 0o600)
	dash := "-" + testCode[1:]
	for _, args := range [][]string{{dash}, {"-session", p, dash}, {dash + "=x"}, {"-" + dash}} {
		r := runHelper(t, f, args, testCode+"\ny\n")
		if r.code != 2 || r.stdout != msgArgs+"\n" {
			t.Fatalf("args %v: %d %q", args, r.code, r.stdout)
		}
		if strings.Contains(r.stdout+r.stderr, dash[1:]) {
			t.Fatalf("args %v: the code reached the output: %q", args, r.stderr)
		}
		if r.stderr != usage+"\n" {
			t.Fatalf("args %v: stderr %q, want the fixed usage", args, r.stderr)
		}
	}
	if pv, up := f.counts(); pv != 0 || up != 0 {
		t.Fatalf("network calls: %d previews, %d uploads", pv, up)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-h"}, strings.NewReader(""), &out, &errb, f.env(t)); code != 0 || errb.String() != usage+"\n" || out.Len() != 0 {
		t.Fatalf("-h: %d %q %q", code, out.String(), errb.String())
	}
}

// Nor from the environment: with an empty stdin there is no code.
func TestCodeNeverFromEnv(t *testing.T) {
	f := newFakeServer(t)
	for _, k := range []string{"GLOVO_LINK_CODE", "LINK_CODE", "CODE"} {
		t.Setenv(k, testCode)
	}
	r := runHelper(t, f, nil, "")
	if r.code == 0 || r.stdout != msgCodeRead+"\n" {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	if pv, _ := f.counts(); pv != 0 {
		t.Fatal("preview called without a code on stdin")
	}
}

func TestDeclineUploadsNothing(t *testing.T) {
	f := newFakeServer(t)
	p := writeSession(t, sessionJSON, 0o600)
	for _, answer := range []string{"n", "", "Y es", "no"} {
		r := runHelper(t, f, []string{"-session", p}, testCode+"\n"+answer+"\n")
		if r.code == 0 || r.stdout != msgDeclined+"\n" {
			t.Fatalf("answer %q: %d %q", answer, r.code, r.stdout)
		}
	}
	if _, up := f.counts(); up != 0 {
		t.Fatalf("%d uploads after declining", up)
	}
}

func TestHTTPSOnly(t *testing.T) {
	f := newFakeServer(t)
	for _, s := range []string{"http://glovo.mctl.ai", "glovo.mctl.ai", "https://u:p@glovo.mctl.ai", "https://glovo.mctl.ai/x", "https://glovo.mctl.ai?q=1"} {
		var out bytes.Buffer
		code := run([]string{"-server", s, "-client-id", testClient}, strings.NewReader(testCode+"\ny\n"), &out, io.Discard, f.env(t))
		if code != 2 || out.String() != msgServer+"\n" {
			t.Fatalf("%s: %d %q", s, code, out.String())
		}
	}
}

func TestSessionFileChecks(t *testing.T) {
	f := newFakeServer(t)
	open := writeSession(t, sessionJSON, 0o644)
	group := writeSession(t, sessionJSON, 0o640)
	link := filepath.Join(t.TempDir(), "link.json")
	if err := os.Symlink(writeSession(t, sessionJSON, 0o600), link); err != nil {
		t.Fatal(err)
	}
	noRefresh := writeSession(t, `{"accessToken":"MARKER-A","deviceUrn":"d"}`, 0o600)
	notJSON := writeSession(t, `MARKER-not-json`, 0o600)
	big := writeSession(t, `{"accessToken":"`+strings.Repeat("a", maxSession)+`"}`, 0o600)
	for _, tc := range []struct{ path, want string }{
		{open, msgSessionMode},
		{group, msgSessionMode},
		{link, msgSessionMode},
		{t.TempDir(), msgSessionMode},
		{filepath.Join(t.TempDir(), "missing.json"), msgSessionRead},
		{noRefresh, msgSessionShape},
		{notJSON, msgSessionRead},
		{big, refusals["request_too_large"]},
	} {
		r := runHelper(t, f, []string{"-session", tc.path}, testCode+"\ny\n")
		if r.code == 0 || r.stdout != tc.want+"\n" {
			t.Errorf("%s: %d %q, want %q", tc.path, r.code, r.stdout, tc.want)
		}
		noMarkers(t, r)
	}
	if _, up := f.counts(); up != 0 {
		t.Fatalf("%d uploads of a refused session", up)
	}
	// The helper never changes the file's mode.
	if st, _ := os.Stat(open); st.Mode().Perm() != 0o644 {
		t.Fatal("helper changed the session file mode")
	}
}

// The server's message is never printed: only the helper's own text for a
// known code, else one generic refusal.
func TestServerRefusalsAreFixedTexts(t *testing.T) {
	f := newFakeServer(t)
	p := writeSession(t, sessionJSON, 0o600)
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{422, `{"error":"session_refused","message":"MARKER-hostile"}`, refusals["session_refused"]},
		{400, `{"error":"code_refused","message":"MARKER-hostile"}`, refusals["code_refused"]},
		{500, `{"error":"MARKER-unknown"}`, msgServerUnknown},
		{302, ``, msgServerUnknown},
		{200, `{"result":"MARKER-other"}`, msgServerUnknown},
	} {
		f.mu.Lock()
		f.upload = func(w http.ResponseWriter) {
			if tc.status == 302 {
				w.Header().Set("Location", "https://example.invalid/steal")
			}
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}
		f.mu.Unlock()
		r := runHelper(t, f, []string{"-session", p}, testCode+"\ny\n")
		if r.code == 0 || r.stdout != tc.want+"\n" {
			t.Errorf("%d: %q, want %q", tc.status, r.stdout, tc.want)
		}
		noMarkers(t, r)
	}
}

func TestBadCodeShapeNeverLeavesTheMachine(t *testing.T) {
	f := newFakeServer(t)
	r := runHelper(t, f, nil, "short\n")
	if r.code == 0 || r.stdout != msgCodeShape+"\n" {
		t.Fatalf("%d %q", r.code, r.stdout)
	}
	if pv, _ := f.counts(); pv != 0 {
		t.Fatal("a malformed code was sent")
	}
}

// --- unlink --------------------------------------------------------------

func TestUnlinkAfterFreshLoginAndConfirmation(t *testing.T) {
	f := newFakeServer(t)
	r := runHelper(t, f, []string{"-unlink"}, "y\n")
	if r.code != 0 || r.stdout != msgUnlinked+"\n" {
		t.Fatalf("%d %q %q", r.code, r.stdout, r.stderr)
	}
	f.mu.Lock()
	n, auths := f.unlinks, append([]string(nil), f.auths...)
	f.mu.Unlock()
	tok := f.idp.issuedToken()
	if n != 1 || tok == "" || len(auths) != 1 || auths[0] != "Bearer "+tok {
		t.Fatalf("unlinks %d, auths %d", n, len(auths))
	}
	// It reads no code and never the session file.
	if strings.Contains(r.stderr, "link code") {
		t.Fatalf("unlink asked for a code: %q", r.stderr)
	}
}

func TestUnlinkDeclinedSendsNothing(t *testing.T) {
	f := newFakeServer(t)
	for _, answer := range []string{"n", "", "Y es", "no"} {
		r := runHelper(t, f, []string{"-unlink"}, answer+"\n")
		if r.code == 0 || r.stdout != msgNotUnlinked+"\n" {
			t.Fatalf("answer %q: %d %q", answer, r.code, r.stdout)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unlinks != 0 || f.total != 0 {
		t.Fatalf("declined unlink reached the server: %d/%d", f.unlinks, f.total)
	}
}

func TestUnlinkServerRefusalIsAFixedText(t *testing.T) {
	f := newFakeServer(t)
	f.freshRequired = true
	r := runHelper(t, f, []string{"-unlink"}, "y\n")
	if r.code == 0 || r.stdout != refusals["fresh_login_required"]+"\n" || strings.Contains(r.stdout+r.stderr, "MARKER") {
		t.Fatalf("%d %q %q", r.code, r.stdout, r.stderr)
	}
}

func TestUnlinkOnlyToTheLinkOrigin(t *testing.T) {
	// The token is for the link server only: the route check admits the
	// unlink path and nothing outside /link.
	if !linkRoute("https://glovo.mctl.ai/link/unlink") || linkRoute("https://glovo.mctl.ai/link/unlink?x=1") || linkRoute("https://glovo.mctl.ai/mcp") {
		t.Fatal("linkRoute admits the wrong paths")
	}
}
