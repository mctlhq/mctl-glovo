package glovoapi

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode %o, want %o", path, got, want)
	}
}

func TestSaveSessionIsAtomicAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "glovo")
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- deliberately too open; SaveSession must tighten it
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(`{"accessToken":"old"}`), 0o644); err != nil { // #nosec G306 -- deliberately too open
		t.Fatal(err)
	}
	s := testSession()
	if err := SaveSession(path, s, testNow); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o600)
	assertMode(t, dir, 0o700)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
	got, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["accessToken"] != validToken || got["keepMe"] != "unknown-field" || got["updatedAt"] != "2026-03-01T12:00:00.000Z" {
		t.Errorf("round trip: %v", got)
	}
	if _, stamped := s["updatedAt"]; stamped {
		t.Error("SaveSession mutated the caller's session")
	}
}

func TestSaveSessionFailureKeepsOldFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "glovo")
	path := filepath.Join(dir, "session.json")
	if err := SaveSession(path, Session{"accessToken": "old"}, testNow); err != nil {
		t.Fatal(err)
	}
	if err := SaveSession(path, Session{"bad": func() {}}, testNow); err == nil {
		t.Fatal("unencodable session saved")
	}
	got, err := LoadSession(path)
	if err != nil || got["accessToken"] != "old" {
		t.Fatalf("old session lost: %v %v", got, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestLoadSessionSecuresPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "glovo")
	if err := os.MkdirAll(dir, 0o755); err != nil { // #nosec G301 -- deliberately too open
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(`{"refreshToken":"r"}`), 0o644); err != nil { // #nosec G306 -- deliberately too open
		t.Fatal(err)
	}
	if _, err := LoadSession(path); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o600)
	assertMode(t, dir, 0o700)
}

func TestLoadSessionRejectsBadFiles(t *testing.T) {
	cases := map[string]struct {
		body string
		want error
	}{
		"not json":         {`{"accessToken":`, ErrSessionMalformed},
		"array":            {`[]`, ErrSessionMalformed},
		"non-string token": {`{"accessToken":{"x":1}}`, ErrSessionMalformed},
		"numeric refresh":  {`{"refreshToken":42}`, ErrSessionMalformed},
		"empty object":     {`{}`, ErrNoSessionData},
		"empty values":     {`{"accessToken":"","location":null}`, ErrNoSessionData},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadSession(path); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := LoadSession(filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
}

func TestReload(t *testing.T) {
	f := newFake(t)
	c, path := newTestClient(t, f, nil)
	if c.Session() != nil {
		t.Fatal("missing file produced a session")
	}
	if err := os.WriteFile(path, []byte(`{"accessToken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); !errors.Is(err, ErrSessionMalformed) {
		t.Fatalf("malformed file: %v", err)
	}
	if err := SaveSession(path, testSession(), testNow); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(); err != nil {
		t.Fatal(err)
	}
	st := c.AuthStatus()
	if !st.SignedIn || !st.AccessTokenValid || !st.HasCustomer || !st.HasLocation || st.SecondsLeft != 3600 {
		t.Errorf("status %+v", st)
	}
}

func TestTokenStatus(t *testing.T) {
	if st := tokenStatus(Session{"accessToken": expiredToken}, testNow); st.Valid || st.SecondsLeft != -60 {
		t.Errorf("expired: %+v", st)
	}
	opaque := Session{"accessToken": "opaque", "createdAt": msNumber(testNow.Add(-30 * 60e9)), "expiresIn": 3600.0}
	if st := tokenStatus(opaque, testNow); !st.Valid || st.SecondsLeft != 1800 {
		t.Errorf("createdAt+expiresIn: %+v", st)
	}
	if st := tokenStatus(Session{"accessToken": "opaque"}, testNow); st.Valid {
		t.Errorf("no expiry info: %+v", st)
	}
	if !strings.HasPrefix(validToken, "Bearer ") || decodeJWTPayload(validToken) == nil {
		t.Error("test JWT does not decode")
	}
}
