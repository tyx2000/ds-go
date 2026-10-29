package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

type cacheKey struct{ method, params string }
type cacheStamp struct{ epoch, generation uint64 }
type cachedResponse struct {
	value     json.RawMessage
	expires   int64
	insertion uint64
}
type cacheOrder struct {
	key       cacheKey
	insertion uint64
}

// One default cache belongs to one Client connection, so private/public slots
// share its single server/principal and can never cross a configuration reload.
type responseCache struct {
	mu              sync.Mutex
	entries         map[cacheKey]cachedResponse
	generations     map[cacheKey]uint64
	order           []cacheOrder
	sequence, epoch uint64
	now             func() time.Time
}

func (cache *responseCache) clock() int64 {
	if cache.now != nil {
		return cache.now().UnixMilli()
	}
	return time.Now().UnixMilli()
}
func (cache *responseCache) capture(key cacheKey) cacheStamp {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.generations == nil {
		cache.generations = map[cacheKey]uint64{}
	}
	value := cache.generations[key]
	cache.generations[key] = value
	return cacheStamp{cache.epoch, value}
}
func (cache *responseCache) read(key cacheKey) (json.RawMessage, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	value, ok := cache.entries[key]
	if !ok || value.expires <= cache.clock() {
		return nil, false
	}
	return bytes.Clone(value.value), true
}
func (cache *responseCache) write(key cacheKey, value json.RawMessage, stamp cacheStamp, retainStale bool) {
	var hints struct {
		TTL   float64 `json:"ttlMs"`
		Scope string  `json:"cacheScope"`
	}
	_ = json.Unmarshal(value, &hints)
	ttl := hints.TTL
	if math.IsNaN(ttl) || math.IsInf(ttl, 0) || ttl < 0 {
		ttl = 0
	}
	ttl = min(ttl, float64((24*time.Hour)/time.Millisecond))
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if stamp.epoch != cache.epoch || stamp.generation != cache.generations[key] {
		return
	}
	if ttl <= 0 && !retainStale {
		return
	}
	if cache.entries == nil {
		cache.entries = map[cacheKey]cachedResponse{}
	}
	prior, exists := cache.entries[key]
	if !exists {
		cache.sequence++
		prior.insertion = cache.sequence
		if key.method == "resources/read" {
			count := 0
			for held := range cache.entries {
				if held.method == "resources/read" {
					count++
				}
			}
			for count >= 512 && len(cache.order) > 0 {
				old := cache.order[0]
				cache.order = cache.order[1:]
				if entry, ok := cache.entries[old.key]; ok && entry.insertion == old.insertion {
					delete(cache.entries, old.key)
					count--
				}
			}
			cache.order = append(cache.order, cacheOrder{key: key, insertion: prior.insertion})
		}
	}
	cache.entries[key] = cachedResponse{value: bytes.Clone(value), expires: cache.clock() + int64(ttl), insertion: prior.insertion}
}
func (cache *responseCache) evict(key cacheKey, alwaysBump bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.generations == nil {
		cache.generations = map[cacheKey]uint64{}
	}
	if _, known := cache.generations[key]; known || alwaysBump {
		cache.generations[key]++
	}
	delete(cache.entries, key)
	if key.method == "resources/read" {
		kept := cache.order[:0]
		for _, entry := range cache.order {
			if entry.key != key {
				kept = append(kept, entry)
			}
		}
		cache.order = kept
	}
}
func (cache *responseCache) reset() {
	cache.mu.Lock()
	cache.entries = nil
	cache.generations = nil
	cache.order = nil
	cache.epoch++
	cache.mu.Unlock()
}

func (c *Client) cacheAvailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return ErrClosed
	default:
		return nil
	}
}

func (c *Client) invalidateCache(message wire) {
	switch message.Method {
	case "notifications/tools/list_changed":
		c.cache.evict(cacheKey{method: "tools/list"}, true)
		c.mu.Lock()
		c.outputSchemas = nil
		c.outputErrors = nil
		c.paramHeaders = nil
		c.taskRequired = nil
		c.mu.Unlock()
	case "notifications/prompts/list_changed":
		c.cache.evict(cacheKey{method: "prompts/list"}, true)
	case "notifications/resources/list_changed":
		c.cache.evict(cacheKey{method: "resources/list"}, true)
		c.cache.evict(cacheKey{method: "resources/templates/list"}, true)
	case "notifications/resources/updated":
		var params map[string]json.RawMessage
		if json.Unmarshal(message.Params, &params) == nil {
			if raw, exists := params["uri"]; exists && string(raw) != "null" {
				var uri string
				if json.Unmarshal(raw, &uri) == nil {
					c.cache.evict(cacheKey{method: "resources/read", params: uri}, false)
				}
			}
		}
	}
}

func (c *Client) cachedList(ctx context.Context, method, key string, refresh bool) ([]json.RawMessage, error) {
	if err := c.cacheAvailable(ctx); err != nil {
		return nil, err
	}
	address := cacheKey{method: method}
	if !refresh {
		if hit, ok := c.cache.read(address); ok {
			var result map[string]json.RawMessage
			_ = json.Unmarshal(hit, &result)
			var values []json.RawMessage
			if err := json.Unmarshal(result[key], &values); err != nil {
				return nil, err
			}
			return values, nil
		}
	}
	stamp := c.cache.capture(address)
	var aggregate map[string]json.RawMessage
	var result, previous []json.RawMessage
	cursor := ""
	hasCursor := false
	for page := 0; page < 64; page++ {
		params := map[string]string{}
		if hasCursor {
			params["cursor"] = cursor
		}
		var answer map[string]json.RawMessage
		if err := c.Call(ctx, method, params, &answer); err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(answer[key], &items); err != nil || items == nil {
			if err == nil {
				err = errors.New("expected array")
			}
			return nil, fmt.Errorf("invalid MCP %s page: %w", method, err)
		}
		next, nextExists := "", false
		if raw, exists := answer["nextCursor"]; exists {
			if string(raw) == "null" || json.Unmarshal(raw, &next) != nil {
				return nil, errors.New("invalid MCP pagination cursor")
			}
			nextExists = true
		}
		if page > 0 && nextExists && next == cursor && samePageItems(items, previous) {
			break
		}
		if aggregate == nil {
			aggregate = answer
		}
		result = append(result, items...)
		if !nextExists {
			break
		}
		if page == 63 {
			return nil, fmt.Errorf("%s: exceeded listMaxPages (64); server pagination did not terminate", method)
		}
		cursor, hasCursor, previous = next, true, items
	}
	if result == nil {
		result = []json.RawMessage{}
	}
	if method == "tools/list" && c.http != nil && c.modern() {
		result = c.excludeInvalidHeaderTools(result)
	}
	aggregate[key] = mustJSON(result)
	delete(aggregate, "nextCursor")
	c.cache.write(address, mustJSON(aggregate), stamp, true)
	return result, nil
}

func samePageItems(a, b []json.RawMessage) bool {
	var first, second bytes.Buffer
	_ = json.Compact(&first, mustJSON(a))
	_ = json.Compact(&second, mustJSON(b))
	return bytes.Equal(first.Bytes(), second.Bytes())
}

func (c *Client) ReadResource(ctx context.Context, uri string) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.readResourceParams(ctx, map[string]string{"uri": uri}, &result)
	return result, err
}
func (c *Client) readResourceParams(ctx context.Context, params any, out any) error {
	if err := c.cacheAvailable(ctx); err != nil {
		return err
	}
	data, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var input struct {
		URI string `json:"uri"`
	}
	if json.Unmarshal(data, &input) != nil {
		return errors.New("invalid MCP resource params")
	}
	address := cacheKey{method: "resources/read", params: input.URI}
	if hit, ok := c.cache.read(address); ok {
		if out != nil {
			return json.Unmarshal(hit, out)
		}
		return nil
	}
	stamp := c.cache.capture(address)
	var result json.RawMessage
	if err := c.Call(ctx, "resources/read", params, &result); err != nil {
		return err
	}
	c.cache.write(address, result, stamp, false)
	if out != nil {
		return json.Unmarshal(result, out)
	}
	return nil
}
