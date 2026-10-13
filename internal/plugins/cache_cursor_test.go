package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestCacheCursorExactUint64AndLegacySafeJSONNumbers(t *testing.T) {
	for _, raw := range []string{`0`, `9007199254740991`, `"0"`, `"9007199254740993"`, `"18446744073709551615"`} {
		var c cacheCursor
		if json.Unmarshal([]byte(raw), &c) != nil {
			t.Fatal("exact supported cursor rejected")
		}
		encoded, err := json.Marshal(c)
		if err != nil || encoded[0] != '"' {
			t.Fatal("cursor output must use an exact decimal string")
		}
	}
	for _, raw := range []string{`9007199254740992`, `18446744073709551615`, `"18446744073709551616"`, `-1`, `1.2`, `1e3`, `"01"`, `null`, `true`, `"-1"`} {
		var c cacheCursor
		if json.Unmarshal([]byte(raw), &c) == nil {
			t.Fatal("lossy or invalid cursor accepted")
		}
	}
}

func TestLiveRedisDecimalCursorContinuationRetainsEveryMember(t *testing.T) {
	s, a, _, fixtures := liveDriverFixture(t)
	var in CacheRegisterInput
	if json.Unmarshal(fixtures["redis"], &in) != nil {
		t.Fatal("Redis fixture unavailable")
	}
	liveAction(t, s, a, "cache_connection_register", cacheRegistration(in))
	base := "cursor:" + strconvTime()
	keys := []string{base + ":hash", base + ":set"}
	for i := 0; i < 16; i++ {
		keys = append(keys, fmt.Sprintf("%s:%d", base, i))
	}
	defer func() {
		raw, _ := json.Marshal(cacheInput{ID: in.ID, Keys: keys})
		if _, err := s.Execute(context.Background(), a, "cache_keys_delete", raw); err != nil {
			t.Error("isolated cursor fixture keys did not clean up", core.ErrorCode(err))
		}
	}()
	for _, key := range keys[2:] {
		liveAction(t, s, a, "cache_key_set", cacheInput{ID: in.ID, Key: key, Value: "scoped cursor fixture", TTL: 120})
	}
	for batch := 0; batch < 6; batch++ {
		fields := map[string]string{}
		members := []string{}
		for i := batch * 100; i < (batch+1)*100; i++ {
			fields["field:"+strconv.Itoa(i)] = "value"
			members = append(members, "member:"+strconv.Itoa(i))
		}
		liveAction(t, s, a, "cache_hash_set", cacheInput{ID: in.ID, Key: keys[0], Fields: fields})
		liveAction(t, s, a, "cache_set_add", cacheInput{ID: in.ID, Key: keys[1], Members: members})
	}
	for _, spec := range []struct {
		action, key string
		want        int
	}{{"cache_keys_scan", "", len(keys)}, {"cache_hash_read", keys[0], 600}, {"cache_set_read", keys[1], 600}} {
		seen := map[string]bool{}
		cursor := "0"
		terminated := false
		for page := 0; page < 2000; page++ {
			input := map[string]any{"id": in.ID, "cursor": cursor, "limit": 10}
			if spec.key != "" {
				input["key"] = spec.key
			}
			result := liveAction(t, s, a, spec.action, input).(map[string]any)
			var ok bool
			cursor, ok = result["cursor"].(string)
			if !ok {
				t.Fatal("actual Redis cursor was serialized as a lossy number")
			}
			if _, err := strconv.ParseUint(cursor, 10, 64); err != nil {
				t.Fatal("actual Redis cursor was not an unsigned decimal string")
			}
			items := result["items"].([]string)
			step := 1
			if spec.action == "cache_hash_read" {
				step = 2
			}
			for i := 0; i < len(items); i += step {
				seen[items[i]] = true
			}
			if cursor == "0" {
				terminated = true
				break
			}
		}
		if !terminated || len(seen) < spec.want {
			t.Fatal("actual Redis cursor pagination did not terminate with all scoped members")
		}
		if spec.action == "cache_keys_scan" {
			for _, key := range keys {
				if !seen[key] {
					t.Fatal("actual SCAN lost a scoped fixture key")
				}
			}
		}
	}
}

func TestCacheCursorSchemaAndOverflowDeniedBeforeAnyConnection(t *testing.T) {
	s, a, m := databaseManagerFixture(t)
	p := CacheConnection{ID: "bounded", Engine: "redis", Host: "127.0.0.1", Port: 1, Prefix: "bounded"}
	raw, _ := json.Marshal(p)
	if _, err := m.deps.State.CompareAndSwap(context.Background(), "cache_connections", p.ID, 0, raw); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"cache_keys_scan", "cache_hash_read", "cache_set_read"} {
		for _, cursor := range []string{`9007199254740992`, `"18446744073709551616"`, `"-1"`} {
			input := json.RawMessage(`{"id":"bounded","cursor":` + cursor + `,"limit":1`)
			if action != "cache_keys_scan" {
				input = append(input, []byte(`,"key":"owned"`)...)
			}
			input = append(input, '}')
			if _, err := s.Execute(context.Background(), a, action, input); core.ErrorCode(err) != "invalid_input" {
				t.Fatal("invalid cursor reached a network operation", action, core.ErrorCode(err))
			}
		}
	}
}
