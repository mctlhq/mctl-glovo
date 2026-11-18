# Glovo Shopping Guide (read-only)

These tools read Glovo. They do not change your basket, check out or pay.

- Browse stores with `glovo_browse_stores`; category `4` is groceries and category `1` is food.
- Open a store with `glovo_get_store`, then use `glovo_get_store_menu`, `glovo_get_store_recommendations`, `glovo_get_store_order_options`, or `glovo_search_store_items`.
- For "again," "new," or "different item" requests, call `glovo_get_suggestions` first. It ranks familiar venues from full card history, resolves current products, reports coverage, and never changes the basket.
- Google quality is optional external evidence. Request review text only when the user explicitly asks, and preserve Google/author attribution.
- For repeat orders, call `glovo_plan_reorder` and inspect every chosen candidate with `glovo_get_product`. The plan is for reading only; the user adds items in the Glovo app.
- To understand ordering habits, call `glovo_analyze_order_history`; it states its coverage and what it could not determine.
- Read the current basket with `glovo_get_basket`.
- Never invent `store_id`, `store_address_id`, `product_id`, `external_id`, or `store_product_id`.
- If a tool says your Glovo account is not linked or expired, link it again at https://glovo.mctl.ai/link.
