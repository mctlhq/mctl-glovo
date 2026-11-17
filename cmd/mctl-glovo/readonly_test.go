package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"encoding/json"
	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/linkflow"
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
	start := strings.Index(body, "{")
	if start < 0 {
		t.Fatalf("tools/list: no JSON in %q", body)
	}
	if err := json.Unmarshal([]byte(body[start:]), &env); err != nil {
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
	e := newMCPEnvWith(t, nil, func(d *mcpDeps) {
		d.BasketWrites = true
		d.BasketApprover = func(context.Context, string, string, map[string]any) error { return nil }
	}, nil)
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

// GLOVO_C2_BASKET_WRITES exists only to be refused: no value turns writes on.
// "true" and every spelling that is not "false" stop startup (with C2 on), and an unset or "false" value leaves the config read-only.
func TestLoadBasketWritesIsNeverOn(t *testing.T) {
	for _, tc := range []struct {
		v       string
		wantErr bool
	}{
		{"", false}, {"false", false},
		{"true", true}, {"TRUE", true}, {"True", true}, {" true", true}, {"true ", true}, {"\ttrue", true},
		{"1", true}, {"yes", true}, {"on", true}, {"enabled", true}, {"FALSE", true}, {" false", true}, {"0", true}, {"maybe", true},
	} {
		got, err := loadBasketWrites(env(map[string]string{"GLOVO_C2_BASKET_WRITES": tc.v}))
		if got {
			t.Errorf("%q: loadBasketWrites returned on", tc.v)
		}
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: err = %v, wantErr %v", tc.v, err, tc.wantErr)
		}
		// Through the whole C2 config. With C2 off nothing else is read and
		// no hosted route exists, so the value is moot there.
		m := fullEnv()
		m["GLOVO_C2_BASKET_WRITES"] = tc.v
		c, err := loadC2(env(m))
		if c.basketWrites || c.basketApprover != nil {
			t.Errorf("%q: the C2 config enables writes", tc.v)
		}
		if (err != nil) != tc.wantErr {
			t.Errorf("%q: loadC2 err = %v, wantErr %v", tc.v, err, tc.wantErr)
		}
		m["GLOVO_C2_ENABLED"] = "false"
		if c, err := loadC2(env(m)); err != nil || c.enabled || c.basketWrites {
			t.Errorf("%q with C2 off: %+v %v", tc.v, c, err)
		}
	}
	if err := func() error {
		_, e := loadBasketWrites(env(map[string]string{"GLOVO_C2_BASKET_WRITES": "true"}))
		return e
	}(); !errors.Is(err, errBasketWritesNoGate) {
		t.Errorf("true: err = %v, want errBasketWritesNoGate", err)
	}
	// Default of the whole C2 config: off, nothing to approve with.
	c, err := loadC2(env(fullEnv()))
	if err != nil || c.basketWrites || c.basketApprover != nil {
		t.Fatalf("default C2 config: basketWrites=%v err=%v", c.basketWrites, err)
	}
}

// The code seam: write tools are served only with an approver. The
// environment cannot supply one, so there is no flag-only path to writes.
func TestBasketWritesNeedAnApprover(t *testing.T) {
	fake := newFakeGlovo(t)
	src := &fakeSource{clients: map[string]*glovoapi.Client{"prn_A": hostedClient(t, fake.srv.URL, "A")}}
	base := mcpDeps{
		Who: testWho, Verifier: tokenVerifier{"tok-a": {ID: "prn_A"}}, Clients: src,
		Audit: slog.New(slog.NewJSONHandler(io.Discard, nil)), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Kill: func(context.Context) error { return nil },
	}
	on := base
	on.BasketWrites = true
	if _, err := newMCPRoutes(on); err == nil || !strings.Contains(err.Error(), "approval") {
		t.Fatalf("writes without an approver: err = %v", err)
	}
	on.BasketApprover = func(context.Context, string, string, map[string]any) error { return nil }
	r, err := newMCPRoutes(on)
	if err != nil {
		t.Fatalf("writes with an approver: %v", err)
	}
	_ = r.Shutdown(context.Background())
}

// approvalMiddleware: reads pass, a write tool needs the approver to say yes,
// and no approver or an error is a refusal that never calls the handler.
func TestApprovalMiddleware(t *testing.T) {
	ok := func(context.Context, string, string, map[string]any) error { return nil }
	no := func(context.Context, string, string, map[string]any) error { return errors.New("no") }
	call := func(approve basketApprover, tool string) (reached bool, res *mcp.CallToolResult) {
		h := approvalMiddleware(approve)(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			reached = true
			return mcp.NewToolResultText("ran"), nil
		})
		req := mcp.CallToolRequest{}
		req.Params.Name = tool
		res, _ = h(context.Background(), req)
		return reached, res
	}
	for _, w := range writeTools {
		if reached, res := call(nil, w); reached || res == nil || !res.IsError {
			t.Errorf("%s with no approver: reached=%v res=%v", w, reached, res)
		}
		if reached, res := call(no, w); reached || res == nil || !res.IsError {
			t.Errorf("%s with a refusing approver: reached=%v res=%v", w, reached, res)
		}
		if reached, res := call(ok, w); !reached || res == nil || res.IsError {
			t.Errorf("%s with an approving approver: reached=%v res=%v", w, reached, res)
		}
	}
	for _, rd := range []string{"glovo_get_basket", "glovo_analyze_order_history"} {
		if reached, _ := call(nil, rd); !reached {
			t.Errorf("read tool %s blocked by the approval middleware", rd)
		}
	}
}

// The consent text may only claim what tools/list serves. With no basket
// write tool listed, every sentence that names a basket must deny changing it,
// no sentence may promise building, adding, ordering or paying, and each
// capability the text names must be backed by a served tool.
func TestConsentTextMatchesTheServedTools(t *testing.T) {
	e := newMCPEnv(t, nil)
	session := e.open(t, "tok-a")
	have := map[string]bool{}
	for _, n := range e.toolsList(t, "tok-a", session) {
		have[n] = true
	}
	for _, w := range writeTools {
		if have[w] {
			t.Fatalf("%s is listed by default; this test is about the read-only list", w)
		}
	}
	// capability phrase -> a tool that must be served for the text to say it
	backing := map[string]string{
		"order history":   "glovo_get_purchase_history",
		"basket":          "glovo_get_basket",
		"saved locations": "glovo_get_saved_locations",
		"reorder":         "glovo_plan_reorder",
		"analyse":         "glovo_analyze_order_history",
	}
	negation := regexp.MustCompile(`(?i)\b(never|not|no)\b`)
	promise := regexp.MustCompile(`(?i)\b(build|builds|building|add|adds|create|creates|prepare|prepares|put|puts|place|places|buy|buys|pay|pays|checkout|check out)\b`)
	text := strings.Join(linkflow.ConsentText, "\n")
	lower := strings.ToLower(text)
	for _, sentence := range strings.Split(text, ". ") {
		low := strings.ToLower(sentence)
		if strings.Contains(low, "basket") && !negation.MatchString(sentence) && !strings.Contains(low, "read") {
			t.Errorf("a sentence names the basket without saying it is only read or never changed: %q", sentence)
		}
		if promise.MatchString(sentence) && !negation.MatchString(sentence) {
			t.Errorf("a sentence promises a capability the tool list does not serve: %q", sentence)
		}
	}
	for phrase, tool := range backing {
		if strings.Contains(lower, phrase) && !have[tool] {
			t.Errorf("the consent text names %q, but %s is not served", phrase, tool)
		}
	}
	// The other way: what the text says it never does has no tool either.
	for n := range have {
		if strings.Contains(n, "checkout") || strings.Contains(n, "pay") || strings.Contains(n, "place_order") {
			t.Errorf("tool %s contradicts the consent text", n)
		}
	}
	if !strings.Contains(lower, "never used to change your basket") {
		t.Errorf("the consent text no longer says the basket is not changed")
	}
}

// The client layer follows the config: none by default, so a hosted client is
// read-only for baskets; the option appears only with the code seam set.
func TestBasketWriteOptionsFollowTheConfig(t *testing.T) {
	c, err := loadC2(env(fullEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if got := basketWriteOptions(c); len(got) != 0 {
		t.Fatalf("default C2 config yields %d basket write option(s)", len(got))
	}
	if got := basketWriteOptions(c2Config{basketWrites: true}); len(got) != 1 {
		t.Fatalf("basketWrites seam yields %d option(s), want 1", len(got))
	}
}

// End to end: with the capability on at both layers and an approver that
// says no, a write tool call is refused by the approval step and nothing
// reaches Glovo, not even the basket read the call would start with. With a
// yes it goes through. Dropping the approval middleware turns the first half
// red.
func TestApproverSitsInFrontOfEveryWriteToolCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		approve error
	}{{"refused", errors.New("no")}, {"approved", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			var fake *fakeGlovo
			src := &fakeSource{clients: map[string]*glovoapi.Client{}}
			e := newMCPEnvWith(t, nil, func(d *mcpDeps) {
				d.BasketWrites = true
				d.BasketApprover = func(_ context.Context, pid, tool string, _ map[string]any) error {
					asked = append(asked, pid+" "+tool)
					return tc.approve
				}
				fake = newFakeGlovo(t)
				c, err := glovoapi.New("",
					glovoapi.WithBaseURL(fake.srv.URL),
					glovoapi.WithSession(glovoapi.Session{
						"accessToken": marker("A"), "refreshToken": "refresh-A", "customerId": float64(1),
						"location": map[string]any{"cityCode": "TST", "latitude": "1", "longitude": "2", "countryCode": "XX"},
					}),
					glovoapi.WithRefreshCoordinator(func(context.Context, glovoapi.Session, func(context.Context, glovoapi.Session) (glovoapi.Session, error)) (glovoapi.Session, error) {
						return nil, errors.New("unexpected refresh")
					}),
					glovoapi.WithRequestGate(func(context.Context) error { return nil }),
					glovoapi.WithClock(func() time.Time { return time.Unix(1_800_000_000, 0) }),
					glovoapi.WithBasketWrites(),
				)
				if err != nil {
					t.Fatal(err)
				}
				src.clients["prn_A"] = c
				d.Clients = src
			}, nil)
			session := e.open(t, "tok-a")
			_, res, body := e.call(t, "tok-a", session, "glovo_remove_from_basket", map[string]any{"store_id": "1", "basket_product_id": "bp"})
			if len(asked) != 1 || !strings.HasSuffix(asked[0], " glovo_remove_from_basket") {
				t.Fatalf("approver asked %v", asked)
			}
			if tc.approve != nil {
				if res == nil || !res.IsError || resultText(res) != glovotools.BasketWritesRefusal {
					t.Fatalf("not refused by the approval step: %s", body)
				}
				if n := len(fake.seen()); n != 0 {
					t.Fatalf("%d request(s) reached Glovo after a refusal", n)
				}
				return
			}
			if res != nil && resultText(res) == glovotools.BasketWritesRefusal {
				t.Fatalf("approved call was refused: %s", body)
			}
			if n := len(fake.seen()); n == 0 {
				t.Fatal("approved call never reached the provider layer")
			}
		})
	}
}
