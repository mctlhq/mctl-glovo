package glovoapi

import (
	"context"
	"regexp"
)

// LocationUpdate is the glovo_set_location input (server.mjs:102). The tool
// always passes all five keys to setLocation, so a field left empty (nil or
// "") removes that stored field instead of keeping it: omitting the country
// clears the stored country. Other stored keys (accuracy, perseus ids, ...)
// are kept. Latitude and Longitude take a string or a number.
type LocationUpdate struct {
	CountryCode  string
	CityCode     string
	Latitude     Value
	Longitude    Value
	LanguageCode string
}

// SetLocation is the glovo_set_location tool: setLocation (api.mjs:145) with
// every LocationUpdate field, so omitted fields clear. The updated session
// is persisted, which also replaces a session file that failed to load.
func (c *Client) SetLocation(u LocationUpdate) (Location, error) {
	return c.SetLocationContext(context.Background(), u)
}

// SetLocationContext is SetLocation with a context for the token store write
// of a hosted client. A file-backed client ignores ctx.
func (c *Client) SetLocationContext(ctx context.Context, u LocationUpdate) (Location, error) {
	str := func(v string) Value {
		if v == "" {
			return nil
		}
		return v
	}
	return c.setLocation(ctx, map[string]any{
		"countryCode":  str(u.CountryCode),
		"cityCode":     str(u.CityCode),
		"latitude":     u.Latitude,
		"longitude":    u.Longitude,
		"languageCode": str(u.LanguageCode),
	})
}

// mergeLocation returns a copy of sess whose location has update applied:
// every key in update overrides the stored one and a key mapped to nil
// removes that field, like the upstream spread of undefined values followed
// by cleanObject. Keys not in update are kept.
func mergeLocation(sess Session, update map[string]any) Session {
	next := map[string]any{}
	if cur, ok := get(map[string]any(sess), "location").(map[string]any); ok {
		for k, v := range cur {
			next[k] = v
		}
	}
	for k, v := range update {
		next[k] = v
	}
	for k, v := range next {
		if clean(v) == nil {
			delete(next, k)
		}
	}
	s := make(Session, len(sess)+1)
	for k, v := range sess {
		s[k] = v
	}
	s["location"] = next
	return s
}

// setLocation is setLocation (api.mjs:145). A hosted client stores the
// change through its SessionWriter, onto the session the store holds, so the
// location lands next to the latest rotated tokens. Without a writer it
// refuses instead of keeping an unsaved copy.
func (c *Client) setLocation(ctx context.Context, update map[string]any) (Location, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hosted() {
		if c.writer == nil {
			return nil, ErrHostedNoFile
		}
		s, err := c.writer(ctx, func(cur Session) (Session, error) {
			return mergeLocation(cur, update), nil
		})
		if err != nil {
			return nil, err
		}
		c.session, c.loadErr = s, nil
		return c.locationLocked(), nil
	}
	s := mergeLocation(c.session, update)
	if err := SaveSession(c.sessionPath, s, c.now()); err != nil {
		return nil, err
	}
	c.session, c.loadErr = s, nil
	return c.locationLocked(), nil
}

// SelectLocationResult is the selectLocation result.
type SelectLocationResult struct {
	Selected    bool    `json:"selected"`
	Deliverable bool    `json:"deliverable"`
	Title       any     `json:"title,omitempty"`
	CityCode    *string `json:"city_code"`
	CountryCode *string `json:"country_code"`
	Reason      string  `json:"reason,omitempty"`
}

var (
	reCountryCode = regexp.MustCompile(`^[A-Z]{2}$`)
	reCityCode    = regexp.MustCompile(`(?i)^[A-Z0-9_-]{2,}$`)
)

func validLocationCodes(countryCode, cityCode Value) bool {
	cc, city := "", ""
	if truthy(countryCode) {
		cc = jsString(countryCode)
	}
	if truthy(cityCode) {
		city = jsString(cityCode)
	}
	return reCountryCode.MatchString(cc) && reCityCode.MatchString(city)
}

func strOrNil(s *string) Value {
	if s == nil {
		return nil
	}
	return *s
}

func orStr(a, b *string) *string {
	if a != nil && *a != "" {
		return a
	}
	if b != nil && *b != "" {
		return b
	}
	return nil
}

// SelectLocation is selectLocation (api.mjs:197): resolve a place, check
// guest serviceability and persist only a valid, deliverable location.
func (c *Client) SelectLocation(ctx context.Context, placeID, provider string) (*SelectLocationResult, error) {
	raw, err := c.ResolveAddress(ctx, placeID, provider)
	if err != nil {
		return nil, err
	}
	resolved := CompactResolvedLocation(raw)
	if !resolved.Valid {
		return &SelectLocationResult{Title: resolved.Title, Reason: resolved.Reason}, nil
	}
	cc := ""
	if resolved.CountryCode != nil {
		cc = *resolved.CountryCode
	}
	serviceRaw, err := c.DeliveryPointInfo(ctx, float64(resolved.Latitude), float64(resolved.Longitude), cc)
	if err != nil {
		return nil, err
	}
	service := CompactDeliveryPointInfo(serviceRaw)
	cityCode := orStr(service.CityCode, resolved.CityCode)
	countryCode := orStr(service.CountryCode, resolved.CountryCode)
	if !service.Deliverable {
		return &SelectLocationResult{Title: resolved.Title, CityCode: cityCode, CountryCode: countryCode, Reason: "not_deliverable"}, nil
	}
	if !validLocationCodes(strOrNil(countryCode), strOrNil(cityCode)) {
		return &SelectLocationResult{Deliverable: true, Title: resolved.Title, CityCode: cityCode, CountryCode: countryCode, Reason: "invalid_codes"}, nil
	}
	if _, err := c.setLocation(ctx, map[string]any{
		"countryCode": *countryCode,
		"cityCode":    *cityCode,
		"latitude":    jsNumberString(float64(resolved.Latitude)),
		"longitude":   jsNumberString(float64(resolved.Longitude)),
		"accuracy":    jsNumberString(float64(resolved.Accuracy)),
	}); err != nil {
		return nil, err
	}
	return &SelectLocationResult{Selected: true, Deliverable: true, Title: resolved.Title, CityCode: cityCode, CountryCode: countryCode}, nil
}
