package childmcp

import (
	"strings"
	"unicode"
)

// Blocked reports whether a tool name suggests checkout, payment or order
// placement. glovo-skill exposes no such tool; this is defence in depth so a
// future upstream tool of that kind is never mirrored to remote callers.
//
// The rule works on name words (split on anything not a letter or digit) so
// read-only tools such as glovo_get_purchase_history, glovo_get_order_items
// or glovo_preview_reorder stay available.
func Blocked(name string) bool {
	lower := strings.ToLower(name)
	// Substrings catch camelCase and run-together names (GlovoCheckout).
	for _, sub := range []string{"checkout", "payment", "placeorder", "submitorder"} {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	words := strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	hasVerb, hasObject := false, false
	for i, w := range words {
		prev, next := "", ""
		if i > 0 {
			prev = words[i-1]
		}
		if i+1 < len(words) {
			next = words[i+1]
		}
		switch w {
		case "pay", "paying", "charge", "buy", "ordernow",
			"submit", "confirm", "finalize", "finalise", "place", "execute":
			return true
		case "purchase":
			// purchase_history is a read; any other purchase is not.
			if next != "history" {
				return true
			}
		case "reorder":
			// preview_reorder and plan_reorder only plan; a bare reorder
			// would place one.
			if prev != "preview" && prev != "plan" {
				return true
			}
		case "order", "orders":
			// order_items, order_stats, order_history and order_options are
			// reads; a bare order (glovo_order, glovo_order_now) is not.
			if !orderReads[next] {
				return true
			}
		}
		hasVerb = hasVerb || orderVerbs[w]
		hasObject = hasObject || orderObjects[w]
	}
	// A verb and an object in either order: create_order, basket_send.
	return hasVerb && hasObject
}

var orderVerbs = map[string]bool{
	"create": true, "complete": true, "send": true, "make": true, "process": true,
}

var orderObjects = map[string]bool{
	"order": true, "orders": true, "purchase": true, "basket": true, "cart": true,
}

var orderReads = map[string]bool{
	"items": true, "stats": true, "history": true, "options": true,
}

// KnownTools are the glovo-skill 0.2.1 tools the gateway mirrors. The
// mirror is an allowlist: a tool glovo-skill adds later is refused and
// logged until an operator lists it in GLOVO_HOME_EXTRA_TOOLS, and Blocked
// still applies to every name on either list.
var KnownTools = []string{
	"glovo_get_shopping_guide", "glovo_auth_status", "glovo_get_location", "glovo_set_location",
	"glovo_get_saved_locations", "glovo_search_locations", "glovo_select_location", "glovo_login",
	"glovo_browse_stores", "glovo_get_suggestions", "glovo_get_store", "glovo_get_store_menu",
	"glovo_browse_store_catalog", "glovo_get_store_recommendations", "glovo_get_store_order_options",
	"glovo_search_store_items", "glovo_get_product", "glovo_get_basket", "glovo_get_purchase_history",
	"glovo_get_order_items", "glovo_preview_reorder", "glovo_plan_reorder", "glovo_get_order_stats",
	"glovo_analyze_order_history", "glovo_add_to_basket", "glovo_set_quantity", "glovo_remove_from_basket",
}
