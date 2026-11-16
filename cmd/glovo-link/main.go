// Command glovo-link links a Glovo account to an MCTL account (slice C2-1b,
// docs/design/token-store.md 4.1 steps 3-6).
//
// It reads the one-time link code from stdin, never from an argument or the
// environment, where shell history and process listings would keep it. It
// shows the masked MCTL identity the code belongs to and asks before it
// reads the Glovo session. It uploads over https only and prints "linked"
// or one fixed refusal; it never prints the session.
//
// Capturing a fresh Glovo login is not part of this helper: it uploads a
// session that already exists (by default ~/.glovo/session.json, the file
// glovo-skill and the remote-login prototype of PR #4 write).
//
// Usage:
//
//	glovo-link [-server https://glovo.mctl.ai] [-session PATH | -session -]
//
// With -session -, stdin carries the code line, then the y/N answer line,
// then the session JSON.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	defaultServer = "https://glovo.mctl.ai"
	uploadSchema  = "glovo-link-upload/v1"
	previewSchema = "glovo-link-preview/v1"
	maxSession    = 16 << 10
	maxAnswer     = 4 << 10
)

// Fixed texts. stdout only ever carries msgLinked or one refusal.
const (
	msgLinked        = "linked"
	msgArgs          = "refused: glovo-link takes no arguments; it reads the link code from stdin"
	msgServer        = "refused: -server must be an https:// origin"
	msgCodeRead      = "refused: no link code on stdin"
	msgCodeShape     = "refused: that is not a link code"
	msgDeclined      = "not linked: you declined"
	msgSessionRead   = "refused: the Glovo session could not be read"
	msgSessionMode   = "refused: the session file must be a regular file readable only by you (chmod 600)"
	msgSessionShape  = "refused: the Glovo session has no access token, refresh token or device id"
	msgUnreachable   = "refused: the link server could not be reached"
	msgServerUnknown = "refused: the link server refused the request"
)

// refusals maps the server's stable error codes to this helper's own
// texts: the server's message is never printed.
var refusals = map[string]string{
	"code_refused":      "refused: the link code is invalid, used or expired; request a new code",
	"session_refused":   "refused: Glovo did not accept the session; sign in to Glovo again and request a new code",
	"rate_limited":      "refused: too many link attempts; try again later",
	"linking_disabled":  "refused: linking is switched off",
	"unavailable":       "refused: linking is unavailable right now; request a new code and try again later",
	"request_too_large": "refused: the session is larger than 16 KiB",
	"malformed_request": "refused: the link server did not understand the request",
}

func main() {
	home, _ := os.UserHomeDir()
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, newHTTPClient(nil), home))
}

func newHTTPClient(roots *tls.Config) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if roots != nil {
		tr.TLSClientConfig = roots
	}
	return &http.Client{
		Transport: tr,
		Timeout:   60 * time.Second,
		// A redirect could carry the session somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer, hc *http.Client, home string) int {
	fs := flag.NewFlagSet("glovo-link", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", defaultServer, "link server origin (https only)")
	sessionPath := fs.String("session", "", "Glovo session file, or - for stdin (default ~/.glovo/session.json)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		// Whatever it is, it may be the code: never echo it.
		_, _ = fmt.Fprintln(stdout, msgArgs)
		return 2
	}
	base, ok := checkServer(*server)
	if !ok {
		_, _ = fmt.Fprintln(stdout, msgServer)
		return 2
	}
	if *sessionPath == "" {
		*sessionPath = filepath.Join(home, ".glovo", "session.json")
	}

	in := bufio.NewReader(stdin)
	_, _ = fmt.Fprint(stderr, "Paste the link code: ")
	code, ok := readLine(in)
	if !ok || code == "" {
		_, _ = fmt.Fprintln(stdout, msgCodeRead)
		return 1
	}
	if !looksLikeCode(code) {
		_, _ = fmt.Fprintln(stdout, msgCodeShape)
		return 1
	}

	ctx := context.Background()
	var preview struct {
		Identity string `json:"identity"`
	}
	if msg := post(ctx, hc, base+"/link/preview", map[string]any{"schema": previewSchema, "code": code}, &preview); msg != "" {
		_, _ = fmt.Fprintln(stdout, msg)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "\nThis code belongs to the MCTL account %s.\n", printable(preview.Identity))
	_, _ = fmt.Fprint(stderr, "Link your Glovo account to this MCTL account? [y/N] ")
	answer, _ := readLine(in)
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		_, _ = fmt.Fprintln(stdout, msgDeclined)
		return 1
	}

	var sess map[string]any
	var msg string
	if *sessionPath == "-" {
		sess, msg = parseSession(io.LimitReader(in, maxSession+1))
	} else {
		sess, msg = readSessionFile(*sessionPath)
	}
	if msg != "" {
		_, _ = fmt.Fprintln(stdout, msg)
		return 1
	}
	var out struct {
		Result string `json:"result"`
	}
	if msg := post(ctx, hc, base+"/link/upload", map[string]any{"schema": uploadSchema, "code": code, "session": sess}, &out); msg != "" {
		_, _ = fmt.Fprintln(stdout, msg)
		return 1
	}
	if out.Result != "linked" {
		_, _ = fmt.Fprintln(stdout, msgServerUnknown)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, msgLinked)
	return 0
}

// checkServer accepts https://host[:port] and nothing else.
func checkServer(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	return "https://" + u.Host, true
}

func readLine(r *bufio.Reader) (string, bool) {
	line, err := r.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// looksLikeCode is 32 bytes of base64url without padding: 43 characters.
func looksLikeCode(s string) bool {
	if len(s) != 43 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// printable bounds what the server can make the terminal show.
func printable(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= 128 {
			break
		}
		if unicode.IsPrint(r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "(unknown)"
	}
	return b.String()
}

// readSessionFile opens the file without following a symlink and refuses
// one that others can read. It never changes the file.
func readSessionFile(path string) (map[string]any, string) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- the user's own session path
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, msgSessionMode
		}
		return nil, msgSessionRead
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, msgSessionRead
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return nil, msgSessionMode
	}
	return parseSession(io.LimitReader(f, maxSession+1))
}

func parseSession(r io.Reader) (map[string]any, string) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, msgSessionRead
	}
	if len(raw) > maxSession {
		return nil, refusals["request_too_large"]
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var sess map[string]any
	if err := dec.Decode(&sess); err != nil || sess == nil {
		return nil, msgSessionRead
	}
	for _, k := range []string{"accessToken", "refreshToken", "deviceUrn"} {
		if v, ok := sess[k].(string); !ok || v == "" {
			return nil, msgSessionShape
		}
	}
	return sess, ""
}

// post sends body and decodes a 200 answer into out. It returns "" on
// success or the fixed refusal to print.
func post(ctx context.Context, hc *http.Client, u string, body any, out any) string {
	raw, err := json.Marshal(body)
	if err != nil {
		return msgSessionRead
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return msgUnreachable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return msgUnreachable
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return msgUnreachable
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil {
			if m, ok := refusals[e.Error]; ok {
				return m
			}
		}
		return msgServerUnknown
	}
	if err := json.Unmarshal(data, out); err != nil {
		return msgServerUnknown
	}
	return ""
}
