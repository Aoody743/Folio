package plugins

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedisCacheStatusParsersExposeOnlyAllowlistedFieldsAndExactCounters(t *testing.T) {
	version, source, uptime, err := parseRedisServerStatus("redis", "# Server\r\nredis_version:8.0.1\r\nuptime_in_seconds:123\r\nmaster_password:upstream-secret\r\n")
	if err != nil || version != "8.0.1" || source != "redis_version" || uptime == nil || *uptime != "123" {
		t.Fatal("Redis server status parsing failed", err)
	}
	memory, err := parseRedisMemoryStatus("# Memory\nused_memory:9007199254740993\nused_memory_peak:9007199254740994\nmaxmemory:0\nused_memory_human:8.00M\n")
	if err != nil || memory.UsedBytes == nil || *memory.UsedBytes != "9007199254740993" || memory.PeakBytes == nil || *memory.PeakBytes != "9007199254740994" || memory.LimitBytes == nil || *memory.LimitBytes != "0" {
		t.Fatal("Redis memory status lost an exact counter", err)
	}
	persistence, err := parseRedisPersistenceStatus("# Persistence\naof_enabled:1\naof_rewrite_in_progress:0\naof_last_write_status:ok\nrdb_bgsave_in_progress:0\nrdb_changes_since_last_save:9007199254740995\nrdb_last_bgsave_status:err\nmaster_user:upstream-user\n")
	if err != nil || persistence == nil || persistence.AOFEnabled == nil || !*persistence.AOFEnabled || persistence.AOFRewriteInProgress == nil || *persistence.AOFRewriteInProgress || persistence.RDBChangesSinceSave == nil || *persistence.RDBChangesSinceSave != "9007199254740995" || persistence.RDBLastSaveStatus == nil || *persistence.RDBLastSaveStatus != "err" {
		t.Fatal("Redis persistence status parsing failed", err)
	}
	replication, err := parseRedisReplicationStatus("# Replication\nrole:master\nconnected_slaves:1\nmaster_repl_offset:9007199254740997\nslave0:ip=192.0.2.44,port=6379,state=online,offset=9007199254740996,lag=7\nmaster_host:10.4.5.6\nmaster_port:6379\nmaster_user:private-upstream-user\nmaster_password:private-upstream-password\nconnected_clients:14\n")
	if err != nil || replication == nil || replication.Role != "master" || replication.LinkState == nil || *replication.LinkState != "not_applicable" || replication.ConnectedSlaveCount == nil || *replication.ConnectedSlaveCount != "1" || replication.Offset == nil || *replication.Offset != "9007199254740997" || replication.LagSeconds == nil || *replication.LagSeconds != "7" {
		t.Fatal("Redis replication status parsing failed", err)
	}

	encoded, err := json.Marshal(CacheStatusResult{Version: version, VersionSource: source, UptimeSeconds: uptime, Memory: memory, Persistence: persistence, Replication: replication})
	if err != nil {
		t.Fatal(err)
	}
	public := string(encoded)
	for _, private := range []string{"master_host", "master_port", "master_user", "master_password", "192.0.2.44", "10.4.5.6", "private-upstream-user", "private-upstream-password", "connected_clients", "used_memory_human"} {
		if strings.Contains(public, private) {
			t.Fatal("Redis private or unlisted status escaped the typed result", private, public)
		}
	}
	for _, exact := range []string{`"used_bytes":"9007199254740993"`, `"offset":"9007199254740997"`, `"lag_seconds":"7"`} {
		if !strings.Contains(public, exact) {
			t.Fatal("status counter was not emitted as an exact decimal string", exact, public)
		}
	}
}

func TestRedisServerStatusUsesValkeyVersionAndLabelsCompatibilityFallback(t *testing.T) {
	version, source, _, err := parseRedisServerStatus("valkey", "# Server\nvalkey_version:8.0.1\nredis_version:7.2.4\n")
	if err != nil || version != "8.0.1" || source != "valkey_version" {
		t.Fatalf("Valkey native version was not preferred: version=%q source=%q err=%v", version, source, err)
	}
	version, source, _, err = parseRedisServerStatus("valkey", "# Server\nredis_version:7.2.4\n")
	if err != nil || version != "7.2.4" || source != "redis_version_compatibility" {
		t.Fatalf("Valkey compatibility fallback was not labeled: version=%q source=%q err=%v", version, source, err)
	}
	version, source, _, err = parseRedisServerStatus("redis", "# Server\nredis_version:7.2.4\nvalkey_version:8.0.1\n")
	if err != nil || version != "7.2.4" || source != "redis_version" {
		t.Fatalf("Redis version selection changed: version=%q source=%q err=%v", version, source, err)
	}
}

func TestRedisReplicaStatusAndUnknownOptionalFields(t *testing.T) {
	status, err := parseRedisReplicationStatus("# Replication\nrole:slave\nmaster_link_status:down\nslave_repl_offset:9007199254740993\nconnected_slaves:0\nmaster_host:198.51.100.9\nmaster_user:private-user\nmaster_password:private-secret\nfuture_replication_metric:ignored\n")
	if err != nil || status == nil || status.Role != "replica" || status.LinkState == nil || *status.LinkState != "down" || status.Offset == nil || *status.Offset != "9007199254740993" || status.ConnectedSlaveCount == nil || *status.ConnectedSlaveCount != "0" || status.LagSeconds != nil {
		t.Fatal("Redis replica status or optional-field handling failed", err)
	}
	encoded, err := json.Marshal(status)
	if err != nil || strings.Contains(string(encoded), "master_host") || strings.Contains(string(encoded), "198.51.100.9") || strings.Contains(string(encoded), "private-secret") {
		t.Fatal("replica status leaked upstream details", string(encoded), err)
	}
	if status, err = parseRedisReplicationStatus("role:sentinel\nmaster_host:should-not-appear\n"); err != nil || status != nil {
		t.Fatal("unknown replication role was reported as a known role", err)
	}
	if persistence, err := parseRedisPersistenceStatus("# Persistence\nfuture_aof_flag:enabled\n"); err != nil || persistence != nil {
		t.Fatal("unknown optional persistence field was made into a health claim", err)
	}
}

func TestCacheStatusRejectsMalformedDuplicateAndOutOfRangeCounters(t *testing.T) {
	for _, sample := range []string{
		"used_memory:-1\n",
		"used_memory:+1\n",
		"used_memory:1152921504606846977\n", // above 1 EiB
		"used_memory:1\nused_memory:2\n",
		"used_memory:18446744073709551616\n",
	} {
		if _, err := parseRedisMemoryStatus(sample); err == nil {
			t.Fatal("invalid Redis memory status was accepted", sample)
		}
	}
	for _, sample := range []string{
		"redis_version:7.2.1\nuptime_in_seconds:-1\n",
		"redis_version:7.2.1\nuptime_in_seconds:3153600001\n",
		"redis_version:7.2.1\nredis_version:7.2.2\n",
		"redis_version:7.2.1;secret\n",
	} {
		if _, _, _, err := parseRedisServerStatus("redis", sample); err == nil {
			t.Fatal("invalid Redis server status was accepted", sample)
		}
	}
	for _, sample := range []string{
		"redis_version:7.2.4\nvalkey_version:8.0.1\nvalkey_version:8.0.2\n",
		"redis_version:7.2.4\nvalkey_version:invalid\n",
		"valkey_version:8.0.1\nvalkey_version:8.0.2\n",
	} {
		if _, _, _, err := parseRedisServerStatus("valkey", sample); err == nil {
			t.Fatal("invalid or duplicate Valkey version was accepted", sample)
		}
	}
	for _, sample := range []string{
		"role:master\nconnected_slaves:-1\n",
		"role:master\nconnected_slaves:1048577\n",
		"role:master\nmaster_repl_offset:-1\n",
		"role:master\nslave0:ip=127.0.0.1,state=online,lag:1\n",
	} {
		if _, err := parseRedisReplicationStatus(sample); err == nil {
			t.Fatal("invalid Redis replication status was accepted", sample)
		}
	}
	if _, err := parseRedisMemoryStatus(strings.Repeat("x", maxRedisInfoBytes+1)); err == nil {
		t.Fatal("oversized Redis INFO section was accepted")
	}
}

func TestMemcachedStatusStatsAllowlistAndStrictBounds(t *testing.T) {
	stats := newMemcachedStatusStats()
	if err := stats.addLine("STAT uptime 9007199254740993"); err == nil {
		t.Fatal("out-of-range Memcached uptime was accepted")
	}

	stats = newMemcachedStatusStats()
	for _, line := range []string{
		"STAT version 1.6.21",
		"STAT uptime 120",
		"STAT bytes 9007199254740993",
		"STAT limit_maxbytes 268435456",
		"STAT curr_items 42",
		"STAT pid 12345",
		"STAT curr_connections 7",
		"STAT address 127.0.0.1:11211",
	} {
		if err := stats.addLine(line); err != nil {
			t.Fatal("valid allowlisted Memcached stats line rejected", line, err)
		}
	}
	result, err := stats.result(CacheConnection{ID: "mc", Engine: "memcached"})
	if err != nil || result.Version != "1.6.21" || result.UptimeSeconds == nil || *result.UptimeSeconds != "120" || result.Memory == nil || result.Memory.UsedBytes == nil || *result.Memory.UsedBytes != "9007199254740993" || result.Memory.LimitBytes == nil || *result.Memory.LimitBytes != "268435456" || result.Memory.Items == nil || *result.Memory.Items != "42" {
		t.Fatal("Memcached status parsing failed", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	public := string(encoded)
	for _, private := range []string{"pid", "12345", "curr_connections", "address", "127.0.0.1:11211"} {
		if strings.Contains(public, private) {
			t.Fatal("unlisted Memcached stats escaped the typed result", private, public)
		}
	}
	if !strings.Contains(public, `"used_bytes":"9007199254740993"`) {
		t.Fatal("Memcached byte count was not serialized exactly", public)
	}

	for _, test := range []struct {
		line       string
		preloadVer bool
	}{
		{line: "STAT version 1.6.21;secret"},
		{line: "STAT uptime -1", preloadVer: true},
		{line: "STAT uptime +1", preloadVer: true},
		{line: "STAT bytes 1152921504606846977", preloadVer: true},
		{line: "STAT curr_items 1125899906842625", preloadVer: true},
		{line: "STAT version 1.6.21", preloadVer: true},
	} {
		candidate := newMemcachedStatusStats()
		if test.preloadVer {
			if err := candidate.addLine("STAT version 1.6.21"); err != nil {
				t.Fatal(err)
			}
		}
		if err := candidate.addLine(test.line); err == nil {
			t.Fatal("invalid or duplicate Memcached stats was accepted", test.line)
		}
	}
	if err := newMemcachedStatusStats().addLine("STAT bytes 1 extra"); err == nil {
		t.Fatal("malformed Memcached stats line was accepted")
	}
}

func TestRedisInfoAndMemcachedStatsResponseLimits(t *testing.T) {
	if _, _, _, err := parseRedisServerStatus("redis", strings.Repeat("x", maxRedisInfoBytes+1)); err == nil {
		t.Fatal("oversized server INFO response was accepted")
	}
	if _, err := newMemcachedStatusStats().result(CacheConnection{ID: "mc", Engine: "memcached"}); err == nil {
		t.Fatal("incomplete Memcached stats response was reported as healthy")
	}
}
