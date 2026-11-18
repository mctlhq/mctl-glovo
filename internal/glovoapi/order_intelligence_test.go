package glovoapi

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Synthetic order-history walks for mctl-glovo#45. All ids and names are
// invented.

func synthPage(next string, ids ...string) string {
	var orders []string
	for _, id := range ids {
		orders = append(orders, fmt.Sprintf(`{"id":%s,"storeName":"Synthetic Kitchen","status":"DELIVERED","total":{"formatted":"10,00 EUR"},"products":[{"name":"Dish %s"}]}`, id, id))
	}
	nextJSON := ""
	if next != "" {
		nextJSON = fmt.Sprintf(`,"pagination":{"next":{"offset":%q}}`, next)
	}
	return `{"orders":[` + strings.Join(orders, ",") + `]` + nextJSON + `}`
}

// A repeated order across pages (and a repeated id inside a page) appears once.
func TestOrderIntelligenceDuplicateOrdersAcrossPagesAreDropped(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v3/customer/orders-list",
		reply{200, synthPage("c2", "1", "2", "2")},
		reply{200, synthPage("", "2", "3")})
	res, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, o := range res.Orders {
		ids = append(ids, jsString(o.OrderID))
	}
	if got := strings.Join(ids, ","); got != "1,2,3" || res.Count != 3 {
		t.Fatalf("ids %q count %d", got, res.Count)
	}
	// Pages report the raw per-page counts, so duplicates are visible there.
	if len(res.Pages) != 2 || res.Pages[0].Count != 3 || res.Pages[1].Count != 2 || res.StoppedReason != "no_next_cursor" {
		t.Fatalf("pages %+v stopped %q", res.Pages, res.StoppedReason)
	}
}

// max_pages reached with a next cursor still pending is reported, not hidden.
func TestOrderIntelligenceMaxPagesIsReportedAsPartial(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v3/customer/orders-list",
		reply{200, synthPage("c2", "1", "2")},
		reply{200, synthPage("c3", "3", "4")},
		reply{200, synthPage("", "5")})
	res, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{MaxPages: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.StoppedReason != "max_pages" || len(res.Pages) != 2 || res.Count != 4 {
		t.Fatalf("stopped %q pages %d count %d", res.StoppedReason, len(res.Pages), res.Count)
	}
	if got := f.count("GET", "/v3/customer/orders-list"); got != 2 {
		t.Fatalf("%d page requests, want 2", got)
	}
}

// A user with no orders: an empty page is a successful, explicitly stopped
// walk, not an error and not a missing field.
func TestOrderIntelligenceNoHistoryIsExplicit(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v3/customer/orders-list", reply{200, `{"orders":[]}`})
	res, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 0 || res.StoppedReason != "empty_page" || len(res.Pages) != 0 || res.Orders == nil {
		t.Fatalf("%+v", res)
	}
}

// Cards keep their status, so a cancelled order is distinguishable downstream.
func TestOrderIntelligenceCardKeepsCancelledStatus(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v3/customer/orders-list", reply{200, `{"orders":[{"id":9,"storeName":"S","status":"CANCELLED","products":[{"name":"X"}]}]}`})
	res, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Orders) != 1 || jsString(res.Orders[0].Status) != "CANCELLED" {
		t.Fatalf("%+v", res.Orders)
	}
}

// Order-history pages are capped at 15 whatever the caller asks for.
func TestOrderIntelligencePageSizeCappedAt15(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, testSession())
	f.on("GET", "/v3/customer/orders-list", reply{200, `{"orders":[]}`})
	if _, err := c.GetAllOrderCards(context.Background(), OrderCardsOptions{Limit: 50}); err != nil {
		t.Fatal(err)
	}
	if u := f.last().URI; !strings.HasSuffix(u, "limit=15") {
		t.Fatalf("uri %s", u)
	}
}
