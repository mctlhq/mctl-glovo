// Package glovoapi is a Go client for the Glovo customer HTTP API.
//
// It is derived from denya/glovo-skill (MIT, Copyright (c) 2026 Denya): the
// GlovoClient class and the compact* helpers in src/glovo/api.mjs, and the
// session store in src/auth/store.mjs. See the NOTICE file in this directory
// for the upstream license.
//
// The client reads public catalog data, the signed-in customer's address book,
// order history and baskets, and writes baskets. Basket writes are its only
// mutations: it has no checkout, payment or order-placement method, and the
// request layer refuses any non-GET request outside a fixed allowlist of
// basket and read-only POST endpoints.
//
// Raw responses are returned as decoded JSON trees (see Value). The Compact*
// functions map them to typed results whose JSON encoding has the same field
// names, and the same present/absent/null behaviour, as the upstream compact*
// functions, so tool output shapes stay identical.
//
// Fail-closed rules: a non-2xx response, a malformed body, a failed refresh or
// a failed page in a paginated read is an error, never an empty result. A 401
// or 403 on an authenticated call triggers exactly one refresh and retry.
//
// Egress: every request, token refresh included, is built from the client's
// one base URL and sent through its one http.Client. Redirects are followed
// only on the base URL origin and only to requests the mutation allowlist
// accepts. WithProxy routes all of it through an explicit HTTP forward proxy
// (CONNECT, TLS end to end) and never consults HTTPS_PROXY or NO_PROXY;
// WithRequireProxy makes a missing proxy a constructor error. An unreachable
// proxy is an error from the call, never a direct retry.
package glovoapi
