package glovoapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// GetOrders is getOrders (api.mjs:526): one order-history page. offset is the
// opaque order-id cursor (nil means 0); a zero limit means 15.
func (c *Client) GetOrders(ctx context.Context, offset Value, limit int) (Value, error) {
	if nullish(offset) {
		offset = 0
	}
	if limit == 0 {
		limit = 15
	}
	q := query{{"offset", jsString(offset)}, {"limit", strconv.Itoa(limit)}}
	return c.call(ctx, withQuery("/v3/customer/orders-list", q), callOpts{auth: true})
}

// GetOrder is getOrder (api.mjs:580): one past order in detail.
func (c *Client) GetOrder(ctx context.Context, orderID string) (Value, error) {
	return c.call(ctx, "/v3/customer/orders/"+seg(orderID), callOpts{auth: true})
}

// OrderCardsOptions are the getAllOrderCards options.
type OrderCardsOptions struct {
	Limit        int             // page size, capped at 15; zero means 15
	MaxPages     int             // zero means unlimited
	PageDelay    time.Duration   // pause between pages (upstream default 750ms)
	MaxRetries   int             // per page; zero means 6, negative means none
	StopOrderIDs map[string]bool // stop after a page containing one of these
}

// OrderPage records one fetched page.
type OrderPage struct {
	Cursor     Value `json:"cursor"`
	Count      int   `json:"count"`
	NextOffset Value `json:"next_offset"`
}

// OrderCards is the getAllOrderCards result.
type OrderCards struct {
	Orders        []*CompactOrder `json:"orders"`
	Pages         []OrderPage     `json:"pages"`
	Count         int             `json:"count"`
	StoppedReason string          `json:"stopped_reason"`
	Strategy      string          `json:"strategy"`
}

// ErrMalformedOrdersPage is returned for an order-history page without an
// orders array.
var ErrMalformedOrdersPage = errors.New("glovo order history page has no orders array")

func retryDelay(err error, attempt int) time.Duration {
	if isRateLimited(err) {
		return time.Duration(math.Min(15_000*math.Pow(2, float64(attempt)), 120_000)) * time.Millisecond
	}
	return time.Duration(math.Min(1000*math.Pow(2, float64(attempt)), 10_000)) * time.Millisecond
}

// withRetry mirrors withRetry in api.mjs, except that an AuthError is not
// retried: it cannot succeed and each attempt could trigger a refresh.
func (c *Client) withRetry(ctx context.Context, maxRetries int, fn func() (Value, error)) (Value, error) {
	for attempt := 0; ; attempt++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		var authErr *AuthError
		if attempt >= maxRetries || errors.As(err, &authErr) || ctx.Err() != nil {
			return nil, err
		}
		if serr := c.sleep(ctx, retryDelay(err, attempt)); serr != nil {
			return nil, err
		}
	}
}

// GetAllOrderCards is getAllOrderCards (api.mjs:531): walk the order-id
// cursor and compact every order card. A page that still fails after its
// retries fails the whole walk; no partial list is returned. A page without
// an orders array is a failed read, not an empty page.
func (c *Client) GetAllOrderCards(ctx context.Context, o OrderCardsOptions) (*OrderCards, error) {
	limit := o.Limit
	if limit <= 0 || limit > 15 {
		limit = 15
	}
	retries := o.MaxRetries
	if retries == 0 {
		retries = 6
	} else if retries < 0 {
		retries = 0
	}
	out := &OrderCards{Orders: []*CompactOrder{}, Pages: []OrderPage{}, Strategy: "order-id-cursor"}
	seenCursors := map[string]bool{}
	seenOrders := map[string]bool{}
	var cursor Value = 0
	stopped := "unknown"
	for o.MaxPages <= 0 || len(out.Pages) < o.MaxPages {
		key := jsString(cursor)
		if seenCursors[key] {
			stopped = "repeated_cursor"
			break
		}
		seenCursors[key] = true
		cur := cursor
		page, err := c.withRetry(ctx, retries, func() (Value, error) { return c.GetOrders(ctx, cur, limit) })
		if err != nil {
			return nil, fmt.Errorf("order history page %d: %w", len(out.Pages)+1, err)
		}
		raw := coalesce(get(page, "orders"), get(page, "data", "orders"), get(page, "data"), get(page, "elements"))
		pageOrders, isList := raw.([]any)
		if !isList {
			return nil, fmt.Errorf("order history page %d: %w", len(out.Pages)+1, ErrMalformedOrdersPage)
		}
		if len(pageOrders) == 0 {
			stopped = "empty_page"
			break
		}
		knownSeen := false
		for _, order := range pageOrders {
			compact := CompactOrderCard(order)
			if compact == nil || !truthy(compact.OrderID) {
				continue
			}
			id := jsString(compact.OrderID)
			if o.StopOrderIDs[id] {
				knownSeen = true
			}
			if seenOrders[id] {
				continue
			}
			seenOrders[id] = true
			out.Orders = append(out.Orders, compact)
		}
		next := orNull(get(page, "pagination", "next", "offset"))
		out.Pages = append(out.Pages, OrderPage{Cursor: cursor, Count: len(pageOrders), NextOffset: next})
		if knownSeen {
			stopped = "known_order"
			break
		}
		if !truthy(next) {
			stopped = "no_next_cursor"
			break
		}
		if jsString(next) == jsString(cursor) {
			stopped = "repeated_next_cursor"
			break
		}
		cursor = next
		if o.PageDelay > 0 {
			if err := c.sleep(ctx, o.PageDelay); err != nil {
				return nil, err
			}
		}
	}
	if o.MaxPages > 0 && len(out.Pages) >= o.MaxPages && stopped == "unknown" {
		stopped = "max_pages"
	}
	out.StoppedReason = stopped
	out.Count = len(out.Orders)
	return out, nil
}
