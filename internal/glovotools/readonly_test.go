package glovotools

import (
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/mctlhq/mctl-glovo/internal/childmcp"
)

// Plausible arguments for each basket write tool: enough to pass schema
// validation, so that a refusal can only come from the read-only gate.
var basketWriteArgs = map[string]map[string]any{
	"glovo_add_to_basket":      {"store_id": "1", "store_address_id": "2", "product_id": "3", "quantity": 1.0},
	"glovo_set_quantity":       {"store_id": "1", "basket_product_id": "bp", "quantity": 2.0},
	"glovo_remove_from_basket": {"store_id": "1", "basket_product_id": "bp"},
}

func listed(e *hostedEnv) map[string]bool {
	m := map[string]bool{}
	for _, st := range e.b.ServerTools() {
		m[st.Tool.Name] = true
	}
	return m
}

// Default config: the three write tools are not in tools/list.
func TestReadOnlyDefaultDoesNotRegisterBasketWriteTools(t *testing.T) {
	e := newHostedEnv(t)
	l := listed(e)
	for _, n := range BasketWriteTools {
		if l[n] {
			t.Errorf("%s is registered by default", n)
		}
	}
	for _, n := range e.b.Names() {
		if isBasketWriteTool(n) {
			t.Errorf("Names() lists %s by default", n)
		}
	}
	if len(BasketWriteTools) != 3 {
		t.Fatalf("BasketWriteTools = %v", BasketWriteTools)
	}
}

// Default config: a call by name is refused with the fixed text, and nothing
// followed it: no Glovo request and not even a client lookup.
func TestReadOnlyDefaultRefusesBasketWriteByNameBeforeAnyIO(t *testing.T) {
	e := newHostedEnv(t)
	for _, n := range BasketWriteTools {
		res, err := e.b.Call(pidCtx("prn_A"), n, basketWriteArgs[n])
		if err != nil || res == nil || !res.IsError {
			t.Fatalf("%s: res=%v err=%v, want an isError result", n, res, err)
		}
		if got := resultTextOf(res); got != BasketWritesRefusal {
			t.Errorf("%s: text %q, want the fixed refusal", n, got)
		}
	}
	if c := e.fake.count(); c != 0 {
		t.Fatalf("%d request(s) reached Glovo: %v", c, e.fake.methods())
	}
	if a := e.src.askedPIDs(); len(a) != 0 {
		t.Fatalf("client lookups happened: %v", a)
	}
}

// The refusal does not depend on the caller being authenticated or on valid
// arguments: it comes first.
func TestReadOnlyRefusalComesBeforeAuthAndValidation(t *testing.T) {
	e := newHostedEnv(t)
	res, err := e.b.Call(t.Context(), "glovo_add_to_basket", map[string]any{"quantity": "not a number"})
	if err != nil || res == nil || !res.IsError || resultTextOf(res) != BasketWritesRefusal {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if e.fake.count() != 0 || len(e.src.askedPIDs()) != 0 {
		t.Fatal("I/O happened")
	}
}

// Reading the basket, the history and the rest is unchanged by the switch
// being off: each reaches Glovo and only with GET (or the documented read
// POSTs), never a basket mutation.
func TestReadOnlyDefaultKeepsReadsWorking(t *testing.T) {
	e := newHostedEnv(t)
	reads := map[string]map[string]any{
		"glovo_get_basket":            {},
		"glovo_get_purchase_history":  {},
		"glovo_get_order_items":       {"order_id": "9003"},
		"glovo_preview_reorder":       {"order_id": "9003"},
		"glovo_get_order_stats":       {"max_pages": 1.0, "page_delay_ms": 0.0},
		"glovo_analyze_order_history": {"max_pages": 1.0, "page_delay_ms": 0.0, "detail_delay_ms": 0.0},
		"glovo_get_saved_locations":   {},
	}
	for n, a := range reads {
		before := e.fake.count()
		res, err := e.b.Call(pidCtx("prn_A"), n, a)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if res != nil && resultTextOf(res) == BasketWritesRefusal {
			t.Fatalf("%s: refused by the read-only gate", n)
		}
		if e.fake.count() == before {
			t.Errorf("%s: no request reached Glovo", n)
		}
	}
	for _, m := range e.fake.methods() {
		if strings.Contains(m, "/baskets") && !strings.HasPrefix(m, "GET ") {
			t.Errorf("a read tool sent %s", m)
		}
	}
	if !strings.Contains(strings.Join(e.fake.methods(), "\n"), "GET /v1/authenticated/customers/1/baskets") {
		t.Errorf("glovo_get_basket did not read the baskets: %v", e.fake.methods())
	}
}

// No served tool, in either mode, can check out, pay or place an order, and
// the writes the switch controls are exactly the basket tools.
func TestNoCheckoutPaymentOrderToolInAnyMode(t *testing.T) {
	for _, writes := range []bool{false, true} {
		e := newHostedEnvWith(t, writes)
		for _, n := range e.b.Names() {
			if childmcp.Blocked(n) {
				t.Errorf("writes=%v: %s is blocked but served", writes, n)
			}
			for _, w := range words(n) {
				for _, f := range forbiddenWords {
					if w == f {
						t.Errorf("writes=%v: %s contains %q", writes, n, f)
					}
				}
			}
		}
	}
	for _, n := range BasketWriteTools {
		if childmcp.Blocked(n) {
			t.Errorf("%s would be refused at New", n)
		}
	}
}

// With the switch on (and clients allowed to write), the tools are listed and
// a call reaches the basket endpoints: the switch is the only thing that
// changed. It does not add an approval step; there is none to skip here.
func TestBasketWritesSwitchOnServesAndForwards(t *testing.T) {
	e := newHostedEnvWith(t, true)
	l := listed(e)
	for _, n := range BasketWriteTools {
		if !l[n] {
			t.Errorf("%s not listed with the switch on", n)
		}
	}
	res, err := e.b.Call(pidCtx("prn_A"), "glovo_set_quantity", basketWriteArgs["glovo_set_quantity"])
	if err != nil || res == nil {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if resultTextOf(res) == BasketWritesRefusal {
		t.Fatal("refused with the switch on")
	}
	if e.fake.count() == 0 {
		t.Fatal("the call did not reach Glovo")
	}
}

// Backend switch on but the client not allowed to write (the second layer):
// the write is refused by the client before the network.
func TestBackendSwitchOnClientOffStillRefuses(t *testing.T) {
	e := newHostedEnvWith(t, false)
	b, err := New(Config{Clients: e.src, BasketWrites: true})
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Call(pidCtx("prn_A"), "glovo_add_to_basket", basketWriteArgs["glovo_add_to_basket"])
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("res=%v err=%v", res, err)
	}
	for _, m := range e.fake.methods() {
		if strings.Contains(m, "/baskets") && !strings.HasPrefix(m, "GET ") {
			t.Errorf("a basket write reached Glovo: %s", m)
		}
	}
}

func resultTextOf(r *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
