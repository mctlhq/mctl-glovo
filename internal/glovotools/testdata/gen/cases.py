# Case generator for internal/glovotools: writes ../cases.json and the
# synthetic fixtures under ../fixtures. gen.mjs then replays each case
# against the upstream glovo-skill v0.2.1 server to record ../golden.
#
#   python3 internal/glovotools/testdata/gen/cases.py
#
# Every input is synthetic (fixtures from this repository's glovoapi and
# glovologic testdata); nothing comes from a real account.
import base64, json, os, sys
root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # internal/glovotools/testdata
os.makedirs(os.path.join(root, "fixtures"), exist_ok=True)
NOW = 1772366400  # 2026-03-01T12:00:00Z
def jwt(exp):
    e = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()
    return "Bearer " + e(b'{"alg":"none","typ":"JWT"}') + "." + e(json.dumps({"exp": exp}, separators=(",", ":")).encode()) + ".sig"
valid, expired, rotated = jwt(NOW + 3600), jwt(NOW - 60), jwt(NOW + 7200)
loc = {"cityCode": "TST", "latitude": "10.5", "longitude": "-3.25", "countryCode": "XX"}
sessions = {
    "default": {"accessToken": valid, "refreshToken": "refresh-test-old", "customerId": 4242, "keepMe": "unknown-field", "location": loc},
    "expired": {"accessToken": expired, "refreshToken": "refresh-test-old", "customerId": 4242, "tokenType": "Bearer", "keepMe": "unknown-field", "location": loc},
    "expired_no_refresh": {"accessToken": expired, "customerId": 4242, "location": loc},
    "location_only": {"location": {"cityCode": "TST", "countryCode": "XX"}},
    "no_location": {"accessToken": valid, "refreshToken": "refresh-test-old", "customerId": 4242},
    "no_customer": {"accessToken": valid, "refreshToken": "refresh-test-old", "location": loc},
}
G = "../../glovoapi/testdata/"
def r(method, path, *replies):
    return {"method": method, "path": path, "replies": list(replies)}
def f(name, status=200): return {"status": status, "file": G + name + ".json"}
def lf(name, status=200): return {"status": status, "file": "fixtures/" + name + ".json"}
def j(obj, status=200): return {"status": status, "json": obj}
def err(status, body=None): return {"status": status, "json": body if body is not None else {"error": {"exceptionName": "TEST_FAILURE"}}}

# Suggestion fixtures, split out of the glovologic synthetic fixture.
sug = json.load(open(os.path.join(root, "../../glovologic/testdata/suggestions.json")))
def dump(name, obj):
    with open(os.path.join(root, "fixtures", name + ".json"), "w") as fh:
        json.dump(obj, fh, indent=1, sort_keys=False)
        fh.write("\n")
dump("suggest_global_search", sug["global_search"])
for k, v in sug["store_content"].items(): dump("suggest_content_" + k, v)
for k, v in sug["product_views"].items(): dump("suggest_view_" + k, v)
for k, v in sug["store_search"].items(): dump("suggest_search_" + k, v)
# Raw order-history pages whose compact cards are the glovologic discovery.
cards = sug["discovery"]["orders"]
raw = [{"orderId": c["order_id"], "content": {"title": c["store"]}, "status": c["status"], "creationTime": 1767225600000 - i * 86400000} for i, c in enumerate(cards)]
dump("suggest_orders_page1", {"pagination": {"next": {"offset": "o3"}}, "orders": raw[:3]})
dump("suggest_orders_page2", {"pagination": {"next": None}, "orders": raw[3:]})
suggest_routes = [
    r("POST", "/v1/web/store_wall/search", lf("suggest_global_search")),
    r("GET", "/v3/customer/orders-list", lf("suggest_orders_page1"), lf("suggest_orders_page2")),
    r("GET", "/v4/stores/store-1/addresses/address-1/content/main", lf("suggest_content_store-1")),
    r("GET", "/v3/stores/store-6/addresses/address-6/search", lf("suggest_search_store-6")),
] + [r("POST", f"/v4/stores/store-{s}/addresses/address-{s}/products/{p}/view", lf("suggest_view_" + p))
     for s in range(1, 8) for p in sug["product_views"].keys()]

B = "/v1/authenticated/customers/4242/baskets"
SB = B + "/stores/1001"
orders_routes = [r("GET", "/v3/customer/orders-list", f("orders_page1"), f("orders_page2"))]
detail_9003 = {"id": 9003, "storeId": 1001, "storeAddressId": 2001, "storeName": "Example Market",
               "currentStatus": {"type": "DELIVERED", "creationTime": 1767312000000},
               "boughtProducts": [{"name": "Whole Milk", "quantity": "3", "price": "4,50 EUR", "id": 1}],
               "total": {"formatted": "4,50 EUR"}}
dump("order_detail_9003", detail_9003)
refresh_ok = j({"access": {"accessToken": rotated, "refreshToken": "refresh-test-new", "expiresIn": 7200, "tokenType": "Bearer"}})

cases = []
def case(name, tool, args=None, session="default", routes=None, **kw):
    c = {"name": name, "tool": tool, "args": args or {}, "session": session, "routes": routes or []}
    c.update(kw)
    cases.append(c)

case("shopping_guide", "glovo_get_shopping_guide")
case("auth_status", "glovo_auth_status")
case("auth_status_expired", "glovo_auth_status", session="expired")
case("auth_status_signed_out", "glovo_auth_status", session=None)
case("get_location", "glovo_get_location")
case("get_location_signed_out", "glovo_get_location", session=None)
case("set_location", "glovo_set_location", {"country_code": "PT", "city_code": "LIS", "latitude": 38.7, "longitude": "-9.1", "language_code": "pt"})
case("set_location_partial", "glovo_set_location", {"city_code": "MAD"})
case("set_location_signed_out", "glovo_set_location", {"country_code": "ES", "city_code": "BCN"}, session=None)
case("set_location_invalid", "glovo_set_location", {"country_code": "ESP"})
case("saved_locations", "glovo_get_saved_locations", routes=[r("GET", "/customer_profile/api/v1/address_book/me/addresses", f("saved_addresses"))])
case("saved_locations_match", "glovo_get_saved_locations", {"match_text": "Example"}, routes=[r("GET", "/customer_profile/api/v1/address_book/me/addresses", f("saved_addresses"))])
case("saved_locations_fail", "glovo_get_saved_locations", routes=[r("GET", "/customer_profile/api/v1/address_book/me/addresses", err(500))], error_exact=True)
case("search_locations", "glovo_search_locations", {"query": "Example street"}, routes=[r("GET", "/v3/addresslookup/pub/address", f("address_search"))])
case("search_locations_limit", "glovo_search_locations", {"query": "Example street", "limit": 2}, routes=[r("GET", "/v3/addresslookup/pub/address", f("address_search"))])
case("search_locations_short", "glovo_search_locations", {"query": "ab"})
case("select_location", "glovo_select_location", {"place_id": "p1", "provider": "google"}, routes=[
    r("GET", "/v3/addresslookup/pub/p1", f("resolved_location")),
    r("GET", "/customer_profile/api/v1/guest/address_book/delivery_point_info", f("delivery_point"))])
case("select_location_invalid", "glovo_select_location", {"place_id": "p2"}, routes=[r("GET", "/v3/addresslookup/pub/p2", f("resolved_invalid"))])
case("select_location_undeliverable", "glovo_select_location", {"place_id": "p1"}, routes=[
    r("GET", "/v3/addresslookup/pub/p1", f("resolved_location")),
    r("GET", "/customer_profile/api/v1/guest/address_book/delivery_point_info", j({"data": {"isDeliverable": False}}))])
case("browse_stores", "glovo_browse_stores", routes=[r("POST", "/v1/web/store_wall/category/4", f("store_wall"))])
case("browse_stores_args", "glovo_browse_stores", {"category_id": 1, "offset": 24, "limit": 2, "previous_store_ids": [1001, "x-2"]}, routes=[r("POST", "/v1/web/store_wall/category/1", f("store_wall"))])
case("browse_stores_fail", "glovo_browse_stores", routes=[r("POST", "/v1/web/store_wall/category/4", err(503))], error_exact=True)
case("get_store", "glovo_get_store", {"store": 1001}, routes=[r("GET", "/v3/stores/1001", f("store"))])
case("get_store_slug", "glovo_get_store", {"store": "example-market"}, routes=[r("GET", "/v3/stores/example-market", f("store"))])
case("get_store_menu", "glovo_get_store_menu", {"store_id": 1001, "store_address_id": "2001"}, routes=[r("GET", "/v3/stores/1001/addresses/2001/node/store_menu", f("menu"))])
case("get_store_menu_limit", "glovo_get_store_menu", {"store_id": "1001", "store_address_id": 2001, "limit": 2}, routes=[r("GET", "/v3/stores/1001/addresses/2001/node/store_menu", f("menu"))])
case("browse_store_catalog", "glovo_browse_store_catalog", {"store_id": 1001, "store_address_id": 2001, "content_uri": "/v3/stores/1001/addresses/2001/content?nodeType=DEEP_LINK&link=dairy"},
     routes=[r("GET", "/v3/stores/1001/addresses/2001/content", f("store_content"))])
case("browse_store_catalog_foreign", "glovo_browse_store_catalog", {"store_id": 1001, "store_address_id": 2001, "content_uri": "/v3/stores/9999/addresses/2001/content"})
case("browse_store_catalog_empty", "glovo_browse_store_catalog", {"store_id": 1001, "store_address_id": 2001, "content_uri": "/v3/stores/1001/addresses/2001/content?link=x", "limit": 5},
     routes=[r("GET", "/v3/stores/1001/addresses/2001/content", j({"data": {"body": []}}))])
case("store_recommendations", "glovo_get_store_recommendations", {"store_id": 1001, "store_address_id": 2001}, routes=[r("GET", "/v4/stores/1001/addresses/2001/content/main", f("store_content"))])
case("store_recommendations_all", "glovo_get_store_recommendations", {"store_id": 1001, "store_address_id": 2001, "kind": "all", "limit": 2}, routes=[r("GET", "/v4/stores/1001/addresses/2001/content/main", f("store_content"))])
case("store_recommendations_top", "glovo_get_store_recommendations", {"store_id": 1001, "store_address_id": 2001, "kind": "top_sellers"}, routes=[r("GET", "/v4/stores/1001/addresses/2001/content/main", f("store_content"))])
oo = [r("GET", "/v1/stores/1001/addresses/2001/node/store_fees", f("store_fees")),
      r("GET", "/v4/stores/1001/addresses/2001/restrictions", f("store_restrictions")),
      r("GET", "/v3/stores/1001/addresses/2001/store_info_screen", f("store_info")),
      r("GET", "/v1/store-view/web/similar-stores", f("store_similar"))]
case("store_order_options", "glovo_get_store_order_options", {"store_id": 1001, "store_address_id": 2001}, routes=oo, unordered_requests=True)
case("store_order_options_args", "glovo_get_store_order_options", {"store_id": 1001, "store_address_id": 2001, "translation": "es", "similar_limit": 3}, routes=oo, unordered_requests=True)
case("store_order_options_fail", "glovo_get_store_order_options", {"store_id": 1001, "store_address_id": 2001},
     routes=[oo[0], r("GET", "/v4/stores/1001/addresses/2001/restrictions", err(500)), oo[2], oo[3]], unordered_requests=True, requests_subset=True)
case("search_store_items", "glovo_search_store_items", {"store_id": 1001, "store_address_id": 2001, "query": "milk"}, routes=[r("GET", "/v3/stores/1001/addresses/2001/search", f("search"))])
case("search_store_items_limit", "glovo_search_store_items", {"store_id": "1001", "store_address_id": "2001", "query": "milk", "limit": 1}, routes=[r("GET", "/v3/stores/1001/addresses/2001/search", f("search"))])
case("get_product", "glovo_get_product", {"store_id": 1001, "store_address_id": 2001, "product_id": 6001, "external_id": "ext-6001", "category_id": 9}, routes=[r("POST", "/v4/stores/1001/addresses/2001/products/6001/view", f("product_view"))])
case("get_product_minimal", "glovo_get_product", {"store_id": "1001", "store_address_id": "2001", "product_id": "6001", "quantity": 2}, routes=[r("POST", "/v4/stores/1001/addresses/2001/products/6001/view", f("product_view"))])
case("get_basket_all", "glovo_get_basket", routes=[r("GET", B, j([{"basketId": "basket-test-1", "storeId": 1001}]))])
case("get_basket_store", "glovo_get_basket", {"store_id": 1001}, routes=[r("GET", SB, f("basket"))])
case("get_basket_store_none", "glovo_get_basket", {"store_id": "1001"}, routes=[r("GET", SB, err(404))])
case("get_basket_refresh", "glovo_get_basket", {"store_id": 1001}, session="expired", routes=[r("POST", "/oauth/refresh", refresh_ok), r("GET", SB, f("basket"))])
case("get_basket_refresh_after_401", "glovo_get_basket", {"store_id": 1001}, routes=[r("GET", SB, err(401), f("basket")), r("POST", "/oauth/refresh", j({"accessToken": rotated, "expiresIn": 7200}))])
case("get_basket_refresh_rejected", "glovo_get_basket", {"store_id": 1001}, session="expired", routes=[r("POST", "/oauth/refresh", err(400, {"error": "invalid_grant"}))], error_exact=True)
case("get_basket_expired_no_refresh", "glovo_get_basket", {"store_id": 1001}, session="expired_no_refresh", routes=[r("GET", SB, err(401))], error_exact=True)
case("get_basket_signed_out", "glovo_get_basket", {"store_id": 1001}, session=None, error_exact=True)
case("get_basket_no_customer", "glovo_get_basket", {"store_id": 1001}, session="no_customer")
case("get_basket_malformed_session", "glovo_get_basket", {"store_id": 1001}, session=None, session_text="{not json", error_exact=True)
case("get_basket_fail", "glovo_get_basket", {"store_id": 1001}, routes=[r("GET", SB, err(500))], error_exact=True)
case("purchase_history", "glovo_get_purchase_history", routes=[r("GET", "/v3/customer/orders-list", f("orders_page1"))])
case("purchase_history_cursor", "glovo_get_purchase_history", {"offset": "9001", "limit": 5}, routes=[r("GET", "/v3/customer/orders-list", f("orders_page2"))])
case("purchase_history_fail", "glovo_get_purchase_history", routes=[r("GET", "/v3/customer/orders-list", err(500))], error_exact=True)
case("order_items", "glovo_get_order_items", {"order_id": 9001}, routes=[r("GET", "/v3/customer/orders/9001", f("order_detail"))])
case("preview_reorder", "glovo_preview_reorder", {"order_id": "9001"}, routes=[r("GET", "/v3/customer/orders/9001", f("order_detail"))])
case("plan_reorder", "glovo_plan_reorder", {"order_id": 9001}, routes=[
    r("GET", "/v3/customer/orders/9001", f("order_detail")),
    r("GET", "/v4/stores/1001/addresses/2001/content/main", f("store_content")),
    r("GET", "/v3/stores/1001/addresses/2001/search", f("search"))])
case("plan_reorder_args", "glovo_plan_reorder", {"order_id": 9001, "max_searches": 1, "candidates_per_line": 1}, routes=[
    r("GET", "/v3/customer/orders/9001", f("order_detail")),
    r("GET", "/v4/stores/1001/addresses/2001/content/main", f("store_content")),
    r("GET", "/v3/stores/1001/addresses/2001/search", f("search"))])
case("order_stats", "glovo_get_order_stats", {"page_delay_ms": 0}, routes=orders_routes)
case("order_stats_one_page", "glovo_get_order_stats", {"max_pages": 1, "page_delay_ms": 0}, routes=orders_routes)
case("analyze_history", "glovo_analyze_order_history", {"page_delay_ms": 0, "detail_delay_ms": 0}, routes=orders_routes + [
    r("GET", "/v3/customer/orders/9001", f("order_detail")),
    r("GET", "/v3/customer/orders/9002", err(500)),
    r("GET", "/v3/customer/orders/9003", lf("order_detail_9003"))])
case("analyze_history_rate_limited", "glovo_analyze_order_history", {"page_delay_ms": 0, "detail_delay_ms": 0, "detail_limit": 3}, routes=orders_routes + [
    r("GET", "/v3/customer/orders/9001", f("order_detail")),
    r("GET", "/v3/customer/orders/9002", err(429))])
case("suggestions_repeat", "glovo_get_suggestions", {"mode": "repeat", "query": "pizza", "item_mode": "repeat", "quality_preference": "personal", "max_choices": 3}, routes=suggest_routes, strip_go_coverage=True, unordered_requests=True)
case("suggestions_different", "glovo_get_suggestions", {"mode": "repeat", "query": "pizza", "venue_query": "Favorite Pizza", "item_mode": "different", "known_liked_only": True, "max_choices": 3}, routes=suggest_routes, strip_go_coverage=True, unordered_requests=True)
case("suggestions_explore_google_unconfigured", "glovo_get_suggestions", {"mode": "explore", "query": "pizza", "include_google_quality": True, "max_choices": 3}, routes=suggest_routes, strip_go_coverage=True, unordered_requests=True)
case("suggestions_fallback", "glovo_get_suggestions", {"mode": "explore", "query": "funghi", "max_choices": 3, "history_refresh": "full"}, routes=suggest_routes, strip_go_coverage=True, unordered_requests=True)
case("suggestions_no_location", "glovo_get_suggestions", {"query": "pizza"}, session="no_location", routes=suggest_routes, error_exact=True)
case("suggestions_signed_out", "glovo_get_suggestions", {"query": "pizza"}, session="location_only", error_exact=True)
# Basket writes.
case("add_to_basket_create", "glovo_add_to_basket", {"store_id": 1001, "store_address_id": 2001, "product_id": 6001, "quantity": 2,
     "selected_options": [{"group_id": 71, "option_id": 702}, {"group_id": "73", "option_id": "721"}]}, routes=[
    r("POST", "/v4/stores/1001/addresses/2001/products/6001/view", f("product_view")),
    r("GET", "/v3/stores/1001", j({"id": 1001, "categoryId": 4, "storeAddressId": 2001})),
    r("GET", SB, err(404)),
    r("POST", B, f("basket"))])
case("add_to_basket_no_category", "glovo_add_to_basket", {"store_id": 1001, "store_address_id": 2001, "product_id": 6001,
     "selected_options": [{"group_id": 71, "option_id": 701}, {"group_id": 73, "option_id": 721}]}, routes=[
    r("POST", "/v4/stores/1001/addresses/2001/products/6001/view", f("product_view")),
    r("GET", "/v3/stores/1001", j({"id": 1001, "storeCategoryId": 4})),
    r("GET", SB, err(404))])
case("add_to_basket_bump", "glovo_add_to_basket", {"store_id": 1001, "store_address_id": 2001, "store_category_id": 4, "product_id": "5001", "external_id": "ext-5001", "store_product_id": "sp-5001", "quantity": 3, "validate_options": False}, routes=[
    r("GET", SB, f("basket")),
    r("PATCH", B + "/basket-test-1/products/quantity", f("basket"))])
case("add_to_basket_append", "glovo_add_to_basket", {"store_id": 1001, "store_address_id": 2001, "store_category_id": "4", "product_id": 6001, "selected_options": [{"group_id": "71", "option_id": "701", "quantity": 1}, {"group_id": 73, "option_id": 721}]}, routes=[
    r("POST", "/v4/stores/1001/addresses/2001/products/6001/view", f("product_view")),
    r("GET", SB, f("basket")),
    r("PUT", B + "/basket-test-1/products", f("basket"))])
case("add_to_basket_missing_option", "glovo_add_to_basket", {"store_id": 1001, "store_address_id": 2001, "store_category_id": 4, "product_id": 6001}, routes=[
    r("POST", "/v4/stores/1001/addresses/2001/products/6001/view", f("product_view"))])
case("add_to_basket_unknown_option", "glovo_add_to_basket", {"store_id": 1001, "store_address_id": 2001, "store_category_id": 4, "product_id": 6001, "selected_options": [{"group_id": "nope", "option_id": "x"}]}, routes=[
    r("POST", "/v4/stores/1001/addresses/2001/products/6001/view", f("product_view"))])
case("add_to_basket_read_fail", "glovo_add_to_basket", {"store_id": 1001, "store_address_id": 2001, "store_category_id": 4, "product_id": "5001", "external_id": "e", "store_product_id": "s", "validate_options": False}, routes=[
    r("GET", SB, err(500))], error_exact=True)
case("set_quantity", "glovo_set_quantity", {"store_id": 1001, "product_id": "5001", "quantity": 4}, routes=[
    r("GET", SB, f("basket")), r("PATCH", B + "/basket-test-1/products/quantity", f("basket"))])
case("set_quantity_zero", "glovo_set_quantity", {"store_id": 1001, "basket_product_id": "bp-test-2", "quantity": 0}, routes=[
    r("GET", SB, f("basket")), r("PATCH", B + "/basket-test-1/products/quantity", f("basket"))])
case("set_quantity_missing", "glovo_set_quantity", {"store_id": 1001, "product_id": "9999", "quantity": 1}, routes=[r("GET", SB, f("basket"))])
case("set_quantity_no_basket", "glovo_set_quantity", {"store_id": 1001, "product_id": "5001", "quantity": 1}, routes=[r("GET", SB, err(404))])
case("remove_from_basket", "glovo_remove_from_basket", {"store_id": 1001, "store_product_id": "sp-5004"}, routes=[
    r("GET", SB, f("basket")), r("PATCH", B + "/basket-test-1/products/quantity", j({"basketId": "basket-test-1", "products": []}))])
case("remove_from_basket_fail", "glovo_remove_from_basket", {"store_id": 1001, "basket_product_id": "bp-test-1"}, routes=[
    r("GET", SB, f("basket")), r("PATCH", B + "/basket-test-1/products/quantity", err(409))], error_exact=True)

spec = {"now_ms": NOW * 1000, "sessions": sessions, "cases": cases}
with open(os.path.join(root, "cases.json"), "w") as fh:
    json.dump(spec, fh, indent=1)
    fh.write("\n")
print(len(cases), "cases")
