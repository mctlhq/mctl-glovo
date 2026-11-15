package glovoapi

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"
)

// Fields typed Value are passed through from the response as upstream does.
// With omitempty, an absent upstream field (undefined) is omitted and a null
// one is written as null, matching JSON.stringify.

var reHTTP = regexp.MustCompile(`^https?://`)

// imageURL mirrors imageUrl (api.mjs:35).
func imageURL(source Value) string {
	if !truthy(source) {
		return ""
	}
	s := jsString(source)
	if reHTTP.MatchString(s) {
		return s
	}
	return "https://glovo.dhmedia.io/image/" + s
}

// availability mirrors availability (api.mjs:41); nil is unknown.
func availability(v Value) *bool {
	if !truthy(v) {
		return nil
	}
	is := func(key string, want bool) bool {
		b, ok := get(v, key).(bool)
		return ok && b == want
	}
	if is("available", true) || is("isAvailable", true) || is("enabled", true) || is("outOfStock", false) {
		return boolPtr(true)
	}
	if is("available", false) || is("isAvailable", false) || is("enabled", false) || is("disabled", true) || is("soldOut", true) || is("outOfStock", true) {
		return boolPtr(false)
	}
	st := coalesce(get(v, "availability", "status"), get(v, "status"))
	status := ""
	if !nullish(st) {
		status = strings.ToUpper(jsString(st))
	}
	switch status {
	case "AVAILABLE", "OPEN", "ENABLED":
		return boolPtr(true)
	case "UNAVAILABLE", "CLOSED", "DISABLED", "SOLD_OUT":
		return boolPtr(false)
	}
	return nil
}

// StoreClassFromCategoryID mirrors storeClassFromCategoryId (api.mjs:675).
func StoreClassFromCategoryID(categoryID Value) string {
	switch jsNumber(categoryID) {
	case 1:
		return "restaurant"
	case 4:
		return "grocery"
	case 3:
		return "retail"
	}
	return ""
}

// StoreCategoryIDFromStore mirrors storeCategoryIdFromStore (api.mjs:1330).
// It returns a string; a missing or null category is "", never "null", so
// that createBasket refuses it as upstream does (api.mjs:432).
func StoreCategoryIDFromStore(store Value) string {
	v := coalesce(get(store, "categoryId"), get(store, "data", "categoryId"), get(store, "store", "categoryId"))
	if nullish(v) {
		return ""
	}
	return jsString(v)
}

// CompactStore is compactStore (api.mjs:648).
type CompactStore struct {
	ID             string `json:"id"`
	StoreID        string `json:"store_id"`
	StoreAddressID Value  `json:"store_address_id,omitempty"`
	Name           Value  `json:"name,omitempty"`
	Slug           Value  `json:"slug,omitempty"`
	Category       Value  `json:"category,omitempty"`
	CategoryID     Value  `json:"category_id,omitempty"`
	StoreClass     string `json:"store_class,omitempty"`
	Open           Value  `json:"open"`
	ETA            Value  `json:"eta,omitempty"`
	Distance       Value  `json:"distance,omitempty"`
	Rating         Value  `json:"rating,omitempty"`
	Votes          Value  `json:"votes,omitempty"`
	DeliveryFee    Value  `json:"delivery_fee,omitempty"`
	ServiceFee     Value  `json:"service_fee,omitempty"`
	Image          string `json:"image,omitempty"`
	Logo           string `json:"logo,omitempty"`
	URL            string `json:"url,omitempty"`
}

// CompactStoreEntry is compactStore: nil for a falsy entry.
func CompactStoreEntry(entry Value) *CompactStore {
	if !truthy(entry) {
		return nil
	}
	id := jsString(coalesce(get(entry, "id"), get(entry, "storeId")))
	categoryID := coalesce(get(entry, "categoryId"), get(entry, "category", "id"))
	s := &CompactStore{
		ID:             id,
		StoreID:        id,
		StoreAddressID: coalesce(get(entry, "metadata", "storeAddressId"), get(entry, "addressId"), get(entry, "storeAddressId")),
		Name:           coalesce(get(entry, "title"), get(entry, "name")),
		Slug:           get(entry, "slug"),
		Category:       coalesce(get(entry, "tag"), get(entry, "category")),
		CategoryID:     categoryID,
		StoreClass:     StoreClassFromCategoryID(categoryID),
		Open:           coalesce(get(entry, "open"), get(entry, "availability", "status") == "OPEN"),
		ETA:            get(entry, "eta"),
		Distance:       get(entry, "distance"),
		Rating:         coalesce(get(entry, "ratings", "score"), get(entry, "rating")),
		Votes:          get(entry, "ratings", "votes"),
		DeliveryFee:    coalesce(get(entry, "deliveryFee", "effectiveFee"), get(entry, "deliveryFee", "baseFee"), get(entry, "deliveryFeeInfo", "fee")),
		ServiceFee:     get(entry, "serviceFee"),
		Image:          imageURL(coalesce(get(entry, "image", "lightImage"), get(entry, "imageId"))),
		Logo:           imageURL(coalesce(get(entry, "logo", "lightImage"), get(entry, "logoImageId"))),
	}
	if slug := get(entry, "slug"); truthy(slug) {
		s.URL = "https://glovoapp.com/en/es/barcelona/stores/" + jsString(slug)
	}
	return s
}

// StoreWallWidget is one compactStoreWall widget.
type StoreWallWidget struct {
	ID         Value `json:"id,omitempty"`
	Title      Value `json:"title,omitempty"`
	TemplateID Value `json:"template_id,omitempty"`
}

// CompactStoreWallResult is compactStoreWall (api.mjs:880).
type CompactStoreWallResult struct {
	Pagination Value              `json:"pagination,omitempty"`
	Count      int                `json:"count"`
	Stores     []*CompactStore    `json:"stores"`
	Category   Value              `json:"category,omitempty"`
	Widgets    *[]StoreWallWidget `json:"widgets,omitempty"`
}

// CompactStoreWall is compactStoreWall (api.mjs:880).
func CompactStoreWall(data Value) (out *CompactStoreWallResult, err error) {
	defer catchShape(&err)
	return compactStoreWall(data), nil
}

func compactStoreWall(data Value) *CompactStoreWallResult {
	stores := coalesce(get(data, "data", "stores"), get(data, "stores"))
	category := coalesce(get(data, "data", "category"), get(data, "category"))
	categoryID := coalesce(get(category, "id"), get(category, "categoryId"))
	entries := strictList(get(stores, "entries"), "stores.entries")
	out := &CompactStoreWallResult{
		Pagination: get(stores, "pagination"),
		Count:      len(entries),
		Stores:     []*CompactStore{},
		Category:   category,
	}
	for _, e := range entries {
		if !nullish(categoryID) {
			m := map[string]any{}
			if em, ok := e.(map[string]any); ok {
				for k, v := range em {
					m[k] = v
				}
			}
			m["categoryId"] = coalesce(get(e, "categoryId"), categoryID)
			e = m
		}
		out.Stores = append(out.Stores, CompactStoreEntry(e))
	}
	if raw := get(data, "data", "widgets"); !nullish(raw) {
		ws := strictList(raw, "data.widgets")
		widgets := make([]StoreWallWidget, 0, len(ws))
		for _, w := range ws {
			widgets = append(widgets, StoreWallWidget{ID: get(w, "id"), Title: get(w, "title"), TemplateID: get(w, "templateId")})
		}
		out.Widgets = &widgets
	}
	return out
}

// MenuSection is one compactMenu node.
type MenuSection struct {
	Name       Value  `json:"name,omitempty"`
	Slug       Value  `json:"slug,omitempty"`
	Depth      int    `json:"depth"`
	Path       string `json:"path"`
	ContentURI Value  `json:"content_uri,omitempty"`
	Tracking   Value  `json:"tracking,omitempty"`
}

// CompactMenuResult is compactMenu (api.mjs:893).
type CompactMenuResult struct {
	Type     Value         `json:"type,omitempty"`
	Count    int           `json:"count"`
	Sections []MenuSection `json:"sections"`
}

// CompactMenu is compactMenu (api.mjs:893). A zero limit means 80.
func CompactMenu(menu Value, limit int) (out *CompactMenuResult, err error) {
	defer catchShape(&err)
	return compactMenu(menu, limit), nil
}

func compactMenu(menu Value, limit int) *CompactMenuResult {
	if limit == 0 {
		limit = 80
	}
	result := []MenuSection{}
	var walk func(items []any, depth int, parent []Value)
	walk = func(items []any, depth int, parent []Value) {
		for _, item := range items {
			if len(result) >= limit {
				return
			}
			name := get(item, "name")
			chain := append(append([]Value{}, parent...), name)
			parts := []string{}
			for _, n := range chain {
				if truthy(n) {
					parts = append(parts, jsString(n))
				}
			}
			result = append(result, MenuSection{
				Name:       name,
				Slug:       get(item, "slug"),
				Depth:      depth,
				Path:       strings.Join(parts, " > "),
				ContentURI: get(item, "action", "data", "path"),
				Tracking:   get(item, "tracking"),
			})
			walk(strictList(or(get(item, "elements"), Null), "elements"), depth+1, chain)
		}
	}
	walk(strictList(or(get(menu, "data", "elements"), get(menu, "elements"), Null), "data.elements"), 0, nil)
	return &CompactMenuResult{Type: get(menu, "type"), Count: len(result), Sections: result}
}

// CompactItem is compactItem (api.mjs:913) with the extras its callers add.
type CompactItem struct {
	ID             string `json:"id"`
	ProductID      string `json:"product_id"`
	ExternalID     Value  `json:"external_id,omitempty"`
	StoreProductID Value  `json:"store_product_id,omitempty"`
	Name           Value  `json:"name,omitempty"`
	Price          Value  `json:"price,omitempty"`
	Amount         Value  `json:"amount,omitempty"`
	Image          Value  `json:"image,omitempty"`
	Sponsored      Value  `json:"sponsored,omitempty"`
	Restricted     Value  `json:"restricted,omitempty"`
	Available      *bool  `json:"available"`
	QuantityLimit  Value  `json:"quantity_limit,omitempty"`
	CategoryID     Value  `json:"category_id,omitempty"`

	// Extras set by compactSearch and compactStoreContent.
	StoreID        Value  `json:"store_id,omitempty"`
	StoreAddressID Value  `json:"store_address_id,omitempty"`
	Description    Value  `json:"description,omitempty"`
	CollectionType Value  `json:"collection_type,omitempty"`
	OrderedBefore  *bool  `json:"ordered_before,omitempty"`
	Source         string `json:"source,omitempty"`

	// PriceInfo is the raw priceInfo object, kept for promotion parsing. It is
	// not part of the tool output.
	// TODO(#7 slice 3): parse discount fields once real field names are confirmed
	PriceInfo json.RawMessage `json:"-"`
}

func rawJSON(v Value) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := marshalJSON(v)
	if err != nil {
		return nil
	}
	return b
}

// CompactItemOf is compactItem (api.mjs:913): nil for a falsy product.
func CompactItemOf(p Value) *CompactItem {
	if !truthy(p) {
		return nil
	}
	id := jsString(coalesce(get(p, "id"), get(p, "productId")))
	var fallbackPrice Value
	if price := get(p, "price"); !nullish(price) {
		fallbackPrice = "€" + jsToFixed2(jsNumber(price))
	}
	image := get(p, "imageUrl")
	if !truthy(image) {
		if u := imageURL(get(p, "imageId")); u != "" {
			image = u
		} else {
			image = nil
		}
	}
	it := &CompactItem{
		ID:             id,
		ProductID:      id,
		ExternalID:     coalesce(get(p, "externalId"), get(p, "productExternalId")),
		StoreProductID: get(p, "storeProductId"),
		Name:           get(p, "name"),
		Price:          coalesce(get(p, "priceInfo", "displayText"), fallbackPrice),
		Amount:         coalesce(get(p, "priceInfo", "amount"), get(p, "price")),
		Image:          image,
		Sponsored:      get(p, "sponsored"),
		Restricted:     get(p, "restricted"),
		Available:      availability(p),
		QuantityLimit:  get(p, "quantityLimit"),
		PriceInfo:      rawJSON(get(p, "priceInfo")),
	}
	if cat := get(p, "categoryId"); !nullish(cat) {
		it.CategoryID = cat
	}
	return it
}

// SearchOptions are the compactSearch options. StoreID is written as a
// string; StoreAddressID is passed through as given. A zero Limit means 24.
type SearchOptions struct {
	StoreID        Value
	StoreAddressID Value
	Limit          int
}

// CompactSearchResult is compactSearch (api.mjs:933).
type CompactSearchResult struct {
	Total   Value          `json:"total"`
	Count   int            `json:"count"`
	Results []*CompactItem `json:"results"`
}

// CompactSearch is compactSearch (api.mjs:933).
func CompactSearch(data Value, o SearchOptions) (out *CompactSearchResult, err error) {
	defer catchShape(&err)
	return compactSearch(data, o), nil
}

func compactSearch(data Value, o SearchOptions) *CompactSearchResult {
	limit := o.Limit
	if limit == 0 {
		limit = 24
	}
	products := []any{}
	for _, r := range strictList(or(get(data, "results"), Null), "results") {
		products = append(products, flatItems(or(get(r, "products"), Null), "results[].products")...)
	}
	n := len(products)
	if n > limit {
		n = limit
	}
	out := &CompactSearchResult{Total: coalesce(get(data, "totalProducts"), len(products)), Count: n, Results: []*CompactItem{}}
	for _, p := range products[:n] {
		it := CompactItemOf(p)
		if it != nil {
			it.StoreID = o.StoreID
			it.StoreAddressID = o.StoreAddressID
		}
		out.Results = append(out.Results, it)
	}
	return out
}

func productEventData(element Value) Value {
	for _, action := range strictList(or(get(element, "actions"), Null), "actions") {
		for _, event := range flatItems(or(get(action, "data", "events"), Null), "actions[].data.events") {
			d := get(event, "data")
			if truthy(get(d, "collectionType")) || truthy(get(d, "isOrderedBefore")) {
				return d
			}
		}
	}
	return map[string]any{}
}

var reNonLetters = regexp.MustCompile(`[^a-z]`)

func contentKind(v Value) string {
	s := ""
	if truthy(v) {
		s = jsString(v)
	}
	return reNonLetters.ReplaceAllString(strings.ToLower(s), "")
}

// StoreContentOptions for CompactStoreContent. Kind "" means "all"; a zero
// Limit means 40. StoreID is written as a string, StoreAddressID as given.
type StoreContentCompactOptions struct {
	Kind           string
	Limit          int
	StoreID        Value
	StoreAddressID Value
}

// ContentSection is one compactStoreContent section.
type ContentSection struct {
	Type           Value          `json:"type,omitempty"`
	Title          Value          `json:"title,omitempty"`
	Slug           Value          `json:"slug,omitempty"`
	CollectionType Value          `json:"collection_type"`
	Products       []*CompactItem `json:"products"`
}

// CompactStoreContentResult is compactStoreContent (api.mjs:953).
type CompactStoreContentResult struct {
	Kind              string           `json:"kind"`
	Count             int              `json:"count"`
	Sections          []ContentSection `json:"sections"`
	Products          []*CompactItem   `json:"products"`
	UnsupportedReason string           `json:"unsupported_reason,omitempty"`
}

// CompactStoreContent is compactStoreContent (api.mjs:953).
func CompactStoreContent(data Value, o StoreContentCompactOptions) (out *CompactStoreContentResult, err error) {
	defer catchShape(&err)
	return compactStoreContent(data, o), nil
}

func compactStoreContent(data Value, o StoreContentCompactOptions) *CompactStoreContentResult {
	kind := o.Kind
	if kind == "" {
		kind = "all"
	}
	wanted := contentKind(kind)
	limit := float64(o.Limit)
	if limit == 0 {
		limit = 40
	}
	remaining := int(math.Max(1, math.Min(limit, 100)))
	out := &CompactStoreContentResult{Kind: kind, Sections: []ContentSection{}, Products: []*CompactItem{}}
	for _, section := range strictList(or(get(data, "data", "body"), get(data, "body"), Null), "data.body") {
		var tiles []any
		for _, el := range strictList(or(get(section, "data", "elements"), Null), "data.body[].data.elements") {
			if get(el, "type") == "PRODUCT_TILE" && truthy(get(el, "data")) {
				tiles = append(tiles, el)
			}
		}
		if len(tiles) == 0 {
			continue
		}
		first := productEventData(tiles[0])
		collectionType := firstValue(get(first, "collectionType"), get(section, "data", "tracking", "collectionType"), get(section, "data", "title"), "products")
		normalized := contentKind(collectionType)
		if wanted != "all" && wanted != normalized {
			continue
		}
		source := "store_content"
		switch normalized {
		case "easyreorder":
			source = "easy_reorder"
		case "topsellers":
			source = "top_sellers"
		}
		if len(tiles) > remaining {
			tiles = tiles[:remaining]
		}
		products := make([]*CompactItem, 0, len(tiles))
		for _, el := range tiles {
			ev := productEventData(el)
			it := CompactItemOf(get(el, "data"))
			if !nullish(o.StoreID) {
				it.StoreID = jsString(o.StoreID)
			}
			if !nullish(o.StoreAddressID) {
				it.StoreAddressID = o.StoreAddressID
			}
			it.Description = get(el, "data", "description")
			it.CollectionType = firstValue(get(ev, "collectionType"), collectionType)
			ob := get(ev, "isOrderedBefore")
			it.OrderedBefore = boolPtr(ob != nil && strings.ToLower(jsString(ob)) == "true")
			it.Source = source
			products = append(products, it)
		}
		remaining -= len(products)
		out.Sections = append(out.Sections, ContentSection{
			Type:           get(section, "type"),
			Title:          get(section, "data", "title"),
			Slug:           get(section, "data", "slug"),
			CollectionType: collectionType,
			Products:       products,
		})
		if remaining == 0 {
			break
		}
	}
	for _, s := range out.Sections {
		out.Products = append(out.Products, s.Products...)
	}
	out.Count = len(out.Products)
	if out.Count == 0 {
		out.UnsupportedReason = "This content node exposes no product tiles. Use store item search for the live catalog."
	}
	return out
}

// BasketRange is one minimum-basket range.
type BasketRange struct {
	LowerBound        Value `json:"lower_bound,omitempty"`
	UpperBound        Value `json:"upper_bound,omitempty"`
	UpperBoundText    Value `json:"upper_bound_text,omitempty"`
	Surcharge         Value `json:"surcharge,omitempty"`
	SurchargeStrategy Value `json:"surcharge_strategy,omitempty"`
	Description       Value `json:"description,omitempty"`
	SuccessText       Value `json:"success_text,omitempty"`
}

// Restriction is one store restriction.
type Restriction struct {
	ID   Value `json:"id,omitempty"`
	Text Value `json:"text,omitempty"`
	Link Value `json:"link,omitempty"`
}

// StoreInfoSection is one store information section.
type StoreInfoSection struct {
	Type Value `json:"type,omitempty"`
	Text Value `json:"text,omitempty"`
}

// CompactStoreOrderOptionsResult is compactStoreOrderOptions (api.mjs:996).
type CompactStoreOrderOptionsResult struct {
	HandlingStrategy    string             `json:"handling_strategy"`
	FeeInformation      Value              `json:"fee_information,omitempty"`
	MinimumBasketRanges []BasketRange      `json:"minimum_basket_ranges"`
	RestrictionsTitle   Value              `json:"restrictions_title,omitempty"`
	Restrictions        []Restriction      `json:"restrictions"`
	StoreInformation    []StoreInfoSection `json:"store_information"`
	SimilarStores       []*CompactStore    `json:"similar_stores"`
	Boundary            string             `json:"boundary"`
}

// CompactStoreOrderOptions is compactStoreOrderOptions (api.mjs:996).
func CompactStoreOrderOptions(fees, restrictions, info, similar Value) (out *CompactStoreOrderOptionsResult, err error) {
	defer catchShape(&err)
	return compactStoreOrderOptions(fees, restrictions, info, similar), nil
}

func compactStoreOrderOptions(fees, restrictions, info, similar Value) *CompactStoreOrderOptionsResult {
	feeData := coalesce(get(fees, "data"), fees)
	if nullish(feeData) {
		feeData = map[string]any{}
	}
	out := &CompactStoreOrderOptionsResult{
		HandlingStrategy:    "DELIVERY",
		FeeInformation:      get(feeData, "feesInformation", "plainText"),
		MinimumBasketRanges: []BasketRange{},
		RestrictionsTitle:   get(restrictions, "title"),
		Restrictions:        []Restriction{},
		StoreInformation:    []StoreInfoSection{},
		SimilarStores:       []*CompactStore{},
		Boundary:            "Read-only pre-check. This does not create a basket, enter checkout, book a slot, pay, or place an order.",
	}
	for _, r := range strictList(or(get(feeData, "ranges"), Null), "ranges") {
		out.MinimumBasketRanges = append(out.MinimumBasketRanges, BasketRange{
			LowerBound:        clean(get(r, "lowerBound")),
			UpperBound:        clean(get(r, "upperBound")),
			UpperBoundText:    clean(get(r, "upperBoundInfo", "displayText")),
			Surcharge:         clean(get(r, "surchargeInfo", "displayText")),
			SurchargeStrategy: clean(get(r, "surchargeStrategy")),
			Description:       clean(get(r, "plainText")),
			SuccessText:       clean(get(r, "successText")),
		})
	}
	for _, r := range strictList(or(get(restrictions, "restrictions"), Null), "restrictions") {
		out.Restrictions = append(out.Restrictions, Restriction{
			ID:   clean(get(r, "id")),
			Text: clean(coalesce(get(r, "text"), get(r, "description"))),
			Link: clean(get(r, "hyperlink", "url")),
		})
	}
	for _, s := range strictList(or(get(info, "sections"), Null), "sections") {
		text := clean(coalesce(get(s, "data", "text"), get(s, "data", "label"), get(s, "data", "title")))
		if !truthy(text) {
			continue
		}
		out.StoreInformation = append(out.StoreInformation, StoreInfoSection{Type: clean(get(s, "type")), Text: text})
	}
	similarStores := coalesce(get(similar, "stores"), get(similar, "data", "stores"))
	for _, s := range strictList(similarStores, "stores") {
		out.SimilarStores = append(out.SimilarStores, CompactStoreEntry(s))
	}
	return out
}

// OptionChoice is one option of an OptionGroup.
type OptionChoice struct {
	ID         string `json:"id"`
	LegacyID   Value  `json:"legacyId,omitempty"`
	OptionID   Value  `json:"optionId,omitempty"`
	ExternalID Value  `json:"externalId,omitempty"`
	Name       Value  `json:"name,omitempty"`
	Price      Value  `json:"price,omitempty"`
}

// OptionGroup is one optionGroupsFromProduct group (api.mjs:1380).
type OptionGroup struct {
	ID              string         `json:"id"`
	GroupLegacyID   Value          `json:"groupLegacyId,omitempty"`
	GroupID         Value          `json:"groupId,omitempty"`
	GroupExternalID Value          `json:"groupExternalId,omitempty"`
	GroupPosition   Value          `json:"groupPosition,omitempty"`
	Name            Value          `json:"name,omitempty"`
	Required        bool           `json:"required"`
	Min             Value          `json:"min"`
	Max             Value          `json:"max"`
	Options         []OptionChoice `json:"options"`
}

func productMeta(data Value) Value {
	return coalesce(get(data, "data", "metadata", "product"), get(data, "metadata", "product"), get(data, "product"), data)
}

func requiredSections(data Value) []any {
	var out []any
	for _, e := range strictList(or(get(data, "data", "actions", "primary", "events"), Null), "data.actions.primary.events") {
		out = append(out, flatItems(or(get(e, "data", "requiredSections"), Null), "requiredSections")...)
	}
	return out
}

// OptionGroupsFromProduct is optionGroupsFromProduct (api.mjs:1380).
func OptionGroupsFromProduct(data Value) (groups []OptionGroup, err error) {
	defer catchShape(&err)
	return optionGroupsFromProduct(data), nil
}

func optionGroupsFromProduct(data Value) []OptionGroup {
	product := productMeta(data)
	required := map[string]bool{}
	for _, section := range requiredSections(data) {
		for _, v := range []Value{get(section, "id"), get(section, "sectionId"), get(section, "attributeGroupId"), get(section, "externalId"), section} {
			switch v.(type) {
			case nil, nullValue, map[string]any, []any:
				continue
			}
			required[jsString(v)] = true
		}
	}
	groups := []OptionGroup{}
	for _, g := range strictList(or(get(product, "attributeGroups"), get(product, "optionGroups"), Null), "attributeGroups") {
		id := jsString(coalesce(get(g, "id"), get(g, "attributeGroupId"), get(g, "externalId")))
		group := OptionGroup{
			ID:              id,
			GroupLegacyID:   get(g, "id"),
			GroupID:         get(g, "attributeGroupId"),
			GroupExternalID: get(g, "externalId"),
			GroupPosition:   get(g, "position"),
			Name:            get(g, "name"),
			Required: truthy(get(g, "required")) ||
				strictNumberEquals(get(g, "minSelection"), 1) ||
				jsNumber(get(g, "minSelections")) > 0 ||
				jsNumber(get(g, "min")) > 0 ||
				required[id] || required[jsString(get(g, "externalId"))],
			Min:     coalesce(get(g, "minSelection"), get(g, "minSelections"), get(g, "min"), 0),
			Max:     orNull(coalesce(get(g, "maxSelection"), get(g, "maxSelections"), get(g, "max"))),
			Options: []OptionChoice{},
		}
		for _, o := range strictList(or(get(g, "attributes"), get(g, "options"), get(g, "values"), Null), "attributeGroups[].attributes") {
			group.Options = append(group.Options, OptionChoice{
				ID:         jsString(coalesce(get(o, "id"), get(o, "attributeId"), get(o, "externalId"))),
				LegacyID:   get(o, "id"),
				OptionID:   get(o, "attributeId"),
				ExternalID: get(o, "externalId"),
				Name:       get(o, "name"),
				Price:      coalesce(get(o, "priceInfo", "displayText"), get(o, "price", "formatted"), get(o, "price")),
			})
		}
		groups = append(groups, group)
	}
	return groups
}

func productAddEnabled(data Value) *bool {
	elements := coalesce(get(data, "data", "footer", "data", "elements"), get(data, "footer", "data", "elements"))
	for _, el := range strictList(elements, "footer.data.elements") {
		if b, ok := get(el, "data", "isEnabled").(bool); ok {
			return boolPtr(b)
		}
	}
	return nil
}

// CompactProductViewResult is compactProductView (api.mjs:1026).
type CompactProductViewResult struct {
	CompactItem
	AddEnabled       *bool         `json:"add_enabled"`
	IsVariant        Value         `json:"is_variant"`
	VariantSelection string        `json:"variant_selection"`
	OptionGroups     []OptionGroup `json:"option_groups"`
	RequiredSections []any         `json:"required_sections"`
}

// CompactProductView is compactProductView (api.mjs:1026).
func CompactProductView(data Value) (out *CompactProductViewResult, err error) {
	defer catchShape(&err)
	return compactProductView(data), nil
}

func compactProductView(data Value) *CompactProductViewResult {
	meta := productMeta(data)
	fields := map[string]any{}
	for _, k := range []string{"id", "externalId", "storeProductId", "categoryId", "name", "priceInfo", "imageUrl", "imageId", "sponsored", "restricted"} {
		if v := get(meta, k); v != nil {
			fields[k] = v
		}
	}
	// api.mjs:1037 passes the raw priceInfo through; CompactItemOf keeps it
	// as json.RawMessage in CompactItem.PriceInfo.
	// TODO(#7 slice 3): parse discount fields once real field names are confirmed
	item := CompactItemOf(fields)
	addEnabled := productAddEnabled(data)
	item.Available = addEnabled
	groups := optionGroupsFromProduct(data)
	isVariant := get(meta, "isVariant")
	selection := "not_exposed"
	if len(groups) > 0 {
		selection = "customizations"
	} else if b, ok := isVariant.(bool); ok && b {
		selection = "separate_catalog_product"
	}
	sections := requiredSections(data)
	if sections == nil {
		sections = []any{}
	}
	return &CompactProductViewResult{
		CompactItem:      *item,
		AddEnabled:       addEnabled,
		IsVariant:        orNull(isVariant),
		VariantSelection: selection,
		OptionGroups:     groups,
		RequiredSections: sections,
	}
}
