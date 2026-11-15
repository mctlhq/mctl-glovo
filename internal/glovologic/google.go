package glovologic

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Google Places field masks (suggestions.mjs).
const (
	GooglePlacesFieldMask      = "places.id,places.displayName,places.location,places.rating,places.userRatingCount,places.googleMapsUri,places.businessStatus,places.attributions"
	GooglePlaceReviewFieldMask = "id,reviews,attributions"
	googlePlacesBaseURL        = "https://places.googleapis.com"
	googleTimeout              = 7500 * time.Millisecond
)

// Location is the selected Glovo delivery location. Latitude and Longitude
// are strings or numbers, as glovoapi.Location holds them.
type Location struct {
	Latitude  any
	Longitude any
	CityCode  string
}

// GoogleRequest is one Google Places lookup for a shortlisted venue.
type GoogleRequest struct {
	VenueName      any
	Location       Location
	CityCode       string
	IncludeReviews bool
	ReviewLimit    int
}

// GoogleProvider looks up Google Maps quality evidence for one venue. The
// result is attached verbatim as google_maps; its match_status must be a
// string ("matched", "no_match" or "ambiguous").
type GoogleProvider func(ctx context.Context, r GoogleRequest) (Object, error)

// numberOrNil is suggestions.mjs number(): Number(v) when finite, else null.
func numberOrNil(v any) *float64 { return finiteOrNil(toNumber(v)) }

func normalizedRating(v any) *float64 {
	p := numberOrNil(v)
	if p == nil {
		return nil
	}
	r := math.Max(0, *p)
	if *p > 5 {
		r = math.Min(5, *p/20)
	}
	return &r
}

// shrunkQuality is the Bayesian count-aware rating with prior 4.2 x 100.
func shrunkQuality(rating, count any) *float64 {
	n := normalizedRating(rating)
	if n == nil {
		return nil
	}
	votes := 0.0
	if c := numberOrNil(count); c != nil && *c != 0 {
		votes = math.Max(0, *c)
	}
	q := (votes**n + 100*4.2) / (votes + 100)
	return &q
}

func roundedOrNil(p *float64, digits int) *float64 {
	if p == nil {
		return nil
	}
	return finiteOrNil(toFixed(*p, digits))
}

func distanceMeters(left Location, right any) *float64 {
	lat1, lon1 := numberOrNil(left.Latitude), numberOrNil(left.Longitude)
	lat2, lon2 := numberOrNil(get(right, "latitude")), numberOrNil(get(right, "longitude"))
	if lat1 == nil || lon1 == nil || lat2 == nil || lon2 == nil {
		return nil
	}
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := rad(*lat2-*lat1), rad(*lon2-*lon1)
	a := math.Pow(math.Sin(dLat/2), 2) + math.Cos(rad(*lat1))*math.Cos(rad(*lat2))*math.Pow(math.Sin(dLon/2), 2)
	d := 6_371_000 * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return &d
}

func nameAgreement(left, right any) float64 {
	a, b := NormalizeVenue(left), NormalizeVenue(right)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return 0.9
	}
	return jaccard(strings.Split(a, " "), strings.Split(b, " "))
}

// PlaceMatch is the matchGooglePlace result.
type PlaceMatch struct {
	Status         string   `json:"status"`
	Place          Object   `json:"place,omitempty"`
	NameScore      *float64 `json:"name_score,omitempty"`
	DistanceMeters *float64 `json:"distance_meters,omitempty"`
}

var placeFields = map[string]kind{
	"id": kString, "displayName": kObject, "location": kObject, "businessStatus": kString,
	"rating": kString | kNumber, "userRatingCount": kString | kNumber, "googleMapsUri": kString, "attributions": kArray,
}

func checkPlaces(places []any, path string) error {
	for i, p := range places {
		pp := idx(path, i)
		if nullish(p) {
			continue
		}
		if _, ok := p.(map[string]any); !ok {
			return &InputError{Path: pp, Msg: "want object, got " + typeName(p)}
		}
		if err := expectFields(p, pp, placeFields); err != nil {
			return err
		}
		if err := expect(get(p, "displayName", "text"), pp+".displayName.text", kString); err != nil {
			return err
		}
		if err := expectFields(get(p, "location"), pp+".location", map[string]kind{"latitude": kString | kNumber, "longitude": kString | kNumber}); err != nil {
			return err
		}
	}
	return nil
}

// MatchGooglePlace is matchGooglePlace: a name score of at least 0.85 within
// 5 km, refusing near ties as ambiguous.
func MatchGooglePlace(venueName any, loc Location, places []any) (PlaceMatch, error) {
	if err := checkPlaces(places, "places"); err != nil {
		return PlaceMatch{}, err
	}
	type entry struct {
		place    any
		name     float64
		distance float64
	}
	var ranked []entry
	for _, p := range places {
		if get(p, "businessStatus") == "CLOSED_PERMANENTLY" {
			continue
		}
		name := nameAgreement(venueName, get(p, "displayName", "text"))
		d := distanceMeters(loc, get(p, "location"))
		if name >= 0.85 && d != nil && *d <= 5000 {
			ranked = append(ranked, entry{place: p, name: name, distance: *d})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].name != ranked[j].name {
			return ranked[i].name > ranked[j].name
		}
		return ranked[i].distance < ranked[j].distance
	})
	if len(ranked) == 0 {
		return PlaceMatch{Status: "no_match"}, nil
	}
	if len(ranked) > 1 && ranked[0].name == ranked[1].name && ranked[1].distance-ranked[0].distance < 1000 {
		return PlaceMatch{Status: "ambiguous"}, nil
	}
	best := ranked[0]
	place, _ := best.place.(map[string]any)
	return PlaceMatch{Status: "matched", Place: place, NameScore: &best.name, DistanceMeters: &best.distance}, nil
}

// NewGooglePlacesProvider is createGooglePlacesProvider. It returns nil when
// apiKey is empty: Google evidence is then reported as not configured. A nil
// client means http.DefaultClient; an empty baseURL means the Places API.
// The key travels only in X-Goog-Api-Key.
func NewGooglePlacesProvider(apiKey string, client *http.Client, baseURL string) GoogleProvider {
	if apiKey == "" {
		return nil
	}
	if client == nil {
		client = http.DefaultClient
	}
	if baseURL == "" {
		baseURL = googlePlacesBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return func(ctx context.Context, r GoogleRequest) (Object, error) {
		return googleLookup(ctx, client, baseURL, apiKey, r)
	}
}

type googleCenter struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

type googleSearchBody struct {
	TextQuery    string `json:"textQuery"`
	PageSize     int    `json:"pageSize"`
	LocationBias *struct {
		Circle struct {
			Center googleCenter `json:"center"`
			Radius int          `json:"radius"`
		} `json:"circle"`
	} `json:"locationBias,omitempty"`
}

func googleCall(ctx context.Context, client *http.Client, method, url, apiKey, mask string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, googleTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Goog-Api-Key", apiKey)
	req.Header.Set("X-Goog-FieldMask", mask)
	resp, err := client.Do(req) // #nosec G107 G704 -- fixed Places API base URL
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, err
}

func googleLookup(ctx context.Context, client *http.Client, baseURL, apiKey string, r GoogleRequest) (Object, error) {
	var parts []string
	for _, p := range []any{r.VenueName, r.CityCode} {
		if truthy(p) {
			parts = append(parts, jsString(p))
		}
	}
	body := googleSearchBody{TextQuery: strings.Join(parts, " "), PageSize: 3}
	lat, lon := numberOrNil(r.Location.Latitude), numberOrNil(r.Location.Longitude)
	if lat != nil && lon != nil {
		body.LocationBias = &struct {
			Circle struct {
				Center googleCenter `json:"center"`
				Radius int          `json:"radius"`
			} `json:"circle"`
		}{}
		body.LocationBias.Circle.Center = googleCenter{Latitude: *lat, Longitude: *lon}
		body.LocationBias.Circle.Radius = 3000
	}
	payload, err := marshalJSON(body)
	if err != nil {
		return nil, err
	}
	status, data, err := googleCall(ctx, client, http.MethodPost, baseURL+"/v1/places:searchText", apiKey, GooglePlacesFieldMask, payload)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("google places search failed (%d)", status)
	}
	parsed, err := decodeAt(data, "google.searchText")
	if err != nil {
		return nil, err
	}
	if err := expect(get(parsed, "places"), "google.searchText.places", kArray); err != nil {
		return nil, err
	}
	places, _ := get(parsed, "places").([]any)
	match, err := MatchGooglePlace(r.VenueName, r.Location, places)
	if err != nil {
		return nil, err
	}
	if match.Status != "matched" {
		return Object{"match_status": match.Status}, nil
	}
	place := match.Place
	reviews := Null
	reviewAttributions := []any{}
	reviewStatus := "not_requested"
	if r.IncludeReviews {
		reviewStatus = "unavailable"
		if rv, attrs, ok := googleReviews(ctx, client, baseURL, apiKey, jsString(place["id"]), r.ReviewLimit); ok {
			reviews, reviewAttributions, reviewStatus = rv, attrs, "returned"
		}
	}
	attributions := []any{}
	if a, ok := place["attributions"].([]any); ok {
		attributions = append(attributions, a...)
	}
	attributions = append(attributions, reviewAttributions...)
	out := Object{
		"match_status":        "matched",
		"source":              "Google Maps",
		"place_id":            nullable(place["id"]),
		"rating":              nullableFloat(numberOrNil(place["rating"])),
		"user_rating_count":   nullableFloat(numberOrNil(place["userRatingCount"])),
		"count_aware_quality": nullableFloat(roundedOrNil(shrunkQuality(place["rating"], place["userRatingCount"]), 3)),
		"quality_prior":       Object{"rating": 4.2, "weight": 100.0},
		"attributions":        attributions,
		"reviews":             reviews,
		"reviews_status":      reviewStatus,
		"review_notice":       Null,
		"review_policy":       Null,
		"match_basis":         "normalized venue name plus proximity to the selected Glovo location",
		"ranking_effect":      "display_only_not_backtested",
	}
	if _, ok := place["id"]; !ok {
		delete(out, "place_id")
	}
	setOrDelete(out, "display_name", get(place, "displayName", "text"))
	setOrDelete(out, "business_status", place["businessStatus"])
	setOrDelete(out, "google_maps_uri", place["googleMapsUri"])
	if r.IncludeReviews {
		out["review_notice"] = "Google selects and orders returned reviews; they may not be representative. Preserve author attribution and Google Maps links when displaying them."
		out["review_policy"] = "https://developers.google.com/maps/documentation/places/web-service/policies"
	}
	return out, nil
}

func nullableFloat(p *float64) any {
	if p == nil {
		return Null
	}
	return *p
}

var reviewFields = map[string]kind{
	"text": kObject, "publishTime": kString, "relativePublishTimeDescription": kString,
	"rating": kString | kNumber, "googleMapsUri": kString, "authorAttribution": kObject,
}

// googleReviews fetches review text; ok is false where upstream reports
// reviews_status "unavailable" (any failure, including a malformed body).
func googleReviews(ctx context.Context, client *http.Client, baseURL, apiKey, placeID string, limit int) (any, []any, bool) {
	status, data, err := googleCall(ctx, client, http.MethodGet, baseURL+"/v1/places/"+encodeURIComponent(placeID), apiKey, GooglePlaceReviewFieldMask, nil)
	if err != nil || status < 200 || status > 299 {
		return nil, nil, false
	}
	details, err := decodeAt(data, "google.place")
	if err != nil {
		return nil, nil, false
	}
	if expect(get(details, "reviews"), "reviews", kArray) != nil || expect(get(details, "attributions"), "attributions", kArray) != nil {
		return nil, nil, false
	}
	raw, _ := get(details, "reviews").([]any)
	n := max(1, min(3, limit))
	if len(raw) > n {
		raw = raw[:n]
	}
	reviews := make([]any, 0, len(raw))
	for i, rv := range raw {
		if !nullish(rv) {
			if _, ok := rv.(map[string]any); !ok {
				return nil, nil, false
			}
		}
		if expectFields(rv, idx("reviews", i), reviewFields) != nil {
			return nil, nil, false
		}
		o := Object{"rating": nullableFloat(numberOrNil(get(rv, "rating"))), "author_attribution": Null}
		setOrDelete(o, "text", get(rv, "text", "text"))
		setOrDelete(o, "language_code", get(rv, "text", "languageCode"))
		setOrDelete(o, "publish_time", get(rv, "publishTime"))
		setOrDelete(o, "relative_publish_time", get(rv, "relativePublishTimeDescription"))
		setOrDelete(o, "google_maps_uri", get(rv, "googleMapsUri"))
		if a := get(rv, "authorAttribution"); truthy(a) {
			aa := Object{}
			setOrDelete(aa, "display_name", get(a, "displayName"))
			setOrDelete(aa, "uri", get(a, "uri"))
			setOrDelete(aa, "photo_uri", get(a, "photoUri"))
			o["author_attribution"] = aa
		}
		reviews = append(reviews, o)
	}
	attrs, _ := get(details, "attributions").([]any)
	if attrs == nil {
		attrs = []any{}
	}
	return reviews, attrs, true
}
