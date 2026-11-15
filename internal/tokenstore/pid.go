package tokenstore

import (
	"crypto/rand"
	"encoding/base32"
	"regexp"

	"github.com/mctlhq/mctl-glovo/internal/authn"
)

// PID is a principal id taken from a verified mctl-api token. Its field is
// unexported: the only constructor outside this package is PIDFrom, so a
// string from a tool argument, header or query cannot become a PID.
type PID struct{ id string }

// pidPattern bounds what a pid may look like. It must never contain "|",
// the separator of the transit context.
var pidPattern = regexp.MustCompile(`^prn_[A-Za-z0-9_-]{1,120}$`)

// PIDFrom returns the PID of a verified principal. A nil principal or an id
// that is not a well-formed prn_ id gives the zero PID, which every store
// method refuses.
func PIDFrom(p *authn.Principal) PID {
	if p == nil {
		return PID{}
	}
	return pidFromString(p.ID)
}

// pidFromString is for ids this package read itself (a link code row).
func pidFromString(s string) PID {
	if !pidPattern.MatchString(s) {
		return PID{}
	}
	return PID{id: s}
}

func (p PID) valid() bool { return p.id != "" }

// String returns the pid. Pids are pseudonymous ids that already appear in
// mctl-api's audit events; they are not secrets.
func (p PID) String() string { return p.id }

var idEncoding = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// newID returns prefix plus 128 random bits in the ULID alphabet (26
// characters). Ordering is not needed, so there is no timestamp part.
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; if it did,
		// continuing with a predictable id would be worse than stopping.
		panic("tokenstore: crypto/rand failed")
	}
	return prefix + idEncoding.EncodeToString(b[:])
}
