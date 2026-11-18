package glovologic

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The reason that a product has required options names a basket add only
// when a basket tool exists; in a read-only deployment it must not.
func TestReasonForReadOnlyDoesNotSuggestABasketAdd(t *testing.T) {
	s := &candidateStore{GlobalStore: &GlobalStore{Name: "Pizzeria"}, familiar: true, history: &Venue{OrderCount: 2}}
	product := Object{"name": "Margherita", "price": "9.00", "required_option_groups": 1}

	file := strings.Join(reasonFor(s, product, intent{}), "\n")
	if !strings.Contains(file, "before a basket add") {
		t.Fatalf("file mode lost its upstream wording: %q", file)
	}
	ro := strings.Join(reasonFor(s, product, intent{readOnly: true}), "\n")
	if strings.Contains(strings.ToLower(ro), "basket") {
		t.Fatalf("read-only reasons mention a basket: %q", ro)
	}
	if !strings.Contains(ro, "1 required option group(s) must be selected in the Glovo app") {
		t.Fatalf("read-only reasons lost the option hint: %q", ro)
	}
}

// SuggestOptions.ReadOnly reaches the served suggestions: every golden case
// run read-only carries the read-only wording and no basket-add hint, and the
// same run without the option still carries the upstream wording.
func TestGetSuggestionsReadOnlyServesNoBasketHint(t *testing.T) {
	f := fixture(t, "suggestions.json")
	want := golden(t, "suggestions.json")
	inputs := want["inputs"].(map[string]any)
	loc := f["location"]
	sess := SessionState{SignedIn: true, HasLocation: true, Location: Location{
		Latitude: get(loc, "latitude"), Longitude: get(loc, "longitude"), CityCode: jsString(get(loc, "cityCode")),
	}}
	var hint, readOnlyHint int
	for _, c := range f["cases"].([]any) {
		if get(c, "google") == "fake" {
			continue
		}
		for _, ro := range []bool{false, true} {
			got, err := GetSuggestions(context.Background(), &suggestionSource{t: t, f: f, inputs: inputs}, sess, suggestionInput(get(c, "input")), SuggestOptions{Sleep: noSleep, ReadOnly: ro})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			text := string(raw)
			if ro {
				if strings.Contains(text, "basket add") {
					t.Errorf("%v: read-only output names a basket add", get(c, "name"))
				}
				readOnlyHint += strings.Count(text, "in the Glovo app before ordering")
			} else {
				hint += strings.Count(text, "before a basket add")
			}
		}
	}
	if hint == 0 || readOnlyHint == 0 {
		t.Fatalf("fixture never reaches the option reason (file %d, read-only %d): the test proves nothing", hint, readOnlyHint)
	}
}
