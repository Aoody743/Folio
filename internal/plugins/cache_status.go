package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/redis/go-redis/v9"
)

const (
	cacheStatusTimeout             = 8 * time.Second
	maxRedisInfoBytes              = 64 << 10
	maxRedisInfoLineBytes          = 4 << 10
	maxRedisInfoLines              = 1024
	maxMemcacheStatsBytes          = 32 << 10
	maxMemcacheStatsLines          = 256
	maxCacheStatusMemoryBytes      = uint64(1 << 60)
	maxCacheStatusCounter          = uint64(1<<63 - 1)
	maxCacheStatusUptimeSeconds    = uint64(3153600000) // 100 years
	maxCacheStatusReplicaCount     = uint64(1 << 20)
	maxCacheStatusItems            = uint64(1 << 50)
	maxCacheStatusVersionByteCount = 64
)

var cacheStatusVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){1,3}([._+-][A-Za-z0-9][A-Za-z0-9._+-]{0,31})?$`)

// CacheStatusResult is the public, read-only cache runtime status contract.
// Counters are canonical decimal strings so JSON consumers do not round large
// byte counts or replication offsets through IEEE-754 numbers.
type CacheStatusResult struct {
	ID            string                  `json:"id"`
	Engine        string                  `json:"engine"`
	Connected     bool                    `json:"connected"`
	Database      int                     `json:"database"`
	Prefix        string                  `json:"prefix"`
	Version       string                  `json:"version"`
	VersionSource string                  `json:"version_source"`
	UptimeSeconds *string                 `json:"uptime_seconds,omitempty"`
	Memory        *CacheMemoryStatus      `json:"memory,omitempty"`
	Persistence   *CachePersistenceStatus `json:"persistence,omitempty"`
	Replication   *CacheReplicationStatus `json:"replication,omitempty"`
	SampledAt     time.Time               `json:"sampled_at"`
}

type CacheMemoryStatus struct {
	UsedBytes  *string `json:"used_bytes,omitempty"`
	PeakBytes  *string `json:"peak_bytes,omitempty"`
	LimitBytes *string `json:"limit_bytes,omitempty"`
	Items      *string `json:"items,omitempty"`
}

type CachePersistenceStatus struct {
	AOFEnabled           *bool   `json:"aof_enabled,omitempty"`
	AOFRewriteInProgress *bool   `json:"aof_rewrite_in_progress,omitempty"`
	AOFLastWriteStatus   *string `json:"aof_last_write_status,omitempty"`
	RDBSaveInProgress    *bool   `json:"rdb_save_in_progress,omitempty"`
	RDBChangesSinceSave  *string `json:"rdb_changes_since_save,omitempty"`
	RDBLastSaveStatus    *string `json:"rdb_last_save_status,omitempty"`
}

type CacheReplicationStatus struct {
	Role                string  `json:"role"`
	LinkState           *string `json:"link_state,omitempty"`
	LagSeconds          *string `json:"lag_seconds,omitempty"`
	ConnectedSlaveCount *string `json:"connected_slave_count,omitempty"`
	Offset              *string `json:"offset,omitempty"`
}

var errInvalidCacheStatus = errors.New("invalid cache status response")

func (m *Manager) cacheStatus(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	_, p, err := m.cacheInput(ctx, raw)
	if err != nil {
		return core.ActionResult{}, err
	}

	bounded, cancel := context.WithTimeout(ctx, cacheStatusTimeout)
	defer cancel()

	var result CacheStatusResult
	switch p.Engine {
	case "memcached":
		stats, e := readMemcacheStatusStats(bounded, p)
		if e != nil {
			return core.ActionResult{}, e
		}
		result, e = stats.result(p)
		if e != nil {
			return core.ActionResult{}, failure("protocol_error", "Memcached runtime status omitted required version or memory fields", 502)
		}
	case "redis", "valkey":
		client, e := m.cacheClient(bounded, a, p)
		if e != nil {
			return core.ActionResult{}, e
		}
		defer client.Close()
		result, e = readRedisCacheStatus(bounded, client, p)
		if e != nil {
			return core.ActionResult{}, e
		}
	default:
		return core.ActionResult{}, failure("invalid_state", "Cache profile engine is unsupported", 500)
	}

	result.SampledAt = time.Now().UTC()
	return cacheResult(p, result), nil
}

func cacheStatusBase(p CacheConnection) CacheStatusResult {
	return CacheStatusResult{
		ID:        p.ID,
		Engine:    p.Engine,
		Connected: true,
		Database:  p.Database,
		Prefix:    p.Prefix,
	}
}

func readRedisCacheStatus(ctx context.Context, client *redis.Client, p CacheConnection) (CacheStatusResult, error) {
	result := cacheStatusBase(p)
	sections := []string{"server", "memory", "persistence", "replication"}
	totalBytes := 0
	for _, section := range sections {
		// go-redis v9.23 reads a complete RESP bulk reply before returning it;
		// it has no configurable bulk-reply limit. The fixed sections and this
		// post-read 64 KiB cap bound parsing/output, but not the SDK's transient
		// allocation for one oversized Redis response.
		raw, err := client.Info(ctx, section).Result()
		if err != nil {
			return CacheStatusResult{}, failure("query_failed", "Cache runtime status could not be read", 502)
		}
		if len(raw) > maxRedisInfoBytes || totalBytes > maxRedisInfoBytes-len(raw) {
			return CacheStatusResult{}, failure("result_limit", "Cache runtime status exceeded the 64 KiB response limit", 502)
		}
		totalBytes += len(raw)

		var parseErr error
		switch section {
		case "server":
			result.Version, result.VersionSource, result.UptimeSeconds, parseErr = parseRedisServerStatus(p.Engine, raw)
		case "memory":
			result.Memory, parseErr = parseRedisMemoryStatus(raw)
		case "persistence":
			result.Persistence, parseErr = parseRedisPersistenceStatus(raw)
		case "replication":
			result.Replication, parseErr = parseRedisReplicationStatus(raw)
		}
		if parseErr != nil {
			return CacheStatusResult{}, failure("protocol_error", "Cache runtime status response was invalid", 502)
		}
	}
	if result.Version == "" || result.Memory == nil || result.Memory.UsedBytes == nil {
		return CacheStatusResult{}, failure("protocol_error", "Cache runtime status omitted required version or memory fields", 502)
	}
	return result, nil
}

func parseRedisServerStatus(engine, raw string) (string, string, *string, error) {
	var redisVersion string
	var valkeyVersion string
	var uptime *string
	seen := map[string]bool{}
	err := eachRedisInfoField(raw, func(key, value string) error {
		switch key {
		case "redis_version", "valkey_version":
			if err := cacheStatusSeen(seen, key); err != nil || !cacheStatusVersionPattern.MatchString(value) || len(value) > maxCacheStatusVersionByteCount {
				return errInvalidCacheStatus
			}
			if key == "redis_version" {
				redisVersion = value
			} else {
				valkeyVersion = value
			}
		case "uptime_in_seconds":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			var err error
			uptime, err = cacheStatusCounter(value, maxCacheStatusUptimeSeconds)
			return err
		}
		return nil
	})
	if err != nil {
		return "", "", nil, errInvalidCacheStatus
	}
	switch engine {
	case "redis":
		if redisVersion == "" {
			return "", "", nil, errInvalidCacheStatus
		}
		return redisVersion, "redis_version", uptime, nil
	case "valkey":
		if valkeyVersion != "" {
			return valkeyVersion, "valkey_version", uptime, nil
		}
		if redisVersion != "" {
			return redisVersion, "redis_version_compatibility", uptime, nil
		}
	}
	return "", "", nil, errInvalidCacheStatus
}

func parseRedisMemoryStatus(raw string) (*CacheMemoryStatus, error) {
	status := &CacheMemoryStatus{}
	seen := map[string]bool{}
	err := eachRedisInfoField(raw, func(key, value string) error {
		var target **string
		switch key {
		case "used_memory":
			target = &status.UsedBytes
		case "used_memory_peak":
			target = &status.PeakBytes
		case "maxmemory":
			target = &status.LimitBytes
		default:
			return nil
		}
		if err := cacheStatusSeen(seen, key); err != nil {
			return err
		}
		parsed, err := cacheStatusCounter(value, maxCacheStatusMemoryBytes)
		if err == nil {
			*target = parsed
		}
		return err
	})
	if err != nil || status.UsedBytes == nil {
		return nil, errInvalidCacheStatus
	}
	return status, nil
}

func parseRedisPersistenceStatus(raw string) (*CachePersistenceStatus, error) {
	status := &CachePersistenceStatus{}
	seen := map[string]bool{}
	err := eachRedisInfoField(raw, func(key, value string) error {
		switch key {
		case "aof_enabled":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			parsed, err := cacheStatusBool(value)
			if err == nil {
				status.AOFEnabled = &parsed
			}
			return err
		case "aof_rewrite_in_progress":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			parsed, err := cacheStatusBool(value)
			if err == nil {
				status.AOFRewriteInProgress = &parsed
			}
			return err
		case "rdb_bgsave_in_progress":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			parsed, err := cacheStatusBool(value)
			if err == nil {
				status.RDBSaveInProgress = &parsed
			}
			return err
		case "rdb_changes_since_last_save":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			parsed, err := cacheStatusCounter(value, maxCacheStatusCounter)
			if err == nil {
				status.RDBChangesSinceSave = parsed
			}
			return err
		case "aof_last_write_status", "rdb_last_bgsave_status":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			if value != "ok" && value != "err" {
				return errInvalidCacheStatus
			}
			parsed := value
			if key == "aof_last_write_status" {
				status.AOFLastWriteStatus = &parsed
			} else {
				status.RDBLastSaveStatus = &parsed
			}
		}
		return nil
	})
	if err != nil {
		return nil, errInvalidCacheStatus
	}
	if status.AOFEnabled == nil && status.AOFRewriteInProgress == nil && status.AOFLastWriteStatus == nil && status.RDBSaveInProgress == nil && status.RDBChangesSinceSave == nil && status.RDBLastSaveStatus == nil {
		return nil, nil
	}
	return status, nil
}

func parseRedisReplicationStatus(raw string) (*CacheReplicationStatus, error) {
	var role string
	var linkState string
	var hasLinkState bool
	var slaveCount, replicaCount *string
	var masterOffset, slaveOffset, replicaOffset *string
	var maxLag *string
	var seenReplicas = map[string]bool{}
	seen := map[string]bool{}
	err := eachRedisInfoField(raw, func(key, value string) error {
		switch key {
		case "role":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			switch strings.ToLower(value) {
			case "master":
				role = "master"
			case "slave", "replica":
				role = "replica"
			default:
				role = ""
			}
		case "master_link_status":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			if value == "up" || value == "down" {
				linkState, hasLinkState = value, true
			}
		case "connected_slaves":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			var err error
			slaveCount, err = cacheStatusCounter(value, maxCacheStatusReplicaCount)
			return err
		case "connected_replicas":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			var err error
			replicaCount, err = cacheStatusCounter(value, maxCacheStatusReplicaCount)
			return err
		case "master_repl_offset":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			var err error
			masterOffset, err = cacheStatusCounter(value, maxCacheStatusCounter)
			return err
		case "slave_repl_offset":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			var err error
			slaveOffset, err = cacheStatusCounter(value, maxCacheStatusCounter)
			return err
		case "replica_repl_offset":
			if err := cacheStatusSeen(seen, key); err != nil {
				return err
			}
			var err error
			replicaOffset, err = cacheStatusCounter(value, maxCacheStatusCounter)
			return err
		default:
			if redisReplicaSummaryKey(key) {
				if seenReplicas[key] {
					return errInvalidCacheStatus
				}
				seenReplicas[key] = true
				lag, online, err := parseRedisReplicaSummary(value)
				if err != nil {
					return err
				}
				if online && lag != nil && (maxLag == nil || decimalStringGreater(*lag, *maxLag)) {
					maxLag = lag
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, errInvalidCacheStatus
	}
	if role == "" {
		return nil, nil
	}
	status := &CacheReplicationStatus{Role: role}
	if replicaCount != nil {
		status.ConnectedSlaveCount = replicaCount
	} else {
		status.ConnectedSlaveCount = slaveCount
	}
	if role == "master" {
		state := "not_applicable"
		status.LinkState = &state
		status.Offset = masterOffset
		status.LagSeconds = maxLag
	} else {
		if hasLinkState {
			status.LinkState = &linkState
		}
		if replicaOffset != nil {
			status.Offset = replicaOffset
		} else {
			status.Offset = slaveOffset
		}
	}
	return status, nil
}

func eachRedisInfoField(raw string, visit func(string, string) error) error {
	if len(raw) > maxRedisInfoBytes {
		return errInvalidCacheStatus
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > maxRedisInfoLines {
		return errInvalidCacheStatus
	}
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if len(line) > maxRedisInfoLineBytes {
			return errInvalidCacheStatus
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || key == "" {
			continue
		}
		if len(key) > 128 || len(value) > maxRedisInfoLineBytes {
			return errInvalidCacheStatus
		}
		if err := visit(key, value); err != nil {
			return err
		}
	}
	return nil
}

func cacheStatusSeen(seen map[string]bool, key string) error {
	if seen[key] {
		return errInvalidCacheStatus
	}
	seen[key] = true
	return nil
}

func cacheStatusCounter(value string, maximum uint64) (*string, error) {
	if value == "" || len(value) > 20 {
		return nil, errInvalidCacheStatus
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return nil, errInvalidCacheStatus
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n > maximum {
		return nil, errInvalidCacheStatus
	}
	canonical := strconv.FormatUint(n, 10)
	return &canonical, nil
}

func cacheStatusBool(value string) (bool, error) {
	switch value {
	case "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, errInvalidCacheStatus
	}
}

func redisReplicaSummaryKey(key string) bool {
	if !strings.HasPrefix(key, "slave") || len(key) <= len("slave") || len(key) > len("slave")+10 {
		return false
	}
	for i := len("slave"); i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return false
		}
	}
	return true
}

func parseRedisReplicaSummary(value string) (*string, bool, error) {
	if len(value) > maxRedisInfoLineBytes {
		return nil, false, errInvalidCacheStatus
	}
	var state string
	var lag *string
	seen := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		key, fieldValue, ok := strings.Cut(part, "=")
		if !ok {
			return nil, false, errInvalidCacheStatus
		}
		switch key {
		case "state":
			if err := cacheStatusSeen(seen, key); err != nil {
				return nil, false, err
			}
			state = fieldValue
		case "lag":
			if err := cacheStatusSeen(seen, key); err != nil {
				return nil, false, err
			}
			var err error
			lag, err = cacheStatusCounter(fieldValue, maxCacheStatusUptimeSeconds)
			if err != nil {
				return nil, false, err
			}
		}
	}
	return lag, state == "online", nil
}

func decimalStringGreater(left, right string) bool {
	if len(left) != len(right) {
		return len(left) > len(right)
	}
	return left > right
}

type memcachedStatusStats struct {
	version       string
	uptimeSeconds *string
	usedBytes     *string
	limitBytes    *string
	items         *string
	seen          map[string]bool
}

func newMemcachedStatusStats() *memcachedStatusStats {
	return &memcachedStatusStats{seen: make(map[string]bool)}
}

func (s *memcachedStatusStats) addLine(line string) error {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != "STAT" || fields[1] == "" {
		return errInvalidCacheStatus
	}
	key, value := fields[1], fields[2]
	switch key {
	case "version":
		if err := cacheStatusSeen(s.seen, key); err != nil || !cacheStatusVersionPattern.MatchString(value) || len(value) > maxCacheStatusVersionByteCount {
			return errInvalidCacheStatus
		}
		s.version = value
	case "uptime":
		if err := cacheStatusSeen(s.seen, key); err != nil {
			return err
		}
		var err error
		s.uptimeSeconds, err = cacheStatusCounter(value, maxCacheStatusUptimeSeconds)
		return err
	case "bytes":
		if err := cacheStatusSeen(s.seen, key); err != nil {
			return err
		}
		var err error
		s.usedBytes, err = cacheStatusCounter(value, maxCacheStatusMemoryBytes)
		return err
	case "limit_maxbytes":
		if err := cacheStatusSeen(s.seen, key); err != nil {
			return err
		}
		var err error
		s.limitBytes, err = cacheStatusCounter(value, maxCacheStatusMemoryBytes)
		return err
	case "curr_items":
		if err := cacheStatusSeen(s.seen, key); err != nil {
			return err
		}
		var err error
		s.items, err = cacheStatusCounter(value, maxCacheStatusItems)
		return err
	default:
		// The full stats output can contain process and connection details.
		// Do not retain any unlisted key/value pairs.
		return nil
	}
	return nil
}

func (s *memcachedStatusStats) result(p CacheConnection) (CacheStatusResult, error) {
	if s.version == "" || s.usedBytes == nil {
		return CacheStatusResult{}, errInvalidCacheStatus
	}
	result := cacheStatusBase(p)
	result.Version = s.version
	result.VersionSource = "memcached_stats"
	result.UptimeSeconds = s.uptimeSeconds
	result.Memory = &CacheMemoryStatus{UsedBytes: s.usedBytes, LimitBytes: s.limitBytes, Items: s.items}
	return result, nil
}
