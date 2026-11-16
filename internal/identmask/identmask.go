// Package identmask holds the two small helpers that keep a person's
// identity out of places that outlive a request.
//
// Email masks an email address for the account-link preview (token-store
// design 4.1, step 3): the user must recognise their own account, and
// nobody else should be able to read it off a screen or a log. Pseudonym
// gives a stable, keyed, non-reversible stand-in for a principal id, so log
// and audit lines of one user correlate without naming the principal.
//
// Both fail closed. Input that is not clearly what the helper expects is an
// error, never an empty string, a partial mask or an unkeyed hash: a caller
// that cannot mask must refuse, not print.
package identmask

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrNotEmail is returned by Email for anything that is not one plain
// address. It never carries the input.
var ErrNotEmail = errors.New("identmask: not a single email address")

// ErrKey is returned when the pseudonym key is missing or too short.
var ErrKey = errors.New("identmask: pseudonym key must be at least 16 bytes")

// ErrID is returned when there is no id to pseudonymise.
var ErrID = errors.New("identmask: empty id")

// MinKeyLen is the shortest accepted pseudonym key.
const MinKeyLen = 16

// Unknown is what Pseudonymizer.Of returns when it cannot pseudonymise. It
// is a fixed marker, not a hash, so it can never be mistaken for a user.
const Unknown = "pn_unknown"

// Email masks an address to its first rune, "***", the first rune of the
// domain's first label and its last label: "alice@mail.example.com" gives
// "a***@m***.com". Surrounding whitespace is trimmed. Anything that is not
// exactly one address (no or several "@", empty parts, empty labels,
// whitespace or control characters inside) is ErrNotEmail.
func Email(s string) (string, error) {
	s = strings.TrimSpace(s)
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") ||
		!utf8.ValidString(s) || strings.ContainsFunc(s, bad) || strings.ContainsAny(s, ",;<>") {
		return "", ErrNotEmail
	}
	labels := strings.Split(domain, ".")
	for _, l := range labels {
		if l == "" {
			return "", ErrNotEmail
		}
	}
	out := first(local) + "***@" + first(labels[0]) + "***"
	if len(labels) > 1 {
		out += "." + labels[len(labels)-1]
	}
	return out, nil
}

func bad(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }

func first(s string) string {
	r, _ := utf8.DecodeRuneInString(s)
	return string(r)
}

// Pseudonym returns "pn_" plus 64 bits of HMAC-SHA256(key, id) in hex. The
// same key and id always give the same value; without the key it cannot be
// recomputed from a known id, and it does not contain the id. 64 bits keep
// collisions negligible for a cohort of tens while staying short in a log.
func Pseudonym(key []byte, id string) (string, error) {
	if len(key) < MinKeyLen {
		return "", ErrKey
	}
	if id == "" {
		return "", ErrID
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id))
	return "pn_" + hex.EncodeToString(m.Sum(nil)[:8]), nil
}

// Pseudonymizer binds a key once, so call sites in a logger cannot forget
// it and cannot print it. The zero value is valid and answers Unknown.
type Pseudonymizer struct{ key []byte }

// NewPseudonymizer copies key. It refuses a key shorter than MinKeyLen.
func NewPseudonymizer(key []byte) (*Pseudonymizer, error) {
	if len(key) < MinKeyLen {
		return nil, ErrKey
	}
	return &Pseudonymizer{key: append([]byte(nil), key...)}, nil
}

// Of returns the pseudonym of id, or Unknown when it cannot compute one. It
// is safe to pass straight to a logger.
func (p *Pseudonymizer) Of(id string) string {
	if p == nil {
		return Unknown
	}
	s, err := Pseudonym(p.key, id)
	if err != nil {
		return Unknown
	}
	return s
}

// String never reveals the key.
func (p *Pseudonymizer) String() string { return "identmask.Pseudonymizer" }

// GoString never reveals the key either (%#v).
func (p *Pseudonymizer) GoString() string { return p.String() }
