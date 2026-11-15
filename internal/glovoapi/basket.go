package glovoapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
)

func (c *Client) customerID() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.session.CustomerID()
	if id == "" {
		return "", &AuthError{Msg: "No customer id in Glovo session. Run glovo_login again."}
	}
	return id, nil
}

// GetBaskets is getBaskets (api.mjs:417): every basket of the customer.
func (c *Client) GetBaskets(ctx context.Context) (Value, error) {
	id, err := c.customerID()
	if err != nil {
		return nil, err
	}
	return c.call(ctx, "/v1/authenticated/customers/"+seg(id)+"/baskets", callOpts{auth: true})
}

// GetBasketByStore is getBasketByStore (api.mjs:423). A 404 is Glovo's
// documented "no basket for this store" and returns nil.
func (c *Client) GetBasketByStore(ctx context.Context, storeID string) (Value, error) {
	id, err := c.customerID()
	if err != nil {
		return nil, err
	}
	return c.call(ctx, "/v1/authenticated/customers/"+seg(id)+"/baskets/stores/"+seg(storeID), callOpts{auth: true, allowNotFound: true})
}

var reDigitsOnly = regexp.MustCompile(`^\d+$`)

func asNumberIfNumeric(s string) Value {
	if reDigitsOnly.MatchString(s) {
		f, err := strconv.ParseFloat(s, 64)
		if err == nil {
			return json.Number(jsNumberString(f))
		}
	}
	return s
}

type createBasketBody struct {
	Products         []BasketProduct `json:"products"`
	StoreID          Value           `json:"storeId"`
	StoreAddressID   Value           `json:"storeAddressId"`
	StoreCategoryID  Value           `json:"storeCategoryId"`
	HandlingStrategy string          `json:"handlingStrategy"`
}

// createBasket is createBasket (api.mjs:429).
func (c *Client) createBasket(ctx context.Context, storeID, storeAddressID, storeCategoryID string, product BasketProduct) (Value, error) {
	id, err := c.customerID()
	if err != nil {
		return nil, err
	}
	// Upstream refuses a falsy or zero category (api.mjs:432). "null" and
	// "undefined" are what String() makes of a missing one; refuse them too.
	if storeCategoryID == "" || storeCategoryID == "null" || storeCategoryID == "undefined" || jsNumber(storeCategoryID) == 0 {
		return nil, errors.New("storeCategoryId is required for new basket creation")
	}
	body := createBasketBody{
		Products:         []BasketProduct{product},
		StoreID:          asNumberIfNumeric(storeID),
		StoreAddressID:   asNumberIfNumeric(storeAddressID),
		StoreCategoryID:  asNumberIfNumeric(storeCategoryID),
		HandlingStrategy: "DELIVERY",
	}
	return c.call(ctx, "/v1/authenticated/customers/"+seg(id)+"/baskets", callOpts{method: http.MethodPost, auth: true, body: body})
}

// updateBasketProducts is updateBasketProducts (api.mjs:446).
func (c *Client) updateBasketProducts(ctx context.Context, basketID string, payload any) (Value, error) {
	id, err := c.customerID()
	if err != nil {
		return nil, err
	}
	return c.call(ctx, "/v1/authenticated/customers/"+seg(id)+"/baskets/"+seg(basketID)+"/products", callOpts{method: http.MethodPut, auth: true, body: payload})
}

// QuantityPayload is the PATCH .../products/quantity body.
type QuantityPayload struct {
	HandlingStrategy Value          `json:"handlingStrategy"`
	BasketVersion    Value          `json:"basketVersion,omitempty"`
	Products         []QuantityLine `json:"products"`
}

// QuantityLine is one line of a QuantityPayload.
type QuantityLine struct {
	BasketProductID Value `json:"basketProductId"`
	Quantity        Value `json:"quantity"`
}

// updateProductQuantity is updateProductQuantity (api.mjs:456).
func (c *Client) updateProductQuantity(ctx context.Context, basketID string, payload QuantityPayload) (Value, error) {
	id, err := c.customerID()
	if err != nil {
		return nil, err
	}
	return c.call(ctx, "/v1/authenticated/customers/"+seg(id)+"/baskets/"+seg(basketID)+"/products/quantity", callOpts{method: http.MethodPatch, auth: true, body: payload})
}

// SelectedOption is one chosen modifier for AddToBasket.
type SelectedOption struct {
	GroupID  string
	OptionID string
	Quantity int // zero means 1
}

func (o SelectedOption) qty() int {
	if o.Quantity == 0 {
		return 1
	}
	return o.Quantity
}

// BasketProductIDs are the ids of a basket product payload.
type BasketProductIDs struct {
	ID             string `json:"id"`
	ExternalID     string `json:"externalId"`
	StoreProductID string `json:"storeProductId"`
}

// Increments is a Glovo quantity object.
type Increments struct {
	Increments Value `json:"increments"`
}

// BasketProduct is the basketProduct payload (api.mjs:1313).
type BasketProduct struct {
	IDs            BasketProductIDs `json:"ids"`
	Quantity       Increments       `json:"quantity"`
	Customizations []Customization  `json:"customizations,omitempty"`
}

// CustomizationIDs are the ids of one customization payload.
type CustomizationIDs struct {
	GroupLegacyID   Value `json:"groupLegacyId"`
	GroupID         Value `json:"groupId"`
	GroupExternalID Value `json:"groupExternalId"`
	GroupPosition   Value `json:"groupPosition"`
	LegacyID        Value `json:"legacyId"`
	ExternalID      Value `json:"externalId"`
	ID              Value `json:"id,omitempty"`
}

// Customization is one customizationPayload entry (api.mjs:1339).
type Customization struct {
	IDs               CustomizationIDs `json:"ids"`
	Name              Value            `json:"name,omitempty"`
	Quantity          Increments       `json:"quantity"`
	CustomizationName Value            `json:"customizationName,omitempty"`
	GroupName         Value            `json:"groupName,omitempty"`
}

func basketProduct(productID, externalID, storeProductID string, quantity int, selected []SelectedOption, productView Value) (BasketProduct, error) {
	if productID == "" {
		return BasketProduct{}, errors.New("productId is required for basket product payloads")
	}
	if externalID == "" {
		return BasketProduct{}, errors.New("externalId is required for basket product payloads")
	}
	if storeProductID == "" {
		return BasketProduct{}, errors.New("storeProductId is required for basket product payloads")
	}
	custom, err := customizationPayload(productView, selected)
	if err != nil {
		return BasketProduct{}, err
	}
	return BasketProduct{
		IDs:            BasketProductIDs{ID: productID, ExternalID: externalID, StoreProductID: storeProductID},
		Quantity:       Increments{Increments: quantity},
		Customizations: custom,
	}, nil
}

func requireValue(v Value, label string) (Value, error) {
	if nullish(v) {
		return nil, fmt.Errorf("missing %s for customization payload", label)
	}
	if s, ok := v.(string); ok && s == "" {
		return nil, fmt.Errorf("missing %s for customization payload", label)
	}
	return v, nil
}

func findGroup(groups []OptionGroup, id string) *OptionGroup {
	for i := range groups {
		if groups[i].ID == id {
			return &groups[i]
		}
	}
	return nil
}

func findOption(g *OptionGroup, id string) *OptionChoice {
	for i := range g.Options {
		if g.Options[i].ID == id {
			return &g.Options[i]
		}
	}
	return nil
}

func customizationPayload(productView Value, selected []SelectedOption) ([]Customization, error) {
	if len(selected) == 0 {
		return nil, nil
	}
	if nullish(productView) {
		return nil, errors.New("productView is required for customization payloads")
	}
	groups, err := OptionGroupsFromProduct(productView)
	if err != nil {
		return nil, err
	}
	out := make([]Customization, 0, len(selected))
	for _, s := range selected {
		g := findGroup(groups, s.GroupID)
		if g == nil {
			return nil, fmt.Errorf("unknown option group %s", s.GroupID)
		}
		o := findOption(g, s.OptionID)
		if o == nil {
			return nil, fmt.Errorf("unknown option %s for group %s", s.OptionID, s.GroupID)
		}
		var ids CustomizationIDs
		fields := []struct {
			dst   *Value
			src   Value
			label string
		}{
			{&ids.GroupLegacyID, g.GroupLegacyID, "groupLegacyId"},
			{&ids.GroupID, g.GroupID, "groupId"},
			{&ids.GroupExternalID, g.GroupExternalID, "groupExternalId"},
			{&ids.GroupPosition, g.GroupPosition, "groupPosition"},
			{&ids.LegacyID, o.LegacyID, "legacyId"},
			{&ids.ExternalID, o.ExternalID, "externalId"},
		}
		for _, f := range fields {
			if *f.dst, err = requireValue(f.src, f.label); err != nil {
				return nil, err
			}
		}
		if clean(o.OptionID) != nil {
			ids.ID = o.OptionID
		}
		out = append(out, Customization{
			IDs:               ids,
			Name:              o.Name,
			Quantity:          Increments{Increments: s.qty()},
			CustomizationName: o.Name,
			GroupName:         g.Name,
		})
	}
	return out, nil
}

// ValidateSelectedOptions is validateSelectedOptions (api.mjs:1412).
func ValidateSelectedOptions(productView Value, selected []SelectedOption) error {
	groups, err := OptionGroupsFromProduct(productView)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, s := range selected {
		g := findGroup(groups, s.GroupID)
		if g == nil {
			return fmt.Errorf("unknown option group %s", s.GroupID)
		}
		if findOption(g, s.OptionID) == nil {
			return fmt.Errorf("unknown option %s for group %s", s.OptionID, s.GroupID)
		}
		counts[s.GroupID] += s.qty()
	}
	for i := range groups {
		g := &groups[i]
		count := float64(counts[g.ID])
		label := g.ID
		if truthy(g.Name) {
			label = jsString(g.Name)
		}
		minimum := 1.0
		if truthy(g.Min) {
			minimum = jsNumber(g.Min)
		}
		if g.Required && count < minimum {
			return fmt.Errorf("missing required option group: %s", label)
		}
		if !nullish(g.Max) && count > jsNumber(g.Max) {
			return fmt.Errorf("too many options for group: %s", label)
		}
	}
	return nil
}

// BasketLineRef identifies a basket line by any of its ids.
type BasketLineRef struct {
	StoreID         string
	ProductID       string
	StoreProductID  string
	BasketProductID string
}

// findBasketProduct is findBasketProduct (api.mjs:1369): ids must be equal
// strings, as upstream compares with ===. A products field that is not a
// list is an error.
func findBasketProduct(basket Value, ref BasketLineRef) (found Value, err error) {
	defer catchShape(&err)
	match := func(v Value, want string) bool {
		s, ok := v.(string)
		return want != "" && ok && s == want
	}
	for _, p := range strictList(or(get(basket, "products"), Null), "products") {
		ids := get(p, "ids")
		if match(get(ids, "basketProductId"), ref.BasketProductID) ||
			match(get(ids, "id"), ref.ProductID) ||
			match(get(ids, "storeProductId"), ref.StoreProductID) {
			return p, nil
		}
	}
	return nil, nil
}

// AddToBasketParams are the addToBasket arguments. A zero Quantity means 1.
// ProductView is the raw GetProduct response used to validate and build
// customizations; nil skips validation.
type AddToBasketParams struct {
	StoreID         string
	StoreAddressID  string
	StoreCategoryID string
	ProductID       string
	ExternalID      string
	StoreProductID  string
	Quantity        int
	SelectedOptions []SelectedOption
	ProductView     Value
}

func handlingStrategy(basket Value) Value {
	if hs := get(basket, "handlingStrategy"); truthy(hs) {
		return hs
	}
	return "DELIVERY"
}

// AddToBasket is addToBasket (api.mjs:476): create the store basket, bump an
// existing line, or append a new line.
func (c *Client) AddToBasket(ctx context.Context, p AddToBasketParams) (Value, error) {
	if p.Quantity == 0 {
		p.Quantity = 1
	}
	if !nullish(p.ProductView) {
		if err := ValidateSelectedOptions(p.ProductView, p.SelectedOptions); err != nil {
			return nil, err
		}
	}
	product, err := basketProduct(p.ProductID, p.ExternalID, p.StoreProductID, p.Quantity, p.SelectedOptions, p.ProductView)
	if err != nil {
		return nil, err
	}
	basket, err := c.GetBasketByStore(ctx, p.StoreID)
	if err != nil {
		return nil, err
	}
	if !truthy(basket) {
		return c.createBasket(ctx, p.StoreID, p.StoreAddressID, p.StoreCategoryID, product)
	}
	existing, err := findBasketProduct(basket, BasketLineRef{ProductID: p.ProductID, StoreProductID: p.StoreProductID})
	if err != nil {
		return nil, err
	}
	if bpID := get(existing, "ids", "basketProductId"); truthy(bpID) {
		current := 0.0
		if inc := get(existing, "quantity", "increments"); truthy(inc) {
			current = jsNumber(inc)
		}
		return c.updateProductQuantity(ctx, jsString(get(basket, "basketId")), QuantityPayload{
			HandlingStrategy: handlingStrategy(basket),
			BasketVersion:    get(basket, "basketVersion"),
			Products:         []QuantityLine{{BasketProductID: bpID, Quantity: numberValue(current + float64(p.Quantity))}},
		})
	}
	m, _ := basket.(map[string]any)
	payload := make(map[string]any, len(m)+1)
	for k, v := range m {
		payload[k] = v
	}
	var products []any
	if err := guardShape(func() { products = append([]any{}, strictList(or(get(basket, "products"), Null), "products")...) }); err != nil {
		return nil, err
	}
	payload["products"] = append(products, product)
	return c.updateBasketProducts(ctx, jsString(get(basket, "basketId")), payload)
}

func (c *Client) patchLineQuantity(ctx context.Context, ref BasketLineRef, quantity int) (Value, error) {
	basket, err := c.GetBasketByStore(ctx, ref.StoreID)
	if err != nil {
		return nil, err
	}
	if !truthy(get(basket, "basketId")) {
		return nil, fmt.Errorf("no active basket for store %s", ref.StoreID)
	}
	var id Value
	if ref.BasketProductID != "" {
		id = ref.BasketProductID
	} else {
		line, err := findBasketProduct(basket, ref)
		if err != nil {
			return nil, err
		}
		id = get(line, "ids", "basketProductId")
	}
	if !truthy(id) {
		return nil, errors.New("product is not in the basket")
	}
	if quantity < 0 {
		quantity = 0
	}
	return c.updateProductQuantity(ctx, jsString(get(basket, "basketId")), QuantityPayload{
		HandlingStrategy: handlingStrategy(basket),
		BasketVersion:    get(basket, "basketVersion"),
		Products:         []QuantityLine{{BasketProductID: id, Quantity: quantity}},
	})
}

// SetQuantity is setQuantity (api.mjs:495). Quantity 0 (or below) removes
// the line through the same quantity PATCH.
func (c *Client) SetQuantity(ctx context.Context, ref BasketLineRef, quantity int) (Value, error) {
	return c.patchLineQuantity(ctx, ref, quantity)
}

// RemoveFromBasket is removeFromBasket (api.mjs:513): a quantity-zero PATCH.
func (c *Client) RemoveFromBasket(ctx context.Context, ref BasketLineRef) (Value, error) {
	return c.patchLineQuantity(ctx, ref, 0)
}
