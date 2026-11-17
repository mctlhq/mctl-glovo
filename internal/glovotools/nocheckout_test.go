package glovotools

import (
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/mctlhq/mctl-glovo/internal/childmcp"
)

// hostedToolSet is the exact set of tool names a hosted backend serves. It
// is pinned: adding or removing a tool is a conscious change to this list,
// made together with a review of what the tool can do on a real account.
// Basket writes only; nothing here checks out, pays or places an order.
var hostedToolSet = []string{
	"glovo_add_to_basket",
	"glovo_analyze_order_history",
	"glovo_auth_status",
	"glovo_browse_store_catalog",
	"glovo_browse_stores",
	"glovo_get_basket",
	"glovo_get_location",
	"glovo_get_order_items",
	"glovo_get_order_stats",
	"glovo_get_product",
	"glovo_get_purchase_history",
	"glovo_get_saved_locations",
	"glovo_get_shopping_guide",
	"glovo_get_store",
	"glovo_get_store_menu",
	"glovo_get_store_order_options",
	"glovo_get_store_recommendations",
	"glovo_get_suggestions",
	"glovo_login",
	"glovo_plan_reorder",
	"glovo_preview_reorder",
	"glovo_remove_from_basket",
	"glovo_search_locations",
	"glovo_search_store_items",
	"glovo_select_location",
	"glovo_set_location",
	"glovo_set_quantity",
}

// forbiddenWords are checkout, payment and order-placement words. This list
// is deliberately independent of childmcp.Blocked, so one cannot be loosened
// without this test noticing. Reading words (order_items, purchase_history,
// preview_reorder) are allowed only through hostedToolSet.
var forbiddenWords = []string{
	"checkout", "payment", "pay", "paying", "charge", "buy", "place", "placeorder",
	"submit", "submitorder", "confirm", "finalize", "finalise", "execute", "ordernow",
}

func words(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func TestHostedServesExactlyThePinnedNonCheckoutToolSet(t *testing.T) {
	e := newHostedEnv(t)
	got := append([]string(nil), e.b.Names()...)
	sort.Strings(got)
	want := append([]string(nil), hostedToolSet...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("hosted tool set changed.\n got: %v\nwant: %v", got, want)
	}
	// The MCP surface (ServerTools) is the same set as Names.
	var served []string
	for _, st := range e.b.ServerTools() {
		served = append(served, st.Tool.Name)
	}
	sort.Strings(served)
	if strings.Join(served, ",") != strings.Join(want, ",") {
		t.Fatalf("ServerTools differ from Names: %v", served)
	}
	for _, n := range got {
		if childmcp.Blocked(n) {
			t.Errorf("%s is blocked by childmcp.Blocked but served", n)
		}
		for _, w := range words(n) {
			for _, f := range forbiddenWords {
				if w == f {
					t.Errorf("%s contains forbidden word %q", n, f)
				}
			}
		}
		if strings.Contains(strings.ToLower(n), "checkout") || strings.Contains(strings.ToLower(n), "payment") {
			t.Errorf("%s names a checkout or payment action", n)
		}
	}
}

// The forbidden-word check itself must bite: the names it is meant to catch
// are caught by both lists.
func TestForbiddenWordsCatchCheckoutNames(t *testing.T) {
	for _, n := range []string{"glovo_checkout", "glovo_place_order", "glovo_submit_basket", "glovo_confirm_order", "glovo_pay", "glovo_make_payment"} {
		hit := childmcp.Blocked(n)
		ws := words(n)
		found := false
		for _, w := range ws {
			for _, f := range forbiddenWords {
				found = found || w == f
			}
		}
		if !hit || !found {
			t.Errorf("%s: Blocked=%v forbiddenWords=%v", n, hit, found)
		}
	}
}

// glovo_login is served for schema parity but never starts a browser login
// for a hosted caller.
func TestHostedLoginIsRefused(t *testing.T) {
	e := newHostedEnv(t)
	res, err := e.b.Call(pidCtx("prn_A"), "glovo_login", map[string]any{})
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("got res=%v err=%v", res, err)
	}
	if e.fake.count() != 0 {
		t.Fatal("glovo_login reached Glovo")
	}
}

// The landing page states the tool count from childmcp.KnownTools. The hosted
// set must stay equal to that list, or the page's figure would describe a
// different set from the one served.
func TestHostedToolSetEqualsKnownTools(t *testing.T) {
	a := append([]string(nil), hostedToolSet...)
	b := append([]string(nil), childmcp.KnownTools...)
	sort.Strings(a)
	sort.Strings(b)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Fatalf("hostedToolSet differs from childmcp.KnownTools:\n%v\n%v", a, b)
	}
}
