package glovoapi

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// basketLineOptionCount mirrors basketLineOptionCount (api.mjs:1431).
func basketLineOptionCount(line Value) int {
	n := 0
	for _, k := range []string{"attributes", "selectedAttributes", "modifiers", "options", "customizations"} {
		v := get(line, k)
		if !truthy(v) {
			continue
		}
		if l, ok := v.([]any); ok {
			n += len(l)
		} else {
			n++
		}
	}
	return n
}

// BasketLine is one compactBasket line.
type BasketLine struct {
	BasketProductID      Value `json:"basket_product_id,omitempty"`
	ProductID            Value `json:"product_id,omitempty"`
	ExternalID           Value `json:"external_id,omitempty"`
	StoreProductID       Value `json:"store_product_id,omitempty"`
	Name                 Value `json:"name,omitempty"`
	Quantity             Value `json:"quantity,omitempty"`
	Price                Value `json:"price,omitempty"`
	Image                Value `json:"image,omitempty"`
	SelectedOptionsCount int   `json:"selected_options_count"`
	HasSelectedOptions   bool  `json:"has_selected_options"`
}

// CompactBasketResult is compactBasket (api.mjs:1052).
type CompactBasketResult struct {
	BasketID       Value        `json:"basket_id,omitempty"`
	BasketVersion  Value        `json:"basket_version,omitempty"`
	StoreID        Value        `json:"store_id,omitempty"`
	StoreAddressID Value        `json:"store_address_id,omitempty"`
	ProductsCount  Number       `json:"products_count"`
	Total          Value        `json:"total,omitempty"`
	Lines          []BasketLine `json:"lines"`
}

// CompactBasket is compactBasket (api.mjs:1052). A nil basket (no basket for
// the store) yields zero products and no lines.
func CompactBasket(basket Value) *CompactBasketResult {
	if !truthy(basket) {
		return &CompactBasketResult{Lines: []BasketLine{}}
	}
	out := &CompactBasketResult{
		BasketID:       get(basket, "basketId"),
		BasketVersion:  get(basket, "basketVersion"),
		StoreID:        get(basket, "storeId"),
		StoreAddressID: get(basket, "storeAddressId"),
		Total:          coalesce(get(basket, "basketPrice", "final", "formatted"), get(basket, "basketPrice", "final", "major")),
		Lines:          []BasketLine{},
	}
	var count float64
	for _, p := range orList(get(basket, "products")) {
		if inc := get(p, "quantity", "increments"); truthy(inc) {
			count += jsNumber(inc)
		}
		n := basketLineOptionCount(p)
		out.Lines = append(out.Lines, BasketLine{
			BasketProductID:      get(p, "ids", "basketProductId"),
			ProductID:            get(p, "ids", "id"),
			ExternalID:           get(p, "ids", "externalId"),
			StoreProductID:       get(p, "ids", "storeProductId"),
			Name:                 coalesce(get(p, "name"), get(p, "productName")),
			Quantity:             get(p, "quantity", "increments"),
			Price:                coalesce(get(p, "price", "final", "formatted"), get(p, "price", "final", "major")),
			Image:                get(p, "imageUrl"),
			SelectedOptionsCount: n,
			HasSelectedOptions:   n > 0,
		})
	}
	out.ProductsCount = Number(count)
	return out
}

// CompactOrder is compactOrder (api.mjs:1076): one order-history card.
type CompactOrder struct {
	OrderID Value  `json:"order_id,omitempty"`
	Store   Value  `json:"store,omitempty"`
	Status  Value  `json:"status,omitempty"`
	Date    Value  `json:"date,omitempty"`
	Total   Value  `json:"total,omitempty"`
	Items   []any  `json:"items"`
	URL     string `json:"url,omitempty"`
}

// CompactOrderCard is compactOrder (api.mjs:1076): nil for a falsy order.
func CompactOrderCard(o Value) *CompactOrder {
	if !truthy(o) {
		return nil
	}
	var cardItems []any
	for _, entry := range orList(get(o, "content", "body")) {
		text := coalesce(get(entry, "data"), get(entry, "text"), "")
		if !truthy(text) {
			continue
		}
		for _, line := range strings.Split(jsString(text), "\n") {
			if t := strings.TrimSpace(line); t != "" {
				cardItems = append(cardItems, t)
			}
		}
	}
	products := orList(or(get(o, "products"), get(o, "orderProducts"), get(o, "items")))
	if len(products) > 5 {
		products = products[:5]
	}
	items := []any{}
	for _, p := range products {
		items = append(items, coalesce(get(p, "name"), get(p, "productName"), get(p, "title")))
	}
	items = append(items, cardItems...)
	if len(items) > 5 {
		items = items[:5]
	}
	out := &CompactOrder{
		OrderID: coalesce(get(o, "id"), get(o, "orderId"), get(o, "orderCode")),
		Store:   coalesce(get(o, "storeName"), get(o, "store", "name"), get(o, "providerName"), get(o, "content", "title")),
		Status:  coalesce(get(o, "status"), get(o, "orderStatus")),
		Date:    coalesce(get(o, "creationTime"), get(o, "createdAt"), get(o, "orderTime"), get(o, "deliveredAt")),
		Total:   coalesce(get(o, "total", "formatted"), get(o, "totalAmount"), get(o, "price"), get(o, "footer", "left", "data")),
		Items:   items,
	}
	if id := coalesce(get(o, "id"), get(o, "orderId")); truthy(id) {
		out.URL = "https://glovoapp.com/en/profile/past-orders/" + jsString(id)
	}
	return out
}

type pastOrderLine struct {
	name, price, customizations, promotion Value
	quantity                               float64
	freeProduct                            bool
}

func orderLines(o Value) []any {
	return orList(or(get(o, "boughtProducts"), get(o, "products"), get(o, "orderProducts"), get(o, "items"), get(o, "order", "products")))
}

// pastOrderLines mirrors pastOrderLines (api.mjs:1125).
func pastOrderLines(o Value) []pastOrderLine {
	out := []pastOrderLine{}
	for _, line := range orderLines(o) {
		qv := coalesce(get(line, "quantity", "increments"), get(line, "quantity"), get(line, "amount"), 1)
		q := jsParseFloat(strings.Replace(jsString(qv), ",", ".", 1))
		if q == 0 || math.IsNaN(q) {
			q = 1
		}
		out = append(out, pastOrderLine{
			name:           orNull(coalesce(get(line, "name"), get(line, "productName"), get(line, "title"))),
			quantity:       q,
			price:          orNull(coalesce(get(line, "price", "formatted"), get(line, "price"))),
			customizations: orNull(coalesce(get(line, "customizationsDescription"), get(line, "description"))),
			freeProduct:    truthy(get(line, "freeProduct")),
			promotion:      orNull(get(line, "promotionDescription")),
		})
	}
	return out
}

var dateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04",
	"2006-01-02",
	time.RFC1123,
	time.RFC1123Z,
}

// orderDateMs mirrors orderDate (api.mjs:1137). Date.parse is approximated
// with ISO 8601 and RFC 1123 layouts.
func orderDateMs(o Value) (float64, bool) {
	v := firstValue(get(o, "creationTime"), get(o, "createdAt"), get(o, "orderTime"), get(o, "deliveredAt"), get(o, "currentStatus", "creationTime"))
	if nullish(v) {
		return 0, false
	}
	_, isNum := v.(json.Number)
	if isNum || reDigitsOnly.MatchString(jsString(v)) {
		n := jsNumber(v)
		if n < 10_000_000_000 {
			n *= 1000
		}
		return n, true
	}
	s := jsString(v)
	for _, layout := range dateLayouts {
		loc := time.UTC
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return float64(t.UnixMilli()), true
		}
	}
	return 0, false
}

// OrderDetailItem is one compactOrderDetail item.
type OrderDetailItem struct {
	Name           Value  `json:"name"`
	Quantity       Number `json:"quantity"`
	Price          Value  `json:"price"`
	Customizations Value  `json:"customizations"`
	FreeProduct    bool   `json:"free_product"`
	Promotion      Value  `json:"promotion"`
}

// PricingLine is one pricing_breakdown line.
type PricingLine struct {
	Type        Value `json:"type,omitempty"`
	Description Value `json:"description,omitempty"`
	Amount      Value `json:"amount,omitempty"`
	FinalAmount Value `json:"final_amount,omitempty"`
	Note        Value `json:"note,omitempty"`
}

// CompactOrderDetailResult is compactOrderDetail (api.mjs:1095).
type CompactOrderDetailResult struct {
	OrderID              Value             `json:"order_id,omitempty"`
	Store                Value             `json:"store,omitempty"`
	StoreID              Value             `json:"store_id,omitempty"`
	StoreAddressID       Value             `json:"store_address_id,omitempty"`
	StoreSlug            Value             `json:"store_slug,omitempty"`
	Status               Value             `json:"status,omitempty"`
	Date                 *string           `json:"date"`
	Total                Value             `json:"total,omitempty"`
	HandlingStrategy     Value             `json:"handling_strategy,omitempty"`
	NativeReorderAllowed bool              `json:"native_reorder_allowed"`
	CanGoToStore         bool              `json:"can_go_to_store"`
	Refunded             bool              `json:"refunded"`
	Items                []OrderDetailItem `json:"items"`
	PricingBreakdown     []PricingLine     `json:"pricing_breakdown"`
}

func nativeReorderAllowed(o Value) bool {
	return truthy(get(o, "reorderData", "allowed")) || truthy(get(o, "reorderData", "isAllowed")) || truthy(get(o, "isRemake"))
}

// CompactOrderDetail is compactOrderDetail (api.mjs:1095): nil for a falsy
// order.
func CompactOrderDetail(o Value) *CompactOrderDetailResult {
	if !truthy(o) {
		return nil
	}
	out := &CompactOrderDetailResult{
		OrderID:              coalesce(get(o, "id"), get(o, "orderId"), get(o, "orderCode")),
		Store:                coalesce(get(o, "storeName"), get(o, "store", "name"), get(o, "providerName")),
		StoreID:              coalesce(get(o, "storeId"), get(o, "store", "id")),
		StoreAddressID:       coalesce(get(o, "storeAddressId"), get(o, "store", "addressId")),
		StoreSlug:            coalesce(get(o, "storeSlug"), get(o, "store", "slug")),
		Status:               coalesce(get(o, "status"), get(o, "orderStatus"), get(o, "currentStatus", "type")),
		Total:                coalesce(get(o, "total", "formatted"), get(o, "totalAmount"), get(o, "price")),
		HandlingStrategy:     coalesce(get(o, "handlingStrategy", "type"), get(o, "handlingStrategy")),
		NativeReorderAllowed: nativeReorderAllowed(o),
		CanGoToStore:         truthy(get(o, "canGoToStore")),
		Refunded:             truthy(get(o, "refunded")),
		Items:                []OrderDetailItem{},
		PricingBreakdown:     []PricingLine{},
	}
	if ms, ok := orderDateMs(o); ok && !math.IsNaN(ms) {
		s := time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05.000Z")
		out.Date = &s
	}
	for _, l := range pastOrderLines(o) {
		out.Items = append(out.Items, OrderDetailItem{
			Name: l.name, Quantity: Number(l.quantity), Price: l.price,
			Customizations: l.customizations, FreeProduct: l.freeProduct, Promotion: l.promotion,
		})
	}
	for _, line := range orList(get(o, "pricingBreakdown", "lines")) {
		out.PricingBreakdown = append(out.PricingBreakdown, PricingLine{
			Type:        clean(get(line, "type")),
			Description: clean(get(line, "description")),
			Amount:      clean(get(line, "amount")),
			FinalAmount: clean(get(line, "finalAmount")),
			Note:        clean(get(line, "note")),
		})
	}
	return out
}

// ReorderPreviewItem is one compactReorderPreview item.
type ReorderPreviewItem struct {
	Index                int      `json:"index"`
	Name                 Value    `json:"name"`
	Quantity             Number   `json:"quantity"`
	HasProductIdentifier bool     `json:"has_product_identifier"`
	HasSelectedOptions   bool     `json:"has_selected_options"`
	CanPrepareLine       bool     `json:"can_prepare_line"`
	UnsupportedReasons   []string `json:"unsupported_reasons"`
}

// ReorderPreview is compactReorderPreview (api.mjs:1268). It is read-only.
type ReorderPreview struct {
	OrderID              Value                `json:"order_id,omitempty"`
	Store                Value                `json:"store"`
	NativeReorderAllowed bool                 `json:"native_reorder_allowed"`
	CanGoToStore         bool                 `json:"can_go_to_store"`
	HasStoreID           bool                 `json:"has_store_id"`
	HasStoreAddressID    bool                 `json:"has_store_address_id"`
	ItemsCount           int                  `json:"items_count"`
	CanPrepareBasket     bool                 `json:"can_prepare_basket"`
	UnsupportedReasons   []string             `json:"unsupported_reasons"`
	Limitation           string               `json:"limitation"`
	Items                []ReorderPreviewItem `json:"items"`

	empty bool
}

// MarshalJSON writes the three-field upstream shape for an empty order.
func (r ReorderPreview) MarshalJSON() ([]byte, error) {
	if r.empty {
		return marshalJSON(struct {
			CanPrepareBasket   bool                 `json:"can_prepare_basket"`
			Items              []ReorderPreviewItem `json:"items"`
			UnsupportedReasons []string             `json:"unsupported_reasons"`
		}{false, []ReorderPreviewItem{}, r.UnsupportedReasons})
	}
	type plain ReorderPreview
	return marshalJSON(plain(r))
}

// CompactReorderPreview is compactReorderPreview (api.mjs:1268).
func CompactReorderPreview(o Value) *ReorderPreview {
	if !truthy(o) {
		return &ReorderPreview{empty: true, Items: []ReorderPreviewItem{}, UnsupportedReasons: []string{"Order detail was empty."}}
	}
	storeID := coalesce(get(o, "storeId"), get(o, "store", "id"), get(o, "store", "storeId"))
	storeAddressID := coalesce(get(o, "storeAddressId"), get(o, "store", "addressId"), get(o, "store", "storeAddressId"))
	out := &ReorderPreview{
		OrderID:              coalesce(get(o, "id"), get(o, "orderId"), get(o, "orderCode"), get(o, "code")),
		Store:                orNull(coalesce(get(o, "storeName"), get(o, "store", "name"), get(o, "providerName"))),
		NativeReorderAllowed: nativeReorderAllowed(o),
		CanGoToStore:         truthy(get(o, "canGoToStore")),
		HasStoreID:           truthy(storeID),
		HasStoreAddressID:    truthy(storeAddressID),
		Items:                []ReorderPreviewItem{},
		UnsupportedReasons:   []string{},
	}
	seen := map[string]bool{}
	allOK := true
	for i, line := range orderLines(o) {
		ids := get(line, "ids")
		productID := coalesce(get(line, "id"), get(line, "productId"), get(line, "product", "id"), get(ids, "id"))
		externalID := coalesce(get(line, "externalId"), get(line, "productExternalId"), get(ids, "externalId"))
		storeProductID := coalesce(get(line, "storeProductId"), get(ids, "storeProductId"))
		optionCount := basketLineOptionCount(line)
		reasons := []string{}
		if !truthy(storeID) {
			reasons = append(reasons, "missing_store_id")
		}
		if !truthy(storeAddressID) {
			reasons = append(reasons, "missing_store_address_id")
		}
		hasID := truthy(productID) || truthy(storeProductID) || truthy(externalID)
		if !hasID {
			reasons = append(reasons, "missing_product_identifier")
		}
		if optionCount > 0 {
			reasons = append(reasons, "option_payload_requires_live_revalidation")
		}
		qv := coalesce(get(line, "quantity", "increments"), get(line, "quantity"), get(line, "amount"), 1)
		q, ok := jsParseInt(jsString(qv))
		if !ok || q == 0 {
			q = 1
		}
		out.Items = append(out.Items, ReorderPreviewItem{
			Index:                i,
			Name:                 orNull(coalesce(get(line, "name"), get(line, "productName"), get(line, "title"))),
			Quantity:             Number(q),
			HasProductIdentifier: hasID,
			HasSelectedOptions:   optionCount > 0,
			CanPrepareLine:       len(reasons) == 0,
			UnsupportedReasons:   reasons,
		})
		if len(reasons) > 0 {
			allOK = false
		}
		for _, r := range reasons {
			if !seen[r] {
				seen[r] = true
				out.UnsupportedReasons = append(out.UnsupportedReasons, r)
			}
		}
	}
	if len(out.Items) == 0 {
		out.UnsupportedReasons = append(out.UnsupportedReasons, "order_detail_has_no_product_lines")
	}
	out.ItemsCount = len(out.Items)
	out.CanPrepareBasket = len(out.Items) > 0 && allOK
	if len(out.UnsupportedReasons) > 0 {
		out.Limitation = "Read-only preview only. This order detail does not expose enough stable current product/option identifiers for a lossless basket rebuild."
	} else {
		out.Limitation = "Preview has candidate identifiers, but any future basket preparation must revalidate live product availability and options before mutating the basket."
	}
	return out
}
