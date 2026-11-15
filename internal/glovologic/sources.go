package glovologic

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"
)

// ErrRateLimited marks a source error as a Glovo rate limit (HTTP 429). Sources
// wrap it so errors.Is(err, ErrRateLimited) holds; it changes the retry delay
// and stops order-detail enrichment, as RateLimitError does upstream.
var ErrRateLimited = errors.New("glovo rate limited")

// CardOptions are the getAllOrderCards options (api.mjs:531).
type CardOptions struct {
	Limit        int             // page size; upstream always passes 15
	MaxPages     int             // zero means unlimited
	PageDelay    time.Duration   // pause between pages
	MaxRetries   int             // per page; zero means the source default (6)
	StopOrderIDs map[string]bool // stop after a page containing one of these
}

// OrderCardSource walks the order-history cursor. Its JSON is the
// getAllOrderCards result: {orders: compactOrder[], pages: [], count,
// stopped_reason, strategy}, as glovoapi.OrderCards encodes it. A failed
// page is an error, never a shorter list.
type OrderCardSource interface {
	OrderCards(ctx context.Context, o CardOptions) (json.RawMessage, error)
}

// OrderDetailSource reads one raw order detail (GET /v3/customer/orders/{id}).
type OrderDetailSource interface {
	OrderDetail(ctx context.Context, orderID string) (json.RawMessage, error)
}

// ContentProducts compacts one already-fetched store content response:
// it returns compactStoreContent(content, {kind, limit}).products
// (api.mjs:953) as a JSON array, without store_id extras.
type ContentProducts func(kind string, limit int) (json.RawMessage, error)

// StoreContentSource fetches getStoreContent(storeID, storeAddressID,
// {auth: true}) once.
type StoreContentSource interface {
	StoreContent(ctx context.Context, storeID, storeAddressID string) (ContentProducts, error)
}

// StoreSearchSource runs searchStoreItems and returns compactSearch(data,
// {limit: 24}).results (api.mjs:933) as a JSON array. This package sets the
// store_id and store_address_id extras itself and applies smaller limits.
type StoreSearchSource interface {
	SearchStoreItems(ctx context.Context, storeID, storeAddressID, query string) (json.RawMessage, error)
}

// ReorderSource is everything planReorder reads.
type ReorderSource interface {
	OrderDetailSource
	StoreContentSource
	StoreSearchSource
}

// HistorySource is everything analyzeOrderHistory reads.
type HistorySource interface {
	OrderCardSource
	OrderDetailSource
}

// ProductRequest identifies a product for getProduct (api.mjs:398). Empty
// strings are absent parameters.
type ProductRequest struct {
	StoreID, StoreAddressID, ProductID, ExternalID       string
	CategoryID, CollectionID, CollectionGroupID, SearchID string
}

// SuggestionSource is everything getSuggestions reads from Glovo.
type SuggestionSource interface {
	OrderCardSource
	StoreContentSource
	StoreSearchSource
	// SearchStores is the raw global store search response
	// (POST /v1/web/store_wall/search).
	SearchStores(ctx context.Context, query string) (json.RawMessage, error)
	// ProductView is compactProductView(getProduct(...)) (api.mjs:1026) as JSON.
	ProductView(ctx context.Context, r ProductRequest) (json.RawMessage, error)
}

// Sleep pauses for d or until ctx is done.
type Sleep func(ctx context.Context, d time.Duration) error

func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func orSleep(s Sleep) Sleep {
	if s == nil {
		return realSleep
	}
	return s
}

func retryDelay(err error, attempt int) time.Duration {
	if errors.Is(err, ErrRateLimited) {
		return time.Duration(math.Min(15_000*math.Pow(2, float64(attempt)), 120_000)) * time.Millisecond
	}
	return time.Duration(math.Min(1000*math.Pow(2, float64(attempt)), 10_000)) * time.Millisecond
}

// withRetry is withRetry (api.mjs:101) without the stderr log line. Input
// errors are not retried: the same input would fail again.
func withRetry[T any](ctx context.Context, sleep Sleep, maxRetries int, fn func() (T, error)) (T, error) {
	for attempt := 0; ; attempt++ {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		var inErr *InputError
		if attempt >= maxRetries || errors.As(err, &inErr) || ctx.Err() != nil {
			return v, err
		}
		if serr := sleep(ctx, retryDelay(err, attempt)); serr != nil {
			return v, err
		}
	}
}

// Discovery is a decoded getAllOrderCards result.
type Discovery struct {
	Orders        []Object `json:"orders"`
	Pages         []any    `json:"pages"`
	Count         int      `json:"count"`
	StoppedReason string   `json:"stopped_reason"`
	Strategy      any      `json:"strategy,omitempty"`
}

// DecodeDiscovery decodes and validates a getAllOrderCards result.
func DecodeDiscovery(raw []byte) (*Discovery, error) {
	v, err := decodeAt(raw, "discovery")
	if err != nil {
		return nil, err
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, &InputError{Path: "discovery", Msg: "want object, got " + typeName(v)}
	}
	if err := expectFields(v, "discovery", map[string]kind{"orders": kArray, "pages": kArray, "stopped_reason": kString, "strategy": kString}); err != nil {
		return nil, err
	}
	orders, ok := get(v, "orders").([]any)
	if !ok {
		return nil, &InputError{Path: "discovery.orders", Msg: "missing orders array"}
	}
	pages, ok := get(v, "pages").([]any)
	if !ok {
		return nil, &InputError{Path: "discovery.pages", Msg: "missing pages array"}
	}
	reason, ok := get(v, "stopped_reason").(string)
	if !ok {
		return nil, &InputError{Path: "discovery.stopped_reason", Msg: "missing stopped_reason"}
	}
	d := &Discovery{Orders: make([]Object, len(orders)), Pages: pages, StoppedReason: reason, Strategy: get(v, "strategy")}
	for i, o := range orders {
		if err := checkOrderCard(o, idx("discovery.orders", i)); err != nil {
			return nil, err
		}
		d.Orders[i] = o.(map[string]any)
	}
	d.Count = len(d.Orders)
	return d, nil
}

func fetchDiscovery(ctx context.Context, src OrderCardSource, o CardOptions) (*Discovery, error) {
	raw, err := src.OrderCards(ctx, o)
	if err != nil {
		return nil, err
	}
	return DecodeDiscovery(raw)
}

// decodeProducts decodes a JSON array of compactItem objects.
func decodeProducts(raw []byte, path string) ([]Object, error) {
	return decodeObjects(raw, path, checkProduct)
}
