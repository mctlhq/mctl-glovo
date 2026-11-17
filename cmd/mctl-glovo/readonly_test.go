package main

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/mctlhq/mctl-glovo/internal/glovotools"
)

var writeTools = glovotools.BasketWriteTools

// toolsList runs tools/list for a token and returns the names.
func (e *mcpEnv) toolsList(t *testing.T, tok, session string) []string {
	t.Helper()
	_, body := e.post(t, tok, session, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	var env struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body[strings.Index(body, "{"):]), &env); err != nil {
		t.Fatalf("tools/list: %v: %q", err, body)
	}
	var out []string
	for _, tl := range env.Result.Tools {
		out = append(out, tl.Name)
	}
	return out
}

// Default deployment: tools/list has no basket write tool, and none of the
// listed names can check out, pay or place an order.
func TestMCPDefaultIsReadOnlyToolsList(t *testing.T) {
	e := newMCPEnv(t, nil)
	session := e.open(t, "tok-a")
	names := e.toolsList(t, "tok-a", session)
	if len(names) == 0 {
		t.Fatal("tools/list is empty")
	}
	deny := regexp.MustCompile(`(?i)(checkout|payment|(^|_)pay(_|$)|place|submit|confirm|finali[sz]e|(^|_)buy(_|$)|(^|_)charge|ordernow|order_now|(^|_)order$)`)
	have := map[string]bool{}
	for _, n := range names {
		have[n] = true
		if strings.Contains(n, "reorder") && n != "glovo_plan_reorder" && n != "glovo_preview_reorder" {
			t.Errorf("tool %q is a reorder that is not a plan or a preview", n)
		}
		if deny.MatchString(n) {
			t.Errorf("tool %q matches the checkout/payment/order deny pattern", n)
		}
		for _, w := range writeTools {
			if n == w {
				t.Errorf("%s is listed by default", n)
			}
		}
	}
	for _, must := range []string{"glovo_get_basket", "glovo_get_purchase_history", "glovo_analyze_order_history", "glovo_get_order_items"} {
		if !have[must] {
			t.Errorf("read tool %s missing from the default list", must)
		}
	}
}

// Default deployment: a tools/call for each write tool by name is refused,
// also with a valid session, and not a single request reaches Glovo.
func TestMCPDefaultRefusesBasketWriteCallsBeforeProviderIO(t *testing.T) {
	e := newMCPEnv(t, nil)
	session := e.open(t, "tok-a")
	args := map[string]map[string]any{
		"glovo_add_to_basket":      {"store_id": "1", "store_address_id": "2", "product_id": "3", "quantity": 1},
		"glovo_set_quantity":       {"store_id": "1", "basket_product_id": "bp", "quantity": 2},
		"glovo_remove_from_basket": {"store_id": "1", "basket_product_id": "bp"},
	}
	for _, n := range writeTools {
		_, res, body := e.call(t, "tok-a", session, n, args[n])
		if res != nil && !res.IsError {
			t.Fatalf("%s: succeeded: %s", n, body)
		}
		if !strings.Contains(body, "error") {
			t.Fatalf("%s: neither a JSON-RPC error nor an isError result: %s", n, body)
		}
	}
	if got := len(e.fake.seen()); got != 0 {
		t.Fatalf("%d request(s) reached Glovo", got)
	}
	if asked := e.src.askedPIDs(); len(asked) != 0 {
		t.Fatalf("client lookups ran: %v", asked)
	}
}

// resources, prompts and completion are not served: the MCP surface is tools
// only, so no other route can carry a basket change.
func TestMCPSurfaceIsToolsOnly(t *testing.T) {
	e := newMCPEnv(t, nil)
	session := e.open(t, "tok-a")
	for _, m := range []string{"resources/list", "resources/read", "prompts/list", "prompts/get", "completion/complete"} {
		_, body := e.post(t, "tok-a", session, `{"jsonrpc":"2.0","id":9,"method":"`+m+`","params":{}}`)
		if strings.Contains(body, `"result"`) && !strings.Contains(body, `"error"`) {
			t.Errorf("%s answered with a result: %s", m, body)
		}
	}
	_, body := e.post(t, "tok-a", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	for _, c := range []string{`"resources"`, `"prompts"`, `"tasks"`} {
		if strings.Contains(body, c) {
			t.Errorf("initialize advertises %s: %s", c, body)
		}
	}
	if !strings.Contains(body, "read-only") || strings.Contains(body, "mutating the real basket") {
		t.Errorf("instructions do not say read-only: %s", body)
	}
}

// With the switch on the tools are listed again and reach the provider. The
// clients carry WithBasketWrites in production (c2.go); this test's fake
// clients do not, so the call is refused by that second layer, still before
// any basket write reaches the network.
func TestMCPSwitchOnListsToolsButClientLayerStillRefuses(t *testing.T) {
	e := newMCPEnvWith(t, nil, func(d *mcpDeps) { d.BasketWrites = true }, nil)
	session := e.open(t, "tok-a")
	have := map[string]bool{}
	for _, n := range e.toolsList(t, "tok-a", session) {
		have[n] = true
	}
	for _, n := range writeTools {
		if !have[n] {
			t.Errorf("%s not listed with the switch on", n)
		}
	}
	_, res, body := e.call(t, "tok-a", session, "glovo_remove_from_basket", map[string]any{"store_id": "1", "basket_product_id": "bp"})
	if res == nil || !res.IsError {
		t.Fatalf("the client layer did not refuse: %s", body)
	}
	if strings.Contains(resultText(res), "not enabled") {
		t.Fatalf("refused by the backend layer with the switch on: %s", body)
	}
	// The call made its basket read (GET); no write followed.
	if n := len(e.fake.seen()); n != 1 {
		t.Fatalf("%d request(s) reached Glovo, want only the basket read", n)
	}
}

func TestLoadBasketWrites(t *testing.T) {
	for _, tc := range []struct {
		v       string
		want    bool
		wantErr bool
	}{{"", false, false}, {"false", false, false}, {"true", true, false}, {"TRUE", false, true}, {"1", false, true}, {"yes", false, true}} {
		got, err := loadBasketWrites(env(map[string]string{"GLOVO_C2_BASKET_WRITES": tc.v}))
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%q: got %v, %v", tc.v, got, err)
		}
	}
	// Default of the whole C2 config: off.
	c, err := loadC2(env(fullEnv()))
	if err != nil || c.basketWrites {
		t.Fatalf("default C2 config: basketWrites=%v err=%v", c.basketWrites, err)
	}
	m := fullEnv()
	m["GLOVO_C2_BASKET_WRITES"] = "true"
	if c, err := loadC2(env(m)); err != nil || !c.basketWrites {
		t.Fatalf("explicit on: basketWrites=%v err=%v", c.basketWrites, err)
	}
	m["GLOVO_C2_BASKET_WRITES"] = "maybe"
	if _, err := loadC2(env(m)); err == nil {
		t.Fatal("a typo in GLOVO_C2_BASKET_WRITES must stop startup")
	}
}
