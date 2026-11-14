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
	for i, w := range words {
		prev, next := "", ""
		if i > 0 {
			prev = words[i-1]
		}
		if i+1 < len(words) {
			next = words[i+1]
		}
		switch w {
		case "pay", "paying", "charge", "buy", "ordernow":
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
		}
		if orderVerbs[w] && orderObjects[next] {
			return true
		}
	}
	return false
}

var orderVerbs = map[string]bool{
	"place": true, "submit": true, "confirm": true, "create": true, "complete": true,
	"finalize": true, "finalise": true, "send": true, "execute": true, "make": true,
}

var orderObjects = map[string]bool{
	"order": true, "orders": true, "purchase": true, "basket": true, "cart": true,
}
