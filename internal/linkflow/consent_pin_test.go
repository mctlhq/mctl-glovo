package linkflow

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// consentPins maps each ConsentVersion to the sha256 of its text. Changing a
// word of ConsentText without a new ConsentVersion (and a new line here) is a
// consent to a text the user was not shown, so it fails.
var consentPins = map[string]string{
	"c2-link-v2": "d03711fc253b87b764ff3a7aa9cd3d92f5da7f5ff95c2d48b536971ba932ef9a",
}

func consentTextHash() string {
	sum := sha256.Sum256([]byte(strings.Join(ConsentText, "\n")))
	return hex.EncodeToString(sum[:])
}

func TestConsentTextIsPinnedToItsVersion(t *testing.T) {
	want, ok := consentPins[ConsentVersion]
	if !ok {
		t.Fatalf("ConsentVersion %q has no pinned text hash; add %q", ConsentVersion, consentTextHash())
	}
	if got := consentTextHash(); got != want {
		t.Fatalf("ConsentText changed under version %q (hash %s, pinned %s): bump ConsentVersion, mctl-api's version and this pin together", ConsentVersion, got, want)
	}
}
