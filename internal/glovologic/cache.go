package glovologic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"time"
)

const (
	cacheVersion  = 1
	cacheFileName = "history-cache.json"
)

// HistoryCache is the order-card cache of history-cache.mjs. The cache file
// is Dir/history-cache.json; the directory is created at mode 0700 and the file
// at 0600, and every write is atomic (temporary file, then rename). An empty
// Dir disables the cache.
type HistoryCache struct {
	Dir string
	Now func() time.Time // nil means time.Now
}

// CacheOptions are the getCachedOrderCards options.
type CacheOptions struct {
	Refresh    string // "incremental" (default) or "full"
	PageDelay  time.Duration
	MaxRetries int
}

// CacheMetadata is the public cache block.
type CacheMetadata struct {
	Status                           string  `json:"status"`
	Mode                             string  `json:"mode"`
	Stale                            bool    `json:"stale"`
	AgeBeforeRefreshSeconds          *int64  `json:"age_before_refresh_seconds"`
	RefreshedAt                      *string `json:"refreshed_at"`
	PagesFetched                     int     `json:"pages_fetched"`
	FullCursorPagesAtLastFullRefresh any     `json:"full_cursor_pages_at_last_full_refresh,omitempty"`
}

// CachedDiscovery is a discovery with cache metadata. Pages are the pages
// fetched by this call; Orders may include cached orders.
type CachedDiscovery struct {
	Discovery
	Cache CacheMetadata `json:"cache"`
}

func (h HistoryCache) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func accountHash(customerID string) string {
	sum := sha256.Sum256([]byte(customerID))
	return hex.EncodeToString(sum[:])
}

func isCompleteDiscovery(d *Discovery) bool {
	return d.StoppedReason == "no_next_cursor" || d.StoppedReason == "empty_page"
}

func orderKey(o Object) (string, bool) {
	id := o["order_id"]
	if nullish(id) {
		return "", false
	}
	return jsString(id), true
}

func uniqueOrders(orders []Object) []Object {
	seen := map[string]bool{}
	out := make([]Object, 0, len(orders))
	for _, o := range orders {
		id, ok := orderKey(o)
		if !ok || id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, o)
	}
	return out
}

func mergeOrders(fresh, cached []Object) []Object {
	index := map[string]int{}
	for i, o := range cached {
		index[jsString(o["order_id"])] = i
	}
	maxOverlap := -1
	for _, o := range fresh {
		if i, ok := index[jsString(o["order_id"])]; ok && i > maxOverlap {
			maxOverlap = i
		}
	}
	if maxOverlap < 0 {
		return uniqueOrders(fresh)
	}
	return uniqueOrders(append(append([]Object{}, fresh...), cached[maxOverlap+1:]...))
}

func cacheAgeSeconds(cache any, now time.Time) *int64 {
	s, ok := get(cache, "updated_at").(string)
	if !ok {
		return nil
	}
	updated, ok := parseDate(s)
	if !ok {
		return nil
	}
	age := int64(math.Max(0, math.Floor((float64(now.UnixMilli())-updated)/1000+0.5)))
	return &age
}

// validateCache is validateCache: "" when usable.
func validateCache(cache any, hash string) string {
	if v, ok := get(cache, "version").(json.Number); !ok || toNumber(v) != cacheVersion {
		return "corrupt"
	}
	if _, ok := get(cache, "orders").([]any); !ok || get(cache, "complete") != true {
		return "corrupt"
	}
	if get(cache, "account_hash") != hash {
		return "account_mismatch"
	}
	for i, c := range get(cache, "orders").([]any) {
		if checkOrderCard(c, idx("cache.orders", i)) != nil {
			return "corrupt"
		}
	}
	return ""
}

// ErrUnsafeCache marks a cache directory or file this package refuses to
// use: a symlink, something other than a directory or regular file, an
// entry owned by another user, or one readable or writable by group or
// others. Upstream tightens such paths with chmod; this port never changes
// the mode of anything it did not create.
var ErrUnsafeCache = errors.New("glovologic: unsafe history cache path")

func unsafeCache(path, why string) error {
	return fmt.Errorf("%w: %s %s", ErrUnsafeCache, path, why)
}

// checkPrivate refuses st unless it is a non-symlink entry of the wanted
// type, owned by this user, with no group or other permission bits.
func checkPrivate(path string, st fs.FileInfo, wantDir bool) error {
	// st comes from Lstat, so a symlink fails both type checks.
	switch {
	case wantDir && !st.IsDir():
		return unsafeCache(path, "is a symlink or not a directory")
	case !wantDir && !st.Mode().IsRegular():
		return unsafeCache(path, "is a symlink or not a regular file")
	case !ownedByMe(st):
		return unsafeCache(path, "is owned by another user")
	case st.Mode().Perm()&0o077 != 0:
		return unsafeCache(path, fmt.Sprintf("has mode %o, want no group or other access", st.Mode().Perm()))
	}
	return nil
}

// privateDir creates dir at 0700 when it is missing and otherwise only
// verifies it; an existing directory is never chmodded.
func privateDir(dir string) error {
	st, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		st, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	return checkPrivate(dir, st, true)
}

// loadPrivateJSON is loadPrivateJson (auth/store.mjs) without the chmod: a
// missing file is fs.ErrNotExist, an unsafe one is ErrUnsafeCache.
func loadPrivateJSON(file string) (any, error) {
	if err := privateDir(filepath.Dir(file)); err != nil {
		return nil, err
	}
	st, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if err := checkPrivate(file, st, false); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file) // #nosec G304 -- path is the configured cache file, checked above
	if err != nil {
		return nil, err
	}
	return decodeJSON(data)
}

// savePrivateJSON is savePrivateJson: write a 0600 temporary file in the
// same directory, then rename it over the target. The rename replaces the
// directory entry, so it never writes through a symlink.
func savePrivateJSON(file string, value any) error {
	dir := filepath.Dir(file)
	if err := privateDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(file)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func(e error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return e
	}
	// CreateTemp already uses 0600; this is the file we just created.
	if err := tmp.Chmod(0o600); err != nil {
		return cleanup(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, file); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func metadata(status, mode string, age *int64, d *Discovery, fullPages any, refreshedAt *string) CacheMetadata {
	return CacheMetadata{
		Status: status, Mode: mode, AgeBeforeRefreshSeconds: age, RefreshedAt: refreshedAt,
		PagesFetched: len(d.Pages), FullCursorPagesAtLastFullRefresh: fullPages,
	}
}

func ordersAsAny(orders []Object) []any {
	out := make([]any, len(orders))
	for i, o := range orders {
		out[i] = o
	}
	return out
}

// CachedOrderCards is getCachedOrderCards. customerID nil (no signed-in
// customer) or an empty Dir disables the cache and walks the full history.
// An incomplete walk is an error and leaves the cache file untouched; an
// unreadable, corrupt or foreign cache is rebuilt by a full walk.
func (h HistoryCache) CachedOrderCards(ctx context.Context, src OrderCardSource, customerID *string, o CacheOptions) (*CachedDiscovery, error) {
	walk := func(stop map[string]bool) (*Discovery, error) {
		return fetchDiscovery(ctx, src, CardOptions{Limit: 15, PageDelay: o.PageDelay, MaxRetries: o.MaxRetries, StopOrderIDs: stop})
	}
	if customerID == nil || h.Dir == "" {
		d, err := walk(nil)
		if err != nil {
			return nil, err
		}
		return &CachedDiscovery{Discovery: *d, Cache: metadata("disabled", "full", nil, d, len(d.Pages), nil)}, nil
	}
	hash := accountHash(*customerID)
	file := filepath.Join(h.Dir, cacheFileName)

	var cache any
	invalid := ""
	loaded, err := loadPrivateJSON(file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		invalid = "missing"
	case errors.Is(err, ErrUnsafeCache):
		return nil, err
	case err != nil:
		invalid = "corrupt"
	default:
		cache = loaded
		invalid = validateCache(cache, hash)
	}

	if o.Refresh == "full" || invalid != "" {
		d, err := walk(nil)
		if err != nil {
			return nil, err
		}
		if !isCompleteDiscovery(d) {
			return nil, fmt.Errorf("full history refresh stopped before cursor exhaustion (%s)", d.StoppedReason)
		}
		now := h.now()
		refreshedAt, err := isoString(float64(now.UnixMilli()))
		if err != nil {
			return nil, err
		}
		if err := savePrivateJSON(file, map[string]any{
			"version": cacheVersion, "account_hash": hash, "updated_at": refreshedAt, "complete": true,
			"full_cursor_pages": len(d.Pages), "orders": ordersAsAny(d.Orders),
		}); err != nil {
			return nil, err
		}
		status := invalid + "_rebuilt"
		if o.Refresh == "full" && invalid == "" {
			status = "forced_full"
		}
		return &CachedDiscovery{Discovery: *d, Cache: metadata(status, "full", cacheAgeSeconds(cache, h.now()), d, len(d.Pages), &refreshedAt)}, nil
	}

	cachedRaw := get(cache, "orders").([]any)
	cached := make([]Object, 0, len(cachedRaw))
	known := map[string]bool{}
	for _, c := range cachedRaw {
		obj := c.(map[string]any)
		cached = append(cached, obj)
		known[jsString(obj["order_id"])] = true
	}
	d, err := walk(known)
	if err != nil {
		return nil, err
	}
	if d.StoppedReason != "known_order" && !isCompleteDiscovery(d) {
		return nil, fmt.Errorf("incremental history refresh did not reach a known order or cursor exhaustion (%s)", d.StoppedReason)
	}
	var orders []Object
	status := "cache_replaced_after_no_overlap"
	if d.StoppedReason == "known_order" {
		orders = mergeOrders(d.Orders, cached)
		status = "hit_refreshed"
	} else {
		orders = uniqueOrders(d.Orders)
	}
	now := h.now()
	refreshedAt, err := isoString(float64(now.UnixMilli()))
	if err != nil {
		return nil, err
	}
	next := copyObject(cache.(map[string]any))
	next["updated_at"] = refreshedAt
	next["orders"] = ordersAsAny(orders)
	if err := savePrivateJSON(file, next); err != nil {
		return nil, err
	}
	out := &CachedDiscovery{Discovery: *d}
	out.Orders = orders
	out.Count = len(orders)
	out.Cache = metadata(status, "incremental", cacheAgeSeconds(cache, h.now()), d, get(cache, "full_cursor_pages"), &refreshedAt)
	return out, nil
}
