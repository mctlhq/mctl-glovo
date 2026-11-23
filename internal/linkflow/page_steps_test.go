package linkflow

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The link page has to be followable by someone who has never heard of the
// helper: the helper does not log in to Glovo, it uploads a session file that
// glovo-skill makes, so the page names every step and the values to type.
func TestLinkPageSaysHowToLink(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/link", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"password or an email code",
		"cannot be linked yet",
		SkillURL,
		"node scripts/login.mjs",
		"not with the Google button",
		"go install github.com/mctlhq/mctl-glovo/cmd/glovo-link@" + HelperVersion,
		"glovo-link -client-id " + HelperClientID,
		"about 20 minutes",
		"glovo-link -unlink -client-id " + HelperClientID,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/link does not say %q", want)
		}
	}
}

// The helper version on the page is the release version. release-please bumps
// both in the same release; a hand edit of one without the other fails here.
func TestHelperVersionIsTheReleaseVersion(t *testing.T) {
	raw, err := os.ReadFile("../../.release-please-manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["."] != HelperVersion {
		t.Fatalf("HelperVersion %q differs from the release manifest %q", HelperVersion, m["."])
	}
}
