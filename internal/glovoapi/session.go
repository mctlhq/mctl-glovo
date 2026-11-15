package glovoapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Session is the decoded glovo-skill session file (src/auth/store.mjs). It is
// kept as a generic JSON object so fields this package does not know about
// survive a refresh and are written back unchanged.
//
// Known fields: accessToken, refreshToken, expiresIn, tokenType, scope,
// createdAt (Unix milliseconds), customerId or customer.id, location,
// deviceUrn, perseusClientId, perseusSessionId, perseusSessionTimestamp and
// updatedAt.
type Session map[string]any

// CustomerID is `customerId || customer?.id` as a string, or "".
func (s Session) CustomerID() string {
	v := or(Value(s["customerId"]), get(map[string]any(s), "customer", "id"))
	if !truthy(v) {
		return ""
	}
	return jsString(v)
}

// ErrSessionMalformed is returned for a session file that is not a valid
// glovo-skill session.
var ErrSessionMalformed = errors.New("malformed Glovo session; run glovo_login again")

// ErrNoSessionData is returned when the session has no token and no location.
var ErrNoSessionData = errors.New("no Glovo token or location in session; run glovo_login first")

// LoadSession reads a session file the way loadSession in store.mjs does: it
// secures the directory to 0700 and the file to 0600, then validates the
// token fields.
func LoadSession(path string) (Session, error) {
	if err := secureSessionDir(path); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secure session file: %w", err)
		}
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the session path is operator configuration
	if err != nil {
		return nil, err
	}
	v, err := decodeJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSessionMalformed, err)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, ErrSessionMalformed
	}
	s := Session(m)
	access, refresh, loc := Value(s["accessToken"]), Value(s["refreshToken"]), Value(s["location"])
	if !truthy(access) && !truthy(refresh) && !truthy(loc) {
		return nil, ErrNoSessionData
	}
	if tok := or(access, refresh); truthy(tok) {
		if _, isString := tok.(string); !isString {
			return nil, ErrSessionMalformed
		}
	}
	return s, nil
}

// SaveSession writes the session atomically with mode 0600 and stamps
// updatedAt, like saveSession in store.mjs.
func SaveSession(path string, s Session, now time.Time) error {
	out := make(map[string]any, len(s)+1)
	for k, v := range s {
		out[k] = v
	}
	out["updatedAt"] = now.UTC().Format("2006-01-02T15:04:05.000Z")
	return savePrivateJSON(path, out)
}

func savePrivateJSON(path string, value any) error {
	if err := secureSessionDir(path); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	tmp := fmt.Sprintf("%s.%d.%d.tmp", path, os.Getpid(), time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- derived from the configured session path
	if err != nil {
		return fmt.Errorf("create session temp file: %w", err)
	}
	cleanup := func(cause error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return cause
	}
	if _, err := f.Write(data); err != nil {
		return cleanup(fmt.Errorf("write session: %w", err))
	}
	if err := f.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync session: %w", err))
	}
	if err := f.Chmod(0o600); err != nil {
		return cleanup(fmt.Errorf("chmod session: %w", err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close session: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace session: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod session: %w", err)
	}
	return nil
}

func secureSessionDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("stat session directory: %w", err)
	}
	if info.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil { // #nosec G302 -- directory needs the execute bit
			return fmt.Errorf("secure session directory: %w", err)
		}
	}
	return nil
}

var bearerPrefix = regexp.MustCompile(`(?i)^Bearer\s+`)

// decodeJWTPayload mirrors decodeJwt: the JSON payload of a JWT, or nil.
func decodeJWTPayload(token string) map[string]any {
	raw := bearerPrefix.ReplaceAllString(token, "")
	parts := strings.Split(raw, ".")
	if len(parts) < 2 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	v, err := decodeJSON(b)
	if err != nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// TokenStatus mirrors tokenStatus in store.mjs.
type TokenStatus struct {
	Valid       bool
	SecondsLeft float64
}

func tokenStatus(s Session, now time.Time) TokenStatus {
	if s == nil {
		return TokenStatus{}
	}
	tok := or(Value(s["accessToken"]), Value(s["access_token"]))
	if !truthy(tok) {
		return TokenStatus{}
	}
	nowMs := float64(now.UnixMilli())
	var left float64
	claims := decodeJWTPayload(jsString(tok))
	if exp := get(claims, "exp"); claims != nil && truthy(exp) {
		left = jsNumber(exp) - math.Floor(nowMs/1000)
	} else if created, expires := Value(s["createdAt"]), Value(s["expiresIn"]); truthy(created) && truthy(expires) {
		left = math.Floor((jsNumber(created) + jsNumber(expires)*1000 - nowMs) / 1000)
	}
	if math.IsNaN(left) {
		left = 0
	}
	return TokenStatus{Valid: left > 0, SecondsLeft: left}
}

func msNumber(t time.Time) json.Number {
	return json.Number(strconv.FormatInt(t.UnixMilli(), 10))
}
