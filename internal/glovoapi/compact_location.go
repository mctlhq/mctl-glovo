package glovoapi

import (
	"math"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// asArray mirrors asArray (api.mjs:684).
func asArray(v Value) []any {
	for _, cand := range []Value{
		v, get(v, "data"), get(v, "results"), get(v, "predictions"), get(v, "addresses"), get(v, "items"),
		get(v, "data", "results"), get(v, "data", "predictions"), get(v, "data", "addresses"),
	} {
		if l, ok := cand.([]any); ok {
			return l
		}
	}
	return []any{}
}

// LocationSuggestion is one compactLocationSearch result.
type LocationSuggestion struct {
	PlaceID  Value `json:"place_id,omitempty"`
	Provider Value `json:"provider,omitempty"`
	Title    Value `json:"title,omitempty"`
	Subtitle Value `json:"subtitle,omitempty"`
}

// CompactLocationSearchResult is compactLocationSearch (api.mjs:700).
type CompactLocationSearchResult struct {
	Count   int                  `json:"count"`
	Results []LocationSuggestion `json:"results"`
}

// CompactLocationSearch is compactLocationSearch (api.mjs:700). The limit is
// clamped to 1..5; zero means 5.
func CompactLocationSearch(data Value, limit int) *CompactLocationSearchResult {
	if limit == 0 {
		limit = 5
	}
	limit = int(math.Min(math.Max(float64(limit), 1), 5))
	entries := asArray(data)
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := &CompactLocationSearchResult{Results: []LocationSuggestion{}}
	for _, e := range entries {
		s := LocationSuggestion{
			PlaceID:  firstValue(get(e, "placeId"), get(e, "place_id"), get(e, "id")),
			Provider: firstValue(get(e, "provider"), get(e, "source")),
			Title:    firstValue(get(e, "title"), get(e, "mainText"), get(e, "primaryText"), get(e, "name"), get(e, "description")),
			Subtitle: firstValue(get(e, "subtitle"), get(e, "secondaryText")),
		}
		if truthy(s.PlaceID) && truthy(s.Title) {
			out.Results = append(out.Results, s)
		}
	}
	out.Count = len(out.Results)
	return out
}

func coordinatePair(data Value) (float64, float64) {
	src := coalesce(get(data, "data"), data)
	loc := coalesce(get(src, "geometry", "location"), get(src, "location"), get(src, "coordinates"), get(src, "coordinate"))
	if nullish(loc) {
		loc = map[string]any{}
	}
	lat := jsNumber(firstValue(get(src, "latitude"), get(src, "lat"), get(loc, "latitude"), get(loc, "lat")))
	lon := jsNumber(firstValue(get(src, "longitude"), get(src, "lon"), get(src, "lng"), get(loc, "longitude"), get(loc, "lon"), get(loc, "lng")))
	return lat, lon
}

// locationCode mirrors locationCode (api.mjs:722).
func locationCode(data Value, keys ...string) *string {
	src := coalesce(get(data, "data"), data)
	for _, k := range keys {
		v := get(src, strings.Split(k, ".")...)
		if !nullish(v) && v != "" {
			return stringPtr(strings.ToUpper(jsString(v)))
		}
	}
	return nil
}

func validCoordinates(lat, lon float64) bool {
	return !math.IsNaN(lat) && !math.IsInf(lat, 0) && lat >= -90 && lat <= 90 &&
		!math.IsNaN(lon) && !math.IsInf(lon, 0) && lon >= -180 && lon <= 180
}

// CompactResolvedLocationResult is compactResolvedLocation (api.mjs:740).
type CompactResolvedLocationResult struct {
	Valid       bool    `json:"valid"`
	Reason      string  `json:"reason,omitempty"`
	Title       Value   `json:"title,omitempty"`
	CountryCode *string `json:"country_code"`
	CityCode    *string `json:"city_code"`
	Latitude    Number  `json:"latitude"`
	Longitude   Number  `json:"longitude"`
	Accuracy    Number  `json:"accuracy"`
}

// CompactResolvedLocation is compactResolvedLocation (api.mjs:740).
func CompactResolvedLocation(data Value) *CompactResolvedLocationResult {
	src := coalesce(get(data, "data"), data)
	if nullish(src) {
		src = map[string]any{}
	}
	lat, lon := coordinatePair(src)
	cc := locationCode(src, "countryCode", "country_code", "country.code", "address.countryCode")
	city := locationCode(src, "cityCode", "city_code", "city.code", "address.cityCode")
	valid := validCoordinates(lat, lon) && cc != nil && reCountryCode.MatchString(*cc)
	out := &CompactResolvedLocationResult{
		Valid:       valid,
		Title:       firstValue(get(src, "title"), get(src, "name"), get(src, "description"), get(src, "formattedAddress"), get(src, "address")),
		CountryCode: cc,
		CityCode:    city,
		Latitude:    Number(lat),
		Longitude:   Number(lon),
		Accuracy:    Number(jsNumber(firstValue(get(src, "accuracy"), get(src, "location", "accuracy"), 0))),
	}
	if !valid {
		out.Reason = "invalid_resolved_location"
	}
	return out
}

// CompactDeliveryPointInfoResult is compactDeliveryPointInfo (api.mjs:759).
type CompactDeliveryPointInfoResult struct {
	Deliverable bool    `json:"deliverable"`
	CountryCode *string `json:"country_code"`
	CityCode    *string `json:"city_code"`
}

// CompactDeliveryPointInfo is compactDeliveryPointInfo (api.mjs:759).
func CompactDeliveryPointInfo(data Value) *CompactDeliveryPointInfoResult {
	src := coalesce(get(data, "data"), data)
	if nullish(src) {
		src = map[string]any{}
	}
	d := firstValue(get(src, "valid"), get(src, "deliverable"), get(src, "isDeliverable"), get(src, "serviceable"), get(src, "isServiceable"), get(src, "deliveryAvailable"), get(src, "isValid"))
	b, isBool := d.(bool)
	return &CompactDeliveryPointInfoResult{
		Deliverable: (isBool && b) || strings.ToLower(jsString(d)) == "true",
		CountryCode: locationCode(src, "countryCode", "country_code", "country.code", "address.countryCode"),
		CityCode:    locationCode(src, "cityCode", "city_code", "city.code", "address.cityCode"),
	}
}

var reNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func stripMarks(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if r >= 0x300 && r <= 0x36f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func normalizeMatchText(v Value) string {
	s := ""
	if truthy(v) {
		s = jsString(v)
	}
	s = strings.ToLower(stripMarks(s))
	return strings.TrimFunc(reNonAlnum.ReplaceAllString(s, " "), unicode.IsSpace)
}

func queryMatches(haystack, q string) bool {
	h := normalizeMatchText(haystack)
	n := normalizeMatchText(q)
	if h == "" || n == "" {
		return false
	}
	if strings.Contains(h, n) {
		return true
	}
	seen := map[string]bool{}
	tokens := 0
	for _, t := range strings.Split(n, " ") {
		if len([]rune(t)) <= 1 || seen[t] {
			continue
		}
		seen[t] = true
		tokens++
		if !strings.Contains(h, t) {
			return false
		}
	}
	return tokens > 0
}

func numberClose(a, b Value) bool {
	if nullish(a) || nullish(b) {
		return false
	}
	return math.Abs(jsNumber(a)-jsNumber(b)) < 0.00001
}

func currentMatchesAddress(current Location, address Value) bool {
	if !truthy(address) {
		return false
	}
	cur := Value(map[string]any(current))
	sameCode := func(key string) bool {
		a, b := get(cur, key), get(address, key)
		return !truthy(a) || !truthy(b) || strings.EqualFold(jsString(a), jsString(b))
	}
	coord := (numberClose(get(cur, "latitude"), get(address, "latitude")) && numberClose(get(cur, "longitude"), get(address, "longitude"))) ||
		(numberClose(get(cur, "latitude"), get(address, "originalLatitude")) && numberClose(get(cur, "longitude"), get(address, "originalLongitude")))
	return sameCode("countryCode") && sameCode("cityCode") && coord
}

// SetLocationArgs are the glovo_set_location arguments for a saved address.
type SetLocationArgs struct {
	CountryCode string `json:"country_code"`
	CityCode    string `json:"city_code"`
	Latitude    string `json:"latitude"`
	Longitude   string `json:"longitude"`
}

func locationArgsFromAddress(address Value) *SetLocationArgs {
	cc := firstValue(get(address, "countryCode"), get(address, "country_code"))
	city := firstValue(get(address, "cityCode"), get(address, "city_code"))
	lat := firstValue(get(address, "latitude"), get(address, "lat"))
	lon := firstValue(get(address, "longitude"), get(address, "lon"), get(address, "lng"))
	if !validLocationCodes(cc, city) || !validCoordinates(jsNumber(lat), jsNumber(lon)) {
		return nil
	}
	return &SetLocationArgs{
		CountryCode: strings.ToUpper(jsString(cc)),
		CityCode:    strings.ToUpper(jsString(city)),
		Latitude:    jsString(lat),
		Longitude:   jsString(lon),
	}
}

// SavedLocation is one compactSavedLocation entry (api.mjs:824).
type SavedLocation struct {
	ID              string           `json:"id,omitempty"`
	Label           Value            `json:"label,omitempty"`
	Subtitle        Value            `json:"subtitle,omitempty"`
	AddressLine     Value            `json:"address_line,omitempty"`
	Details         Value            `json:"details,omitempty"`
	Kind            Value            `json:"kind,omitempty"`
	Tag             Value            `json:"tag,omitempty"`
	City            Value            `json:"city,omitempty"`
	CountryCode     string           `json:"country_code,omitempty"`
	CityCode        string           `json:"city_code,omitempty"`
	Latitude        string           `json:"latitude,omitempty"`
	Longitude       string           `json:"longitude,omitempty"`
	Selected        bool             `json:"selected"`
	Default         bool             `json:"default"`
	MatchesQuery    *bool            `json:"matches_query,omitempty"`
	SetLocationArgs *SetLocationArgs `json:"set_location_args,omitempty"`
}

func compactSavedLocation(entry, address Value, current Location, matchText string) SavedLocation {
	args := locationArgsFromAddress(address)
	parts := []string{}
	searchable := []Value{
		get(entry, "title"), get(entry, "subtitle"), get(address, "addressLine"), get(address, "details"),
		get(address, "tag"), get(address, "kind"), get(address, "cityName"), get(address, "cityCode"), get(address, "countryCode"),
	}
	if fields := get(address, "fields"); !nullish(fields) {
		for _, f := range strictList(fields, "addresses[].fields") {
			searchable = append(searchable, get(f, "value"))
		}
	}
	for _, v := range searchable {
		if truthy(v) {
			parts = append(parts, jsString(v))
		}
	}
	def := firstValue(get(entry, "isDefault"), get(entry, "default"), get(entry, "defaultAddress"), get(address, "isDefault"), get(address, "default"), get(address, "defaultAddress"))
	defBool, isBool := def.(bool)
	s := SavedLocation{
		Label:       firstValue(get(entry, "title"), get(address, "tag"), get(address, "kind")),
		Subtitle:    firstValue(get(entry, "subtitle"), get(address, "cityName")),
		AddressLine: clean(get(address, "addressLine")),
		Details:     clean(get(address, "details")),
		Kind:        clean(get(address, "kind")),
		Tag:         clean(get(address, "tag")),
		City:        clean(get(address, "cityName")),
		Selected:    currentMatchesAddress(current, address),
		Default:     (isBool && defBool) || strings.ToLower(jsString(def)) == "true",
	}
	if id := get(address, "id"); !nullish(id) {
		s.ID = jsString(id)
	}
	if args != nil {
		s.CountryCode, s.CityCode, s.Latitude, s.Longitude = args.CountryCode, args.CityCode, args.Latitude, args.Longitude
		s.SetLocationArgs = args
	} else {
		if cc := get(address, "countryCode"); truthy(cc) {
			s.CountryCode = strings.ToUpper(jsString(cc))
		}
		if city := get(address, "cityCode"); truthy(city) {
			s.CityCode = strings.ToUpper(jsString(city))
		}
	}
	if matchText != "" {
		s.MatchesQuery = boolPtr(queryMatches(strings.Join(parts, " "), matchText))
	}
	return s
}

// CurrentLocation is the current_location block of SavedLocations.
type CurrentLocation struct {
	CountryCode             Value  `json:"country_code,omitempty"`
	CityCode                Value  `json:"city_code,omitempty"`
	Latitude                string `json:"latitude,omitempty"`
	Longitude               string `json:"longitude,omitempty"`
	SelectedSavedLocationID string `json:"selected_saved_location_id,omitempty"`
}

// SavedLocations is compactSavedLocations (api.mjs:859).
type SavedLocations struct {
	Count           int             `json:"count"`
	CurrentLocation CurrentLocation `json:"current_location"`
	Selected        *SavedLocation  `json:"selected"`
	Default         *SavedLocation  `json:"default"`
	Matched         *SavedLocation  `json:"matched"`
	SavedLocations  []SavedLocation `json:"saved_locations"`
}

// CompactSavedLocations is compactSavedLocations (api.mjs:859).
func CompactSavedLocations(data Value, current Location, matchText string) (out *SavedLocations, err error) {
	defer catchShape(&err)
	return compactSavedLocations(data, current, matchText), nil
}

func compactSavedLocations(data Value, current Location, matchText string) *SavedLocations {
	entries := coalesce(get(data, "data", "addresses"), get(data, "addresses"))
	var list0 []any
	if nullish(entries) {
		list0 = asArray(data)
	} else {
		list0 = strictList(entries, "addresses")
	}
	out := &SavedLocations{SavedLocations: []SavedLocation{}}
	for _, e := range list0 {
		keep := false
		if et := get(e, "entryType"); truthy(et) {
			keep = et == "SAVED_ADDRESS"
		} else {
			keep = truthy(get(e, "address"))
		}
		if !keep {
			continue
		}
		address := coalesce(get(e, "address"), e)
		if _, ok := address.(map[string]any); !ok {
			continue
		}
		out.SavedLocations = append(out.SavedLocations, compactSavedLocation(e, address, current, matchText))
	}
	for i := range out.SavedLocations {
		l := &out.SavedLocations[i]
		if out.Selected == nil && l.Selected {
			out.Selected = l
		}
		if out.Default == nil && l.Default {
			out.Default = l
		}
		if matchText != "" && out.Matched == nil && l.MatchesQuery != nil && *l.MatchesQuery {
			out.Matched = l
		}
	}
	out.Count = len(out.SavedLocations)
	cur := Value(map[string]any(current))
	out.CurrentLocation = CurrentLocation{
		CountryCode: clean(get(cur, "countryCode")),
		CityCode:    clean(get(cur, "cityCode")),
	}
	if lat := get(cur, "latitude"); !nullish(lat) {
		out.CurrentLocation.Latitude = jsString(lat)
	}
	if lon := get(cur, "longitude"); !nullish(lon) {
		out.CurrentLocation.Longitude = jsString(lon)
	}
	if out.Selected != nil {
		out.CurrentLocation.SelectedSavedLocationID = out.Selected.ID
	}
	return out
}
