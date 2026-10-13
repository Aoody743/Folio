package plugins

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/redis/go-redis/v9"
)

func (m *Manager) cacheInput(ctx context.Context, raw json.RawMessage) (cacheInput, CacheConnection, error) {
	var in cacheInput
	if e := parse(raw, &in); e != nil {
		return in, CacheConnection{}, e
	}
	p, e := m.cacheProfile(ctx, in.ID)
	if e != nil {
		return in, p, e
	}
	if in.Key != "" && !cacheKey.MatchString(in.Key) {
		return in, p, failure("invalid_input", "Cache key contains whitespace/control characters or exceeds 240 bytes", 400)
	}
	for _, key := range in.Keys {
		if !cacheKey.MatchString(key) {
			return in, p, failure("invalid_input", "Invalid cache key", 400)
		}
	}
	if p.Engine == "memcached" {
		keys := append([]string{in.Key}, in.Keys...)
		for _, key := range keys {
			if key != "" && len(physicalCacheKey(p, key)) > 250 {
				return in, p, failure("invalid_input", "Memcached's physical namespace key exceeds 250 bytes", 400)
			}
		}
	}
	return in, p, nil
}
func physicalCacheKey(p CacheConnection, key string) string { return p.Prefix + ":" + key }
func cacheResult(p CacheConnection, data any) core.ActionResult {
	return core.ActionResult{Data: data, Resources: []string{CacheResource(p)}}
}
func cacheMutationError(err error) error {
	if err == nil {
		return nil
	}
	return failure("operation_recovery_required", "Cache mutation was not acknowledged; inspect the key before a new operation", 409)
}
func (m *Manager) cacheGet(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	in, p, e := m.cacheInput(ctx, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if p.Engine == "memcached" {
		value, found, e := memcacheGet(bounded, p, physicalCacheKey(p, in.Key))
		if e != nil {
			return core.ActionResult{}, e
		}
		return cacheResult(p, map[string]any{"key": in.Key, "found": found, "value": value}), nil
	}
	client, e := m.cacheClient(bounded, a, p)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Close()
	key := physicalCacheKey(p, in.Key)
	size, e := client.StrLen(bounded, key).Result()
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "Cache key could not be read or has a different type", 400)
	}
	if size > 128<<10 {
		return core.ActionResult{}, failure("result_limit", "Cache value exceeds 128 KiB", 413)
	}
	// GETRANGE bounds the wire allocation even when another client replaces
	// the value between STRLEN and this request.
	value, e := client.GetRange(bounded, key, 0, 128<<10).Result()
	if e == redis.Nil {
		return cacheResult(p, map[string]any{"key": in.Key, "found": false}), nil
	}
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "Cache key could not be read or has a different type", 400)
	}
	if len(value) > 128<<10 {
		return core.ActionResult{}, failure("result_limit", "Cache value exceeds 128 KiB", 413)
	}
	if value == "" {
		exists, e := client.Exists(bounded, key).Result()
		if e != nil {
			return core.ActionResult{}, failure("query_failed", "Cache key existence could not be read", 502)
		}
		if exists == 0 {
			return cacheResult(p, map[string]any{"key": in.Key, "found": false}), nil
		}
	}
	return cacheResult(p, map[string]any{"key": in.Key, "found": true, "value": value}), nil
}
func (m *Manager) cacheSet(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	in, p, e := m.cacheInput(ctx, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	if len(in.Value) > 32768 || in.TTL < 0 || in.TTL > 2592000 {
		return core.ActionResult{}, failure("invalid_input", "Cache value/TTL exceeds limits", 400)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if p.Engine == "memcached" {
		if e = memcacheSet(bounded, p, physicalCacheKey(p, in.Key), in.Value, in.TTL); e != nil {
			return core.ActionResult{}, e
		}
	} else {
		client, e := m.cacheClient(bounded, a, p)
		if e != nil {
			return core.ActionResult{}, e
		}
		defer client.Close()
		if e = client.Set(bounded, physicalCacheKey(p, in.Key), in.Value, time.Duration(in.TTL)*time.Second).Err(); e != nil {
			return core.ActionResult{}, cacheMutationError(e)
		}
	}
	return cacheResult(p, map[string]any{"key": in.Key, "stored": true, "ttl_seconds": in.TTL}), nil
}
func (m *Manager) cacheDelete(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	in, p, e := m.cacheInput(ctx, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	if len(in.Keys) < 1 || len(in.Keys) > 100 {
		return core.ActionResult{}, failure("invalid_input", "Delete requires 1–100 explicit keys", 400)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys := make([]string, len(in.Keys))
	for i, key := range in.Keys {
		keys[i] = physicalCacheKey(p, key)
	}
	var deleted int64
	if p.Engine == "memcached" {
		for _, key := range keys {
			found, e := memcacheDelete(bounded, p, key)
			if e != nil {
				return core.ActionResult{}, e
			}
			if found {
				deleted++
			}
		}
	} else {
		client, e := m.cacheClient(bounded, a, p)
		if e != nil {
			return core.ActionResult{}, e
		}
		defer client.Close()
		deleted, e = client.Del(bounded, keys...).Result()
		if e != nil {
			return core.ActionResult{}, cacheMutationError(e)
		}
	}
	return cacheResult(p, map[string]any{"deleted_count": deleted}), nil
}
func (m *Manager) cacheScan(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	in, p, e := m.cacheInput(ctx, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	client, e := m.cacheClient(ctx, a, p)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Close()
	limit := in.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 1000 {
		return core.ActionResult{}, failure("invalid_input", "Scan limit must be 1–1000", 400)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys, cursor, e := client.Scan(bounded, uint64(in.Cursor), p.Prefix+":*", limit).Result()
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "Cache scan failed", 502)
	}
	visible := []string{}
	for _, key := range keys {
		if strings.HasPrefix(key, p.Prefix+":") {
			visible = append(visible, strings.TrimPrefix(key, p.Prefix+":"))
		}
	}
	// SCAN COUNT is a hint. Never discard part of a returned batch while
	// advancing its cursor, which would silently omit keys from pagination.
	if encoded, _ := json.Marshal(visible); len(encoded) > 1<<20 {
		return core.ActionResult{}, failure("result_limit", "Cache scan batch exceeds 1 MiB; use a smaller count", 413)
	}
	return cacheResult(p, map[string]any{"items": visible, "cursor": strconv.FormatUint(cursor, 10), "limit": limit}), nil
}
func (m *Manager) cacheType(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	in, p, e := m.cacheInput(ctx, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	client, e := m.cacheClient(ctx, a, p)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Close()
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	kind, e := client.Type(bounded, physicalCacheKey(p, in.Key)).Result()
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "Cache key type failed", 502)
	}
	ttl, e := client.TTL(bounded, physicalCacheKey(p, in.Key)).Result()
	if e != nil {
		return core.ActionResult{}, failure("query_failed", "Cache key TTL failed", 502)
	}
	seconds := int64(ttl / time.Second)
	if ttl < 0 {
		seconds = int64(ttl)
	}
	return cacheResult(p, map[string]any{"key": in.Key, "type": kind, "ttl_seconds": seconds}), nil
}
func (m *Manager) cacheCollectionBindings() []core.Binding {
	base := func() map[string]any { return map[string]any{"id": idSchema(), "key": cacheKeySchema()} }
	array := map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": cacheValueSchema()}
	hash := base()
	hash["fields"] = map[string]any{"type": "object", "minProperties": 1, "maxProperties": 100, "additionalProperties": cacheValueSchema()}
	members := base()
	members["members"] = array
	rangeFields := base()
	rangeFields["start"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 100000}
	rangeFields["stop"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 100999}
	scanFields := base()
	scanFields["cursor"] = cacheCursorSchema()
	scanFields["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}
	return []core.Binding{m.binding("cache_hash_set", "Set explicit bounded hash fields in one namespace", "cache:write", false, obj(hash, "id", "key", "fields"), m.cacheResources, m.cacheHashSet), m.binding("cache_hash_read", "Read one complete HSCAN batch and its continuation cursor", "cache:read", true, obj(scanFields, "id", "key"), m.cacheResources, m.cacheHashRead), m.binding("cache_set_add", "Add bounded members to a namespaced set", "cache:write", false, obj(members, "id", "key", "members"), m.cacheResources, m.cacheSetAdd), m.binding("cache_set_read", "Read one complete SSCAN batch and its continuation cursor", "cache:read", true, obj(scanFields, "id", "key"), m.cacheResources, m.cacheSetRead), m.binding("cache_list_push", "Push bounded list values without retrying ambiguous writes", "cache:write", false, obj(members, "id", "key", "members"), m.cacheResources, m.cacheListPush), m.binding("cache_list_read", "Read a bounded list range", "cache:read", true, obj(rangeFields, "id", "key", "start", "stop"), m.cacheResources, m.cacheListRead)}
}
func (m *Manager) cacheCollection(ctx context.Context, a core.Actor, raw json.RawMessage, operation string) (core.ActionResult, error) {
	in, p, e := m.cacheInput(ctx, raw)
	if e != nil {
		return core.ActionResult{}, e
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client, e := m.cacheClient(bounded, a, p)
	if e != nil {
		return core.ActionResult{}, e
	}
	defer client.Close()
	key := physicalCacheKey(p, in.Key)
	var data any
	switch operation {
	case "hash_set":
		if len(in.Fields) < 1 || len(in.Fields) > 100 {
			return core.ActionResult{}, failure("invalid_input", "Hash fields exceed limits", 400)
		}
		count, e := client.HSet(bounded, key, in.Fields).Result()
		if e != nil {
			return core.ActionResult{}, cacheMutationError(e)
		}
		data = map[string]any{"added_count": count}
	case "hash_read":
		limit := in.Limit
		if limit == 0 {
			limit = 100
		}
		if limit < 1 || limit > 1000 {
			return core.ActionResult{}, failure("invalid_input", "Scan limit must be 1–1000", 400)
		}
		items, cursor, e := client.HScan(bounded, key, uint64(in.Cursor), "*", limit).Result()
		if e != nil {
			return core.ActionResult{}, failure("query_failed", "Hash scan failed", 400)
		}
		data = map[string]any{"items": items, "cursor": strconv.FormatUint(cursor, 10)}
	case "set_add", "list_push":
		if len(in.Members) < 1 || len(in.Members) > 100 {
			return core.ActionResult{}, failure("invalid_input", "Members exceed limits", 400)
		}
		members := make([]any, len(in.Members))
		for i, v := range in.Members {
			members[i] = v
		}
		var count int64
		var e error
		if operation == "set_add" {
			count, e = client.SAdd(bounded, key, members...).Result()
		} else {
			count, e = client.RPush(bounded, key, members...).Result()
		}
		if e != nil {
			return core.ActionResult{}, cacheMutationError(e)
		}
		data = map[string]any{"count": count}
	case "set_read":
		limit := in.Limit
		if limit == 0 {
			limit = 100
		}
		if limit < 1 || limit > 1000 {
			return core.ActionResult{}, failure("invalid_input", "Scan limit must be 1–1000", 400)
		}
		items, cursor, e := client.SScan(bounded, key, uint64(in.Cursor), "*", limit).Result()
		if e != nil {
			return core.ActionResult{}, failure("query_failed", "Set scan failed", 400)
		}
		data = map[string]any{"items": items, "cursor": strconv.FormatUint(cursor, 10)}
	case "list_read":
		if in.Start < 0 || in.Stop < in.Start || in.Stop-in.Start >= 1000 {
			return core.ActionResult{}, failure("invalid_input", "List range must contain at most 1000 entries", 400)
		}
		items, e := client.LRange(bounded, key, in.Start, in.Stop).Result()
		if e != nil {
			return core.ActionResult{}, failure("query_failed", "List range failed", 400)
		}
		data = map[string]any{"items": items}
	}
	encoded, _ := json.Marshal(data)
	if len(encoded) > 1<<20 {
		return core.ActionResult{}, failure("result_limit", "Cache collection result exceeds 1 MiB", 413)
	}
	return cacheResult(p, data), nil
}
func (m *Manager) cacheHashSet(ctx context.Context, a core.Actor, r json.RawMessage) (core.ActionResult, error) {
	return m.cacheCollection(ctx, a, r, "hash_set")
}
func (m *Manager) cacheHashRead(ctx context.Context, a core.Actor, r json.RawMessage) (core.ActionResult, error) {
	return m.cacheCollection(ctx, a, r, "hash_read")
}
func (m *Manager) cacheSetAdd(ctx context.Context, a core.Actor, r json.RawMessage) (core.ActionResult, error) {
	return m.cacheCollection(ctx, a, r, "set_add")
}
func (m *Manager) cacheSetRead(ctx context.Context, a core.Actor, r json.RawMessage) (core.ActionResult, error) {
	return m.cacheCollection(ctx, a, r, "set_read")
}
func (m *Manager) cacheListPush(ctx context.Context, a core.Actor, r json.RawMessage) (core.ActionResult, error) {
	return m.cacheCollection(ctx, a, r, "list_push")
}
func (m *Manager) cacheListRead(ctx context.Context, a core.Actor, r json.RawMessage) (core.ActionResult, error) {
	return m.cacheCollection(ctx, a, r, "list_read")
}
