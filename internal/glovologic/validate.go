package glovologic

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// InputError reports malformed input: undecodable JSON, or a field present
// with a JSON type the logic cannot use. It is never turned into an empty
// result.
type InputError struct {
	Path string
	Msg  string
}

func (e *InputError) Error() string {
	return fmt.Sprintf("glovologic: malformed input at %s: %s", e.Path, e.Msg)
}

type kind uint8

const (
	kString kind = 1 << iota
	kNumber
	kBool
	kObject
	kArray
)

func (k kind) String() string {
	var parts []string
	for _, p := range []struct {
		k kind
		n string
	}{{kString, "string"}, {kNumber, "number"}, {kBool, "boolean"}, {kObject, "object"}, {kArray, "array"}} {
		if k&p.k != 0 {
			parts = append(parts, p.n)
		}
	}
	return strings.Join(parts, "|")
}

// kindOf is the JSON type of v; 0 for absent or null.
func kindOf(v any) kind {
	switch v.(type) {
	case string:
		return kString
	case json.Number, float64, int:
		return kNumber
	case bool:
		return kBool
	case map[string]any:
		return kObject
	case []any:
		return kArray
	default:
		return 0
	}
}

func typeName(v any) string {
	if k := kindOf(v); k != 0 {
		return k.String()
	}
	return "null"
}

// expect accepts an absent or null v, or one of the allowed kinds.
func expect(v any, path string, allowed kind) error {
	if nullish(v) {
		return nil
	}
	if kindOf(v)&allowed == 0 {
		return &InputError{Path: path, Msg: "want " + allowed.String() + ", got " + typeName(v)}
	}
	return nil
}

// expectFields checks each listed field of obj.
func expectFields(obj any, path string, fields map[string]kind) error {
	for _, k := range sortedKeys(fields) {
		if err := expect(get(obj, k), path+"."+k, fields[k]); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string]kind) []string { return slices.Sorted(maps.Keys(m)) }

func idx(path string, i int) string { return path + "[" + strconv.Itoa(i) + "]" }

// decodeAt decodes raw JSON and wraps a syntax error as an InputError.
func decodeAt(raw []byte, path string) (any, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, &InputError{Path: path, Msg: "invalid JSON: " + err.Error()}
	}
	return v, nil
}

// decodeObjects decodes a JSON array of objects and validates each one.
func decodeObjects(raw []byte, path string, check func(any, string) error) ([]Object, error) {
	v, err := decodeAt(raw, path)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, &InputError{Path: path, Msg: "want array, got " + typeName(v)}
	}
	out := make([]Object, len(arr))
	for i, e := range arr {
		p := idx(path, i)
		o, ok := e.(map[string]any)
		if !ok {
			return nil, &InputError{Path: p, Msg: "want object, got " + typeName(e)}
		}
		if err := check(o, p); err != nil {
			return nil, err
		}
		out[i] = o
	}
	return out, nil
}

var idKinds = kString | kNumber

// orderCardFields are the compactOrder (api.mjs:1076) fields this package
// reads, plus detail_source and history_index which upstream also honours.
var orderCardFields = map[string]kind{
	"order_id":      idKinds,
	"store":         kString,
	"store_name":    kString,
	"status":        kString,
	"total":         kString | kNumber,
	"date":          kString | kNumber,
	"detail_source": kString,
	"history_index": kString | kNumber,
	"items":         kArray,
	"url":           kString,
}

func checkOrderCard(v any, path string) error {
	if _, ok := v.(map[string]any); !ok {
		return &InputError{Path: path, Msg: "want object, got " + typeName(v)}
	}
	return expectFields(v, path, orderCardFields)
}

// productFields are the compactItem (api.mjs:913) fields this package reads.
var productFields = map[string]kind{
	"name":                kString,
	"product_id":          idKinds,
	"external_id":         idKinds,
	"store_product_id":    idKinds,
	"store_id":            idKinds,
	"store_address_id":    idKinds,
	"available":           kBool,
	"category_id":         idKinds,
	"collection_id":       idKinds,
	"collection_group_id": idKinds,
	"search_id":           idKinds,
	"source":              kString,
	"match_score":         kNumber,
}

func checkProduct(v any, path string) error { return expectFields(v, path, productFields) }

// productViewFields are the compactProductView (api.mjs:1026) fields read.
var productViewFields = map[string]kind{
	"name":          kString,
	"price":         kString | kNumber,
	"add_enabled":   kBool,
	"option_groups": kArray,
}

func checkProductView(v any, path string) error {
	if err := expectFields(v, path, productViewFields); err != nil {
		return err
	}
	groups, _ := get(v, "option_groups").([]any)
	for i, g := range groups {
		p := idx(path+".option_groups", i)
		if _, ok := g.(map[string]any); !ok {
			return &InputError{Path: p, Msg: "want object, got " + typeName(g)}
		}
		if err := expect(get(g, "required"), p+".required", kBool); err != nil {
			return err
		}
	}
	return nil
}

var orderDetailFields = map[string]kind{
	"id": idKinds, "orderId": idKinds, "orderCode": idKinds, "code": idKinds,
	"storeId": idKinds, "storeAddressId": idKinds,
	"storeName": kString, "providerName": kString,
	"store": kObject, "reorderData": kObject, "currentStatus": kObject, "order": kObject,
	"isRemake":     kBool,
	"creationTime": kString | kNumber, "createdAt": kString | kNumber,
	"orderTime": kString | kNumber, "deliveredAt": kString | kNumber,
	"boughtProducts": kArray, "products": kArray, "orderProducts": kArray, "items": kArray,
}

var orderLineFields = map[string]kind{
	"name": kString, "productName": kString, "title": kString,
	"quantity": kNumber | kString | kObject, "amount": kNumber | kString,
	"price":                     kString | kNumber | kObject,
	"customizationsDescription": kString, "description": kString,
}

// checkOrderDetail validates the raw order detail (GET /v3/customer/orders/{id})
// fields read by pastOrderLines, orderDate and compactReorderPreview. A null
// detail is allowed: upstream handles it as an empty order.
func checkOrderDetail(o any, path string) error {
	if nullish(o) {
		return nil
	}
	if _, ok := o.(map[string]any); !ok {
		return &InputError{Path: path, Msg: "want object, got " + typeName(o)}
	}
	if err := expectFields(o, path, orderDetailFields); err != nil {
		return err
	}
	if err := expectFields(get(o, "store"), path+".store", map[string]kind{
		"id": idKinds, "storeId": idKinds, "addressId": idKinds, "storeAddressId": idKinds, "name": kString,
	}); err != nil {
		return err
	}
	if err := expectFields(get(o, "reorderData"), path+".reorderData", map[string]kind{"allowed": kBool, "isAllowed": kBool}); err != nil {
		return err
	}
	if err := expectFields(get(o, "currentStatus"), path+".currentStatus", map[string]kind{"creationTime": kString | kNumber}); err != nil {
		return err
	}
	if err := expect(get(o, "order", "products"), path+".order.products", kArray); err != nil {
		return err
	}
	lines, key := orderLineContainer(o)
	for i, line := range lines {
		p := idx(path+"."+key, i)
		if nullish(line) {
			continue
		}
		if _, ok := line.(map[string]any); !ok {
			return &InputError{Path: p, Msg: "want object, got " + typeName(line)}
		}
		if err := expectFields(line, p, orderLineFields); err != nil {
			return err
		}
		if q, ok := get(line, "quantity").(map[string]any); ok {
			if err := expect(q["increments"], p+".quantity.increments", kNumber|kString); err != nil {
				return err
			}
		}
		if pr, ok := get(line, "price").(map[string]any); ok {
			if err := expect(pr["formatted"], p+".price.formatted", kNumber|kString); err != nil {
				return err
			}
		}
	}
	return nil
}

// orderLineContainer is the `||` chain of pastOrderLines (api.mjs:1124).
func orderLineContainer(o any) ([]any, string) {
	for _, path := range [][]string{{"boughtProducts"}, {"products"}, {"orderProducts"}, {"items"}, {"order", "products"}} {
		if v := get(o, path...); truthy(v) {
			arr, _ := v.([]any)
			return arr, strings.Join(path, ".")
		}
	}
	return nil, ""
}
