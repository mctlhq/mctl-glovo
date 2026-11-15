package glovotools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/glovologic"
)

// source adapts a glovoapi.Client to the glovologic source interfaces. Each
// method returns exactly what the upstream client call returns, compacted
// where the interface says so. A failed read is always an error: the client
// never turns one into an empty value, and neither does this adapter.
type source struct{ c *glovoapi.Client }

var (
	_ glovologic.SuggestionSource = source{}
	_ glovologic.ReorderSource    = source{}
	_ glovologic.HistorySource    = source{}
)

// rateLimited marks an HTTP 429 so errors.Is(err, glovologic.ErrRateLimited)
// holds, without changing the message.
type rateLimited struct{ error }

func (r rateLimited) Is(target error) bool { return target == glovologic.ErrRateLimited }
func (r rateLimited) Unwrap() error        { return r.error }

func wrap(err error) error {
	var apiErr *glovoapi.APIError
	if errors.As(err, &apiErr) && apiErr.RateLimited() {
		return rateLimited{err}
	}
	return err
}

func marshal(v any) (json.RawMessage, error) {
	b, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s source) OrderCards(ctx context.Context, o glovologic.CardOptions) (json.RawMessage, error) {
	cards, err := s.c.GetAllOrderCards(ctx, glovoapi.OrderCardsOptions{
		Limit: o.Limit, MaxPages: o.MaxPages, PageDelay: o.PageDelay, MaxRetries: o.MaxRetries, StopOrderIDs: o.StopOrderIDs,
	})
	if err != nil {
		return nil, wrap(err)
	}
	return marshal(cards)
}

func (s source) OrderDetail(ctx context.Context, orderID string) (json.RawMessage, error) {
	v, err := s.c.GetOrder(ctx, orderID)
	if err != nil {
		return nil, wrap(err)
	}
	return marshal(orNull(v))
}

func (s source) StoreContent(ctx context.Context, storeID, storeAddressID string) (glovologic.ContentProducts, error) {
	content, err := s.c.GetStoreContent(ctx, storeID, storeAddressID, glovoapi.StoreContentOptions{Auth: true})
	if err != nil {
		return nil, wrap(err)
	}
	return func(kind string, limit int) (json.RawMessage, error) {
		out, err := glovoapi.CompactStoreContent(content, glovoapi.StoreContentCompactOptions{Kind: kind, Limit: limit})
		if err != nil {
			return nil, err
		}
		products := out.Products
		if products == nil {
			products = []*glovoapi.CompactItem{}
		}
		return marshal(products)
	}, nil
}

func (s source) SearchStoreItems(ctx context.Context, storeID, storeAddressID, query string) (json.RawMessage, error) {
	raw, err := s.c.SearchStoreItems(ctx, storeID, storeAddressID, query)
	if err != nil {
		return nil, wrap(err)
	}
	out, err := glovoapi.CompactSearch(raw, glovoapi.SearchOptions{Limit: 24})
	if err != nil {
		return nil, err
	}
	results := out.Results
	if results == nil {
		results = []*glovoapi.CompactItem{}
	}
	return marshal(results)
}

func (s source) SearchStores(ctx context.Context, query string) (json.RawMessage, error) {
	v, err := s.c.SearchStores(ctx, query)
	if err != nil {
		return nil, wrap(err)
	}
	return marshal(orNull(v))
}

func (s source) ProductView(ctx context.Context, r glovologic.ProductRequest) (json.RawMessage, error) {
	opt := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	raw, err := s.c.GetProduct(ctx, glovoapi.ProductParams{
		StoreID: r.StoreID, StoreAddressID: r.StoreAddressID, ProductID: r.ProductID, ExternalID: r.ExternalID,
		CategoryID: opt(r.CategoryID), CollectionID: opt(r.CollectionID), CollectionGroupID: opt(r.CollectionGroupID),
		SearchID: r.SearchID,
	})
	if err != nil {
		return nil, wrap(err)
	}
	view, err := glovoapi.CompactProductView(raw)
	if err != nil {
		return nil, err
	}
	return marshal(view)
}
