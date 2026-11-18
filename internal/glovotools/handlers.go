package glovotools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mctlhq/mctl-glovo/internal/glovoapi"
	"github.com/mctlhq/mctl-glovo/internal/glovologic"
)

// args are the validated tool arguments: JSON numbers are float64, as
// JavaScript numbers are.
type args map[string]any

func (a args) has(k string) bool { v, ok := a[k]; return ok && v != nil }

// raw is the argument as given, nil when absent.
func (a args) raw(k string) any { return a[k] }

// str is String(arg), or "" when the argument is absent.
func (a args) str(k string) string {
	if !a.has(k) {
		return ""
	}
	return glovoapi.JSString(a[k])
}

// truthyStr is String(arg) when the argument is truthy, else "": upstream
// tests ids with `id && ...` before comparing String(id).
func (a args) truthyStr(k string) string {
	if !glovoapi.Truthy(a[k]) {
		return ""
	}
	return glovoapi.JSString(a[k])
}

// num is `arg ?? def`.
func (a args) num(k string, def float64) float64 {
	if f, ok := a[k].(float64); ok {
		return f
	}
	return def
}

func (a args) intOr(k string, def int) int { return int(a.num(k, float64(def))) }

func (a args) boolean(k string) bool { b, _ := a[k].(bool); return b }

func (a args) ms(k string, def int) time.Duration {
	return time.Duration(a.intOr(k, def)) * time.Millisecond
}

func handlers(b *Backend) map[string]handler {
	return map[string]handler{
		"glovo_get_shopping_guide": func(context.Context, *glovoapi.Client, args) (any, error) {
			if b.readOnly() {
				return textResult(shoppingGuideReadOnly), nil
			}
			return textResult(shoppingGuide), nil
		},
		"glovo_auth_status": func(_ context.Context, c *glovoapi.Client, _ args) (any, error) {
			return c.AuthStatus(), nil
		},
		"glovo_get_location": func(_ context.Context, c *glovoapi.Client, _ args) (any, error) {
			return c.Location(), nil
		},
		"glovo_set_location": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			return c.SetLocationContext(ctx, glovoapi.LocationUpdate{
				CountryCode:  a.str("country_code"),
				CityCode:     a.str("city_code"),
				Latitude:     a.raw("latitude"),
				Longitude:    a.raw("longitude"),
				LanguageCode: a.str("language_code"),
			})
		},
		"glovo_get_saved_locations": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			return c.SavedLocations(ctx, a.str("match_text"))
		},
		"glovo_search_locations": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.SearchAddresses(ctx, a.str("query"), true)
			if err != nil {
				return nil, err
			}
			return glovoapi.CompactLocationSearch(raw, a.intOr("limit", 5)), nil
		},
		"glovo_select_location": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			return c.SelectLocation(ctx, a.str("place_id"), a.str("provider"))
		},
		"glovo_browse_stores": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			prev := []string{}
			if ids, ok := a["previous_store_ids"].([]any); ok {
				for _, id := range ids {
					prev = append(prev, glovoapi.JSString(id))
				}
			}
			raw, err := c.BrowseStores(ctx, glovoapi.BrowseStoresParams{
				CategoryID:       a.intOr("category_id", 4),
				Offset:           a.intOr("offset", 0),
				Limit:            a.intOr("limit", 24),
				PreviousStoreIDs: prev,
			})
			if err != nil {
				return nil, err
			}
			return glovoapi.CompactStoreWall(raw)
		},
		"glovo_get_suggestions": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			return b.suggestions(ctx, c, a)
		},
		"glovo_get_store": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.GetStore(ctx, a.str("store"))
			if err != nil {
				return nil, err
			}
			return glovoapi.CompactStoreEntry(raw), nil
		},
		"glovo_get_store_menu": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.GetStoreMenu(ctx, a.str("store_id"), a.str("store_address_id"), glovoapi.MenuOptions{})
			if err != nil {
				return nil, err
			}
			return glovoapi.CompactMenu(raw, a.intOr("limit", 80))
		},
		"glovo_browse_store_catalog": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.GetStoreCatalog(ctx, a.str("store_id"), a.str("store_address_id"), a.str("content_uri"), true)
			if err != nil {
				return nil, err
			}
			return glovoapi.CompactStoreContent(raw, glovoapi.StoreContentCompactOptions{
				Kind: "all", Limit: a.intOr("limit", 40), StoreID: a.raw("store_id"), StoreAddressID: a.raw("store_address_id"),
			})
		},
		"glovo_get_store_recommendations": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.GetStoreContent(ctx, a.str("store_id"), a.str("store_address_id"), glovoapi.StoreContentOptions{Auth: true})
			if err != nil {
				return nil, err
			}
			kind := a.str("kind")
			if kind == "" {
				kind = "easy_reorder"
			}
			return glovoapi.CompactStoreContent(raw, glovoapi.StoreContentCompactOptions{Kind: kind, Limit: a.intOr("limit", 40)})
		},
		"glovo_get_store_order_options": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			translation := a.str("translation")
			if translation == "" {
				translation = "en"
			}
			return c.StoreOrderOptions(ctx, a.str("store_id"), a.str("store_address_id"), translation, a.intOr("similar_limit", 5))
		},
		"glovo_search_store_items": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.SearchStoreItems(ctx, a.str("store_id"), a.str("store_address_id"), a.str("query"))
			if err != nil {
				return nil, err
			}
			return glovoapi.CompactSearch(raw, glovoapi.SearchOptions{StoreID: a.str("store_id"), StoreAddressID: a.raw("store_address_id"), Limit: a.intOr("limit", 24)})
		},
		"glovo_get_product": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.GetProduct(ctx, glovoapi.ProductParams{
				StoreID: a.str("store_id"), StoreAddressID: a.str("store_address_id"), ProductID: a.str("product_id"),
				ExternalID: a.truthyStr("external_id"), CategoryID: a.raw("category_id"), Quantity: a.intOr("quantity", 1),
			})
			if err != nil {
				return nil, err
			}
			view, err := glovoapi.CompactProductView(raw)
			if err != nil {
				return nil, err
			}
			// {store_id, store_address_id, ...compactProductView(...)}
			out, err := toObject(view)
			if err != nil {
				return nil, err
			}
			if _, ok := out["store_id"]; !ok {
				out["store_id"] = a.str("store_id")
			}
			if _, ok := out["store_address_id"]; !ok {
				out["store_address_id"] = a.raw("store_address_id")
			}
			return out, nil
		},
		"glovo_get_basket": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			if glovoapi.Truthy(a.raw("store_id")) {
				raw, err := c.GetBasketByStore(ctx, a.str("store_id"))
				if err != nil {
					return nil, err
				}
				return glovoapi.CompactBasket(raw)
			}
			return c.GetBaskets(ctx)
		},
		"glovo_get_purchase_history": purchaseHistory,
		"glovo_get_order_items": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.GetOrder(ctx, a.str("order_id"))
			if err != nil {
				return nil, err
			}
			return glovoapi.CompactOrderDetail(raw)
		},
		"glovo_preview_reorder": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			raw, err := c.GetOrder(ctx, a.str("order_id"))
			if err != nil {
				return nil, err
			}
			return glovoapi.CompactReorderPreview(raw)
		},
		"glovo_plan_reorder": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			plan, err := glovologic.PlanReorder(ctx, source{c}, a.str("order_id"), glovologic.PlanOptions{
				MaxSearches: a.intOr("max_searches", 5), CandidatesPerLine: a.intOr("candidates_per_line", 3),
			})
			if err == nil && plan != nil && b.readOnly() {
				plan.NextStep = ReadOnlyNextStep
			}
			return plan, err
		},
		"glovo_get_order_stats": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			return glovologic.GetOrderStats(ctx, source{c}, glovologic.StatsOptions{
				MaxPages: a.intOr("max_pages", 0), PageDelay: a.ms("page_delay_ms", 750),
			})
		},
		"glovo_analyze_order_history": func(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
			return glovologic.AnalyzeOrderHistory(ctx, source{c}, glovologic.AnalyzeOptions{
				MaxPages: a.intOr("max_pages", 0), DetailLimit: a.intOr("detail_limit", 10),
				PageDelay: a.ms("page_delay_ms", 750), DetailDelay: a.ms("detail_delay_ms", 1000), Sleep: b.cfg.Sleep,
			})
		},
		"glovo_add_to_basket":      addToBasket,
		"glovo_set_quantity":       setQuantity,
		"glovo_remove_from_basket": removeFromBasket,
	}
}

func (b *Backend) suggestions(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
	in := glovologic.SuggestionInput{
		Mode: a.str("mode"), Query: a.str("query"), VenueQuery: a.str("venue_query"), ItemMode: a.str("item_mode"),
		KnownLikedOnly: a.boolean("known_liked_only"), QualityPreference: a.str("quality_preference"),
		IncludeGoogle: a.boolean("include_google_quality"), IncludeGoogleReviews: a.boolean("include_google_reviews"),
		HistoryRefresh: a.str("history_refresh"),
	}
	if f, ok := a["novelty_tolerance"].(float64); ok {
		in.NoveltyTolerance = &f
	}
	if f, ok := a["max_choices"].(float64); ok {
		n := int(f)
		in.MaxChoices = &n
	}
	st := c.AuthStatus()
	loc := c.Location()
	city, _ := loc["cityCode"].(string)
	sess := glovologic.SessionState{
		SignedIn: st.SignedIn, HasLocation: st.HasLocation,
		Location: glovologic.Location{Latitude: loc["latitude"], Longitude: loc["longitude"], CityCode: city},
	}
	if id := c.Session().CustomerID(); id != "" {
		sess.CustomerID = &id
	}
	return glovologic.GetSuggestions(ctx, source{c}, sess, in, glovologic.SuggestOptions{
		ReadOnly: b.readOnly(),
		Cache:    b.historyCache(), GoogleAPIKey: b.cfg.GoogleAPIKey, GoogleProvider: b.cfg.GoogleProvider, Sleep: b.cfg.Sleep,
	})
}

// purchaseHistory is server.mjs's glovo_get_purchase_history, including its
// shape probing of the page.
func purchaseHistory(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
	var cursor any = 0.0
	if a.has("offset") {
		cursor = a.raw("offset")
	}
	data, err := c.GetOrders(ctx, cursor, a.intOr("limit", 15))
	if err != nil {
		return nil, err
	}
	orders := nullishCoalesce(field(data, "orders"), field(field(data, "data"), "orders"), field(data, "data"), field(data, "elements"))
	if orders == nil {
		orders = []any{}
	}
	pagination := nullishCoalesce(field(data, "pagination"))
	out := object{
		{"raw_shape", nil}, {"cursor", cursor}, {"count", nil}, {"next_offset", nil}, {"pagination", nil}, {"orders", nil},
	}
	list, isList := orders.([]any)
	if isList {
		out.set("raw_shape", "array")
		out.set("count", len(list))
		compact := make([]any, len(list))
		for i, o := range list {
			co, err := glovoapi.CompactOrderCard(o)
			if err != nil {
				return nil, err
			}
			compact[i] = co
		}
		out.set("orders", compact)
	} else {
		out.set("raw_shape", jsTypeof(orders))
		out.drop("count")
		out.set("orders", orNull(data))
	}
	out.set("next_offset", orNull(nullishCoalesce(field(field(pagination, "next"), "offset"))))
	out.set("pagination", orNull(pagination))
	return out, nil
}

func addToBasket(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
	quantity := a.intOr("quantity", 1)
	selected, _ := a["selected_options"].([]any)
	var productView any
	// Upstream skips the product read only for validate_options: false with
	// no selected options.
	if a["validate_options"] != false || len(selected) > 0 {
		v, err := c.GetProduct(ctx, glovoapi.ProductParams{
			StoreID: a.str("store_id"), StoreAddressID: a.str("store_address_id"), ProductID: a.str("product_id"),
			ExternalID: a.truthyStr("external_id"), Quantity: quantity,
		})
		if err != nil {
			return nil, err
		}
		productView = v
	}
	var compactProduct *glovoapi.CompactProductViewResult
	if glovoapi.Truthy(productView) {
		cp, err := glovoapi.CompactProductView(productView)
		if err != nil {
			return nil, err
		}
		compactProduct = cp
	} else {
		productView = nil
	}
	storeCategoryID := a.str("store_category_id")
	if !a.has("store_category_id") {
		store, err := c.GetStore(ctx, a.str("store_id"))
		if err != nil {
			return nil, err
		}
		storeCategoryID = glovoapi.StoreCategoryIDFromStore(store)
	}
	externalID := a.str("external_id")
	storeProductID := a.str("store_product_id")
	if compactProduct != nil {
		if !a.has("external_id") {
			externalID = optString(compactProduct.ExternalID)
		}
		if !a.has("store_product_id") {
			storeProductID = optString(compactProduct.StoreProductID)
		}
	}
	opts := make([]glovoapi.SelectedOption, 0, len(selected))
	for _, s := range selected {
		o := args(s.(map[string]any))
		opts = append(opts, glovoapi.SelectedOption{GroupID: o.str("group_id"), OptionID: o.str("option_id"), Quantity: o.intOr("quantity", 0)})
	}
	raw, err := c.AddToBasket(ctx, glovoapi.AddToBasketParams{
		StoreID: a.str("store_id"), StoreAddressID: a.str("store_address_id"), StoreCategoryID: storeCategoryID,
		ProductID: a.str("product_id"), ExternalID: externalID, StoreProductID: storeProductID,
		Quantity: quantity, SelectedOptions: opts, ProductView: productView,
	})
	if err != nil {
		return nil, err
	}
	return glovoapi.CompactBasket(raw)
}

func lineRef(a args) glovoapi.BasketLineRef {
	return glovoapi.BasketLineRef{
		StoreID: a.str("store_id"), ProductID: a.truthyStr("product_id"),
		StoreProductID: a.truthyStr("store_product_id"), BasketProductID: a.truthyStr("basket_product_id"),
	}
}

func setQuantity(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
	raw, err := c.SetQuantity(ctx, lineRef(a), a.intOr("quantity", 0))
	if err != nil {
		return nil, err
	}
	return glovoapi.CompactBasket(raw)
}

func removeFromBasket(ctx context.Context, c *glovoapi.Client, a args) (any, error) {
	raw, err := c.RemoveFromBasket(ctx, lineRef(a))
	if err != nil {
		return nil, err
	}
	return glovoapi.CompactBasket(raw)
}

// optString is String(v) for a present, non-null value, else "".
func optString(v any) string {
	if isNullish(v) {
		return ""
	}
	return glovoapi.JSString(v)
}

func isNullish(v any) bool { return v == nil || v == glovoapi.Null }

func field(v any, k string) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return m[k]
}

// nullishCoalesce is a ?? b ?? ...; nil when all are nullish.
func nullishCoalesce(vs ...any) any {
	for _, v := range vs {
		if !isNullish(v) {
			return v
		}
	}
	return nil
}

func orNull(v any) any {
	if v == nil {
		return glovoapi.Null
	}
	return v
}

func jsTypeof(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	default:
		return "object"
	}
}

// object is a JSON object that keeps key order, like a JavaScript literal.
type object []kv

func (o *object) set(k string, v any) {
	for i := range *o {
		if (*o)[i].K == k {
			(*o)[i].V = v
			return
		}
	}
	*o = append(*o, kv{k, v})
}

func (o *object) drop(k string) {
	for i := range *o {
		if (*o)[i].K == k {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

func (o object) MarshalJSON() ([]byte, error) { return issue(o).MarshalJSON() }

func toObject(v any) (map[string]any, error) {
	b, err := marshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// errorText maps an error to upstream's isError text: an auth error's own
// message, "Not signed in yet" for a missing file, otherwise "Error: " and
// the message. Go messages follow Go style (lower case, no final period);
// the upstream sentence form is restored, except for API errors, whose text
// is the redacted response line exactly as upstream builds it.
func errorText(err error) string {
	var r refusal
	if errors.As(err, &r) {
		return string(r)
	}
	var authErr *glovoapi.AuthError
	if errors.As(err, &authErr) {
		return authErr.Msg
	}
	msg := err.Error()
	var apiErr *glovoapi.APIError
	if errors.As(err, &apiErr) {
		return "Error: " + msg
	}
	return "Error: " + sentence(msg)
}

func sentence(msg string) string {
	r, n := utf8.DecodeRuneInString(msg)
	if n == 0 {
		return msg
	}
	// An identifier at the start (storeCategoryId) keeps its case.
	first, _, _ := strings.Cut(msg, " ")
	if strings.ToLower(first) == first {
		msg = string(unicode.ToUpper(r)) + msg[n:]
	}
	last, _ := utf8.DecodeLastRuneInString(msg)
	if unicode.IsLetter(last) || unicode.IsDigit(last) {
		msg += "."
	}
	return msg
}
