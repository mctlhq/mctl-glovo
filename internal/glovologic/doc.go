// Package glovologic holds the pure tool logic of the Glovo MCP tools that
// work on data the HTTP client has already fetched: order statistics, order
// history analysis, reorder planning, personalized suggestions, the venue
// model and the order-history cache.
//
// It is derived from denya/glovo-skill (MIT, Copyright (c) 2026 Denya): the
// glovo_get_order_stats, glovo_analyze_order_history, glovo_plan_reorder and
// glovo_get_suggestions handlers in src/server.mjs, orderStatsFromCards,
// orderAnalysisFromDetails, buildReorderPlan, productMatchScore and
// GlovoClient.planReorder / analyzeOrderHistory in src/glovo/api.mjs, and
// src/glovo/suggestions.mjs, src/glovo/venue-model.mjs and
// src/glovo/history-cache.mjs. See the NOTICE file in this directory for the
// upstream license.
//
// The package never mutates anything remote. It has no checkout, payment,
// order-placement or basket-write code; every plan and suggestion it returns
// carries mutates_basket: false. Remote data arrives through the small source
// interfaces in sources.go, as JSON, so this package does not depend on the
// internals of the HTTP client.
//
// Fail-closed rules: malformed JSON, or a field present with the wrong JSON
// type, is an *InputError, never an empty result. A field that is absent or
// null stays empty, as upstream. JSON objects are decoded into Object, where
// an absent key means JavaScript undefined (omitted from output) and Null
// means a present JSON null.
package glovologic
