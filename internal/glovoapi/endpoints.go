package glovoapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Path segments are always escaped with encodeURIComponent. Upstream
// interpolates several ids raw (store, address, product, customer, order);
// escaping keeps numeric ids identical and stops a crafted id from changing
// the path.
func seg(s string) string { return encodeURIComponent(s) }

// SearchAddresses is searchAddresses (api.mjs:152): public address lookup.
func (c *Client) SearchAddresses(ctx context.Context, address string, allowFallback bool) (Value, error) {
	if utf8.RuneCountInString(strings.TrimSpace(address)) < 3 {
		return nil, errors.New("address search requires at least 3 characters")
	}
	q := query{{"address", address}, {"allowFallback", strconv.FormatBool(allowFallback)}}
	return c.call(ctx, withQuery("/v3/addresslookup/pub/address", q), callOpts{})
}

// ResolveAddress is resolveAddress (api.mjs:158).
func (c *Client) ResolveAddress(ctx context.Context, placeID, provider string) (Value, error) {
	if placeID == "" {
		return nil, errors.New("placeId is required")
	}
	var q query
	if provider != "" {
		q.set("provider", provider)
	}
	return c.call(ctx, withQuery("/v3/addresslookup/pub/"+seg(placeID), q), callOpts{})
}

// ReverseAddress is reverseAddress (api.mjs:165): coordinates to address.
func (c *Client) ReverseAddress(ctx context.Context, latitude, longitude float64, allowFallback bool) (Value, error) {
	q := query{
		{"latitude", jsNumberString(latitude)},
		{"longitude", jsNumberString(longitude)},
		{"allowFallback", strconv.FormatBool(allowFallback)},
	}
	return c.call(ctx, withQuery("/v3/addresslookup/pub/coordinates", q), callOpts{})
}

// DeliveryPointInfo is deliveryPointInfo (api.mjs:175): guest serviceability.
func (c *Client) DeliveryPointInfo(ctx context.Context, latitude, longitude float64, countryCode string) (Value, error) {
	if countryCode == "" {
		return nil, errors.New("countryCode is required")
	}
	q := query{
		{"latitude", jsNumberString(latitude)},
		{"longitude", jsNumberString(longitude)},
		{"countryCode", countryCode},
	}
	return c.call(ctx, withQuery("/customer_profile/api/v1/guest/address_book/delivery_point_info", q), callOpts{})
}

// GetSavedLocations is getSavedLocations (api.mjs:186): the address book.
func (c *Client) GetSavedLocations(ctx context.Context) (Value, error) {
	return c.call(ctx, "/customer_profile/api/v1/address_book/me/addresses", callOpts{auth: true})
}

// SavedLocations is savedLocations (api.mjs:190).
func (c *Client) SavedLocations(ctx context.Context, matchText string) (*SavedLocations, error) {
	raw, err := c.GetSavedLocations(ctx)
	if err != nil {
		return nil, err
	}
	return CompactSavedLocations(raw, c.Location(), matchText)
}

// BrowseStoresParams are the browseStores options. Zero values take the
// upstream defaults: category 4, limit 50, handling type DELIVERY.
type BrowseStoresParams struct {
	CategoryID       int
	Offset           int
	Limit            int
	Filters          []any
	PreviousStoreIDs []string
	HandlingType     string
}

// BrowseStores is browseStores (api.mjs:315): the store wall of a category.
func (c *Client) BrowseStores(ctx context.Context, p BrowseStoresParams) (Value, error) {
	if p.CategoryID == 0 {
		p.CategoryID = 4
	}
	if p.Limit == 0 {
		p.Limit = 50
	}
	if p.HandlingType == "" {
		p.HandlingType = "DELIVERY"
	}
	if p.Filters == nil {
		p.Filters = []any{}
	}
	if p.PreviousStoreIDs == nil {
		p.PreviousStoreIDs = []string{}
	}
	q := query{
		{"moduleId", "StoreFeeds"},
		{"offset", strconv.Itoa(p.Offset)},
		{"limit", strconv.Itoa(p.Limit)},
		{"handlingType", p.HandlingType},
	}
	body := struct {
		Filters          []any    `json:"filters"`
		PreviousStoreIDs []string `json:"previousStoreIds"`
	}{p.Filters, p.PreviousStoreIDs}
	return c.call(ctx, withQuery("/v1/web/store_wall/category/"+strconv.Itoa(p.CategoryID), q), callOpts{method: http.MethodPost, body: body})
}

// SearchStores is searchStores (api.mjs:328): store wall search.
func (c *Client) SearchStores(ctx context.Context, searchQuery string) (Value, error) {
	v := strings.TrimSpace(searchQuery)
	if v == "" {
		return nil, errors.New("store search query is required")
	}
	body := map[string]any{"searchContext": map[string]any{"searchId": c.newID()}}
	return c.call(ctx, withQuery("/v1/web/store_wall/search", query{{"searchQuery", v}}), callOpts{method: http.MethodPost, body: body})
}

// GetStore is getStore (api.mjs:338): a store by slug or id.
func (c *Client) GetStore(ctx context.Context, store string) (Value, error) {
	return c.call(ctx, "/v3/stores/"+seg(store)+"?includeClosed=true&includeDisabled=false", callOpts{})
}

// MenuOptions are the optional getStoreMenu parameters.
type MenuOptions struct {
	Translation string
	Consents    string
}

// GetStoreMenu is getStoreMenu (api.mjs:342).
func (c *Client) GetStoreMenu(ctx context.Context, storeID, storeAddressID string, o MenuOptions) (Value, error) {
	var q query
	if o.Translation != "" {
		q.set("translation", o.Translation)
	}
	if o.Consents != "" {
		q.set("consents", o.Consents)
	}
	return c.call(ctx, withQuery("/v3/stores/"+seg(storeID)+"/addresses/"+seg(storeAddressID)+"/node/store_menu", q), callOpts{})
}

// StoreContentOptions are the getStoreContent options. UseV3 selects the v3
// endpoint; upstream defaults to v4.
type StoreContentOptions struct {
	ContentSlug string
	Translation string
	Consents    string
	UseV3       bool
	Auth        bool
}

// GetStoreContent is getStoreContent (api.mjs:350): store carousels.
func (c *Client) GetStoreContent(ctx context.Context, storeID, storeAddressID string, o StoreContentOptions) (Value, error) {
	var q query
	if o.Translation != "" {
		q.set("translation", o.Translation)
	}
	if o.ContentSlug != "" {
		q.set("nodeType", "DEEP_LINK")
		q.set("link", o.ContentSlug)
	}
	if o.Consents != "" {
		q.set("consents", o.Consents)
	}
	path := "/v4/stores/" + seg(storeID) + "/addresses/" + seg(storeAddressID) + "/content/main"
	if o.UseV3 {
		path = "/v3/stores/" + seg(storeID) + "/addresses/" + seg(storeAddressID) + "/content"
	}
	return c.call(ctx, withQuery(path, q), callOpts{auth: o.Auth})
}

// GetStoreCatalog is getStoreCatalog (api.mjs:364): an exact content_uri from
// the store menu, accepted only when it stays on the API origin and under the
// selected store and address.
func (c *Client) GetStoreCatalog(ctx context.Context, storeID, storeAddressID, contentURI string, auth bool) (Value, error) {
	base, err := url.Parse(c.baseURL + "/")
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(contentURI)
	if err != nil {
		return nil, errors.New("catalog content URI is not a valid URL")
	}
	u := base.ResolveReference(ref)
	if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil {
		return nil, errors.New("catalog content URI must use the Glovo API origin")
	}
	p := u.EscapedPath()
	ok := false
	for _, v := range []string{"3", "4"} {
		prefix := "/v" + v + "/stores/" + seg(storeID) + "/addresses/" + seg(storeAddressID) + "/content"
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			ok = true
		}
	}
	if !ok {
		return nil, errors.New("catalog content URI must belong to the selected store and address")
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return c.call(ctx, p, callOpts{auth: auth})
}

// GetStoreFees is getStoreFees (api.mjs:376).
func (c *Client) GetStoreFees(ctx context.Context, storeID, storeAddressID string) (Value, error) {
	return c.call(ctx, "/v1/stores/"+seg(storeID)+"/addresses/"+seg(storeAddressID)+"/node/store_fees", callOpts{})
}

// GetStoreRestrictions is getStoreRestrictions (api.mjs:380).
func (c *Client) GetStoreRestrictions(ctx context.Context, storeID, storeAddressID string) (Value, error) {
	return c.call(ctx, "/v4/stores/"+seg(storeID)+"/addresses/"+seg(storeAddressID)+"/restrictions", callOpts{})
}

// GetStoreInfo is getStoreInfo (api.mjs:384). An empty translation means "en".
func (c *Client) GetStoreInfo(ctx context.Context, storeID, storeAddressID, translation string) (Value, error) {
	if translation == "" {
		translation = "en"
	}
	return c.call(ctx, "/v3/stores/"+seg(storeID)+"/addresses/"+seg(storeAddressID)+"/store_info_screen?translation="+encodeURIComponent(translation), callOpts{})
}

// GetSimilarStores is getSimilarStores (api.mjs:388). A zero limit means 5.
func (c *Client) GetSimilarStores(ctx context.Context, storeID string, limit int) (Value, error) {
	if limit == 0 {
		limit = 5
	}
	q := query{{"limit", strconv.Itoa(limit)}, {"storeId", storeID}, {"city_changed", "false"}}
	return c.call(ctx, withQuery("/v1/store-view/web/similar-stores", q), callOpts{})
}

// StoreOrderOptions fetches fees, restrictions, info and similar stores and
// compacts them, like the glovo_get_store_order_options tool. Any failed read
// fails the whole result.
func (c *Client) StoreOrderOptions(ctx context.Context, storeID, storeAddressID, translation string, similarLimit int) (*CompactStoreOrderOptionsResult, error) {
	fees, err := c.GetStoreFees(ctx, storeID, storeAddressID)
	if err != nil {
		return nil, err
	}
	restrictions, err := c.GetStoreRestrictions(ctx, storeID, storeAddressID)
	if err != nil {
		return nil, err
	}
	info, err := c.GetStoreInfo(ctx, storeID, storeAddressID, translation)
	if err != nil {
		return nil, err
	}
	similar, err := c.GetSimilarStores(ctx, storeID, similarLimit)
	if err != nil {
		return nil, err
	}
	return CompactStoreOrderOptions(fees, restrictions, info, similar)
}

// SearchStoreItems is searchStoreItems (api.mjs:393): product search inside
// one store.
func (c *Client) SearchStoreItems(ctx context.Context, storeID, storeAddressID, searchQuery string) (Value, error) {
	q := query{{"query", searchQuery}, {"searchId", c.newID()}}
	return c.call(ctx, withQuery("/v3/stores/"+seg(storeID)+"/addresses/"+seg(storeAddressID)+"/search", q), callOpts{})
}

// ProductParams are the getProduct arguments. CategoryID, CollectionID and
// CollectionGroupID are passed through as given (string or number); nil
// omits them. A zero Quantity means 1.
type ProductParams struct {
	StoreID           string
	StoreAddressID    string
	ProductID         string
	ExternalID        string
	CategoryID        any
	CollectionID      any
	CollectionGroupID any
	SearchID          string
	Quantity          int
}

type productViewBody struct {
	EditingMode       bool    `json:"editingMode"`
	Origin            string  `json:"origin"`
	CategoryID        any     `json:"categoryId,omitempty"`
	CollectionID      any     `json:"collectionId,omitempty"`
	CollectionGroupID any     `json:"collectionGroupId,omitempty"`
	Translation       *string `json:"translation"`
	SearchID          string  `json:"searchId,omitempty"`
	TrackingOrigin    *string `json:"trackingOrigin"`
	Quantity          int     `json:"quantity"`
}

// GetProduct is getProduct (api.mjs:398): the product view. It is a POST but
// reads only.
func (c *Client) GetProduct(ctx context.Context, p ProductParams) (Value, error) {
	if p.Quantity == 0 {
		p.Quantity = 1
	}
	var q query
	if p.ExternalID != "" {
		q.set("productExternalId", p.ExternalID)
	}
	body := productViewBody{
		Origin:            "STORE",
		CategoryID:        p.CategoryID,
		CollectionID:      p.CollectionID,
		CollectionGroupID: p.CollectionGroupID,
		SearchID:          p.SearchID,
		Quantity:          p.Quantity,
	}
	path := "/v4/stores/" + seg(p.StoreID) + "/addresses/" + seg(p.StoreAddressID) + "/products/" + seg(p.ProductID) + "/view"
	return c.call(ctx, withQuery(path, q), callOpts{method: http.MethodPost, body: body})
}
