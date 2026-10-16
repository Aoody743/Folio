package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	maxRedisClusterInfoBytes     = 64 << 10
	maxRedisClusterNodesBytes    = 1 << 20
	maxRedisClusterNodeLineBytes = 16 << 10
	maxRedisClusterNodes         = 256
	maxRedisClusterOutputBytes   = 256 << 10
	redisClusterSlotCount        = 16384
)

var redisClusterNodeID = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// CacheClusterInfo is a typed, bounded view of Redis/Valkey CLUSTER INFO.
// It intentionally contains no connection address or raw server response.
type CacheClusterInfo struct {
	ID            string    `json:"id"`
	Engine        string    `json:"engine"`
	Database      int       `json:"database"`
	Prefix        string    `json:"prefix"`
	State         string    `json:"state"`
	SlotsAssigned int       `json:"slots_assigned"`
	SlotsOK       int       `json:"slots_ok"`
	SlotsPFail    int       `json:"slots_pfail"`
	SlotsFail     int       `json:"slots_fail"`
	KnownNodes    int       `json:"known_nodes"`
	Size          int       `json:"size"`
	CurrentEpoch  *string   `json:"current_epoch,omitempty"`
	MyEpoch       *string   `json:"my_epoch,omitempty"`
	SampledAt     time.Time `json:"sampled_at"`
}

// CacheClusterSlotRange avoids expanding each of the 16,384 possible slots.
type CacheClusterSlotRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type CacheClusterSlotTransition struct {
	Slot      int    `json:"slot"`
	Direction string `json:"direction"`
}

// CacheClusterNode deliberately omits the CLUSTER NODES address column.
type CacheClusterNode struct {
	NodeID       string                       `json:"node_id"`
	MasterID     string                       `json:"master_id,omitempty"`
	Role         string                       `json:"role"`
	Status       string                       `json:"status"`
	Self         bool                         `json:"self"`
	LinkState    string                       `json:"link_state"`
	PingSent     string                       `json:"ping_sent"`
	PongReceived string                       `json:"pong_received"`
	ConfigEpoch  string                       `json:"config_epoch"`
	Slots        []CacheClusterSlotRange      `json:"slots"`
	Transitions  []CacheClusterSlotTransition `json:"transitions,omitempty"`
}

type CacheClusterNodes struct {
	ID        string             `json:"id"`
	Engine    string             `json:"engine"`
	Database  int                `json:"database"`
	Prefix    string             `json:"prefix"`
	Nodes     []CacheClusterNode `json:"nodes"`
	SampledAt time.Time          `json:"sampled_at"`
}

func (m *Manager) cacheClusterBindings() []core.Binding {
	id := obj(map[string]any{"id": idSchema()}, "id")
	return []core.Binding{
		m.binding("cache_cluster_info", "Read bounded Redis/Valkey cluster state counters without exposing peer addresses", "cache:read", true, id, m.cacheResources, m.cacheClusterInfo),
		m.binding("cache_cluster_nodes", "Read bounded Redis/Valkey node roles and slot ranges without exposing peer addresses", "cache:read", true, id, m.cacheResources, m.cacheClusterNodes),
	}
}

func (m *Manager) cacheClusterInfo(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	_, profile, err := m.cacheInput(ctx, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	if profile.Engine != "redis" && profile.Engine != "valkey" {
		return core.ActionResult{}, failure("unsupported_operation", "Cluster inspection requires Redis or Valkey", 400)
	}
	bounded, cancel := context.WithTimeout(ctx, cacheStatusTimeout)
	defer cancel()
	client, err := m.cacheClient(bounded, actor, profile)
	if err != nil {
		return core.ActionResult{}, err
	}
	defer client.Close()
	text, err := client.Do(bounded, "CLUSTER", "INFO").Text()
	if err != nil {
		return core.ActionResult{}, cacheClusterQueryError(err)
	}
	info, err := parseRedisClusterInfo(text)
	if err != nil {
		return core.ActionResult{}, failure("protocol_error", "Redis/Valkey cluster info response was invalid", 502)
	}
	info.ID = profile.ID
	info.Engine = profile.Engine
	info.Database = profile.Database
	info.Prefix = profile.Prefix
	info.SampledAt = time.Now().UTC()
	return cacheResult(profile, info), nil
}

func (m *Manager) cacheClusterNodes(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	_, profile, err := m.cacheInput(ctx, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	if profile.Engine != "redis" && profile.Engine != "valkey" {
		return core.ActionResult{}, failure("unsupported_operation", "Cluster inspection requires Redis or Valkey", 400)
	}
	bounded, cancel := context.WithTimeout(ctx, cacheStatusTimeout)
	defer cancel()
	client, err := m.cacheClient(bounded, actor, profile)
	if err != nil {
		return core.ActionResult{}, err
	}
	defer client.Close()
	text, err := client.Do(bounded, "CLUSTER", "NODES").Text()
	if err != nil {
		return core.ActionResult{}, cacheClusterQueryError(err)
	}
	nodes, err := parseRedisClusterNodes(text)
	if err != nil {
		return core.ActionResult{}, failure("protocol_error", "Redis/Valkey cluster node response was invalid", 502)
	}
	result := CacheClusterNodes{ID: profile.ID, Engine: profile.Engine, Database: profile.Database, Prefix: profile.Prefix, Nodes: nodes, SampledAt: time.Now().UTC()}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxRedisClusterOutputBytes {
		return core.ActionResult{}, failure("result_limit", "Redis/Valkey cluster node view exceeded the 256 KiB response limit", 502)
	}
	return cacheResult(profile, result), nil
}

func cacheClusterQueryError(err error) error {
	if isRedisClusterDisabled(err) {
		return failure("cluster_disabled", "Redis/Valkey cluster mode is disabled for this server", 412)
	}
	return failure("query_failed", "Redis/Valkey cluster information could not be read", 502)
}

func isRedisClusterDisabled(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "cluster support is disabled") || strings.Contains(message, "cluster support disabled") || strings.Contains(message, "cluster mode is not enabled") || strings.Contains(message, "cluster is disabled")
}

func parseRedisClusterInfo(raw string) (CacheClusterInfo, error) {
	if len(raw) > maxRedisClusterInfoBytes {
		return CacheClusterInfo{}, errors.New("oversized cluster info")
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > maxRedisInfoLines {
		return CacheClusterInfo{}, errors.New("too many cluster info fields")
	}
	values := map[string]string{}
	allowed := map[string]bool{
		"cluster_state":          true,
		"cluster_slots_assigned": true,
		"cluster_slots_ok":       true,
		"cluster_slots_pfail":    true,
		"cluster_slots_fail":     true,
		"cluster_known_nodes":    true,
		"cluster_size":           true,
		"cluster_current_epoch":  true,
		"cluster_my_epoch":       true,
	}
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if len(line) > maxRedisInfoLineBytes {
			return CacheClusterInfo{}, errors.New("oversized cluster info field")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || len(key) == 0 || len(key) > 128 || len(value) > maxRedisInfoLineBytes {
			return CacheClusterInfo{}, errors.New("invalid cluster info field")
		}
		if !allowed[key] {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return CacheClusterInfo{}, errors.New("duplicate cluster info field")
		}
		values[key] = value
	}
	if values["cluster_state"] != "ok" && values["cluster_state"] != "fail" {
		return CacheClusterInfo{}, errors.New("missing cluster state")
	}
	result := CacheClusterInfo{State: values["cluster_state"]}
	limits := map[string]uint64{
		"cluster_slots_assigned": redisClusterSlotCount,
		"cluster_slots_ok":       redisClusterSlotCount,
		"cluster_slots_pfail":    redisClusterSlotCount,
		"cluster_slots_fail":     redisClusterSlotCount,
		"cluster_known_nodes":    1 << 20,
		"cluster_size":           1 << 20,
	}
	parsed := map[string]uint64{}
	for key, limit := range limits {
		value, exists := values[key]
		if !exists {
			return CacheClusterInfo{}, fmt.Errorf("missing cluster counter %s", key)
		}
		counter, err := parseBoundedCacheClusterCounter(value, limit)
		if err != nil {
			return CacheClusterInfo{}, err
		}
		parsed[key] = counter
	}
	if parsed["cluster_slots_ok"]+parsed["cluster_slots_pfail"]+parsed["cluster_slots_fail"] > parsed["cluster_slots_assigned"] || parsed["cluster_size"] > parsed["cluster_known_nodes"] {
		return CacheClusterInfo{}, errors.New("inconsistent cluster counters")
	}
	result.SlotsAssigned = int(parsed["cluster_slots_assigned"])
	result.SlotsOK = int(parsed["cluster_slots_ok"])
	result.SlotsPFail = int(parsed["cluster_slots_pfail"])
	result.SlotsFail = int(parsed["cluster_slots_fail"])
	result.KnownNodes = int(parsed["cluster_known_nodes"])
	result.Size = int(parsed["cluster_size"])
	for key, field := range map[string]**string{"cluster_current_epoch": &result.CurrentEpoch, "cluster_my_epoch": &result.MyEpoch} {
		if value, exists := values[key]; exists {
			counter, err := parseBoundedCacheClusterCounter(value, maxCacheStatusCounter)
			if err != nil {
				return CacheClusterInfo{}, err
			}
			text := strconv.FormatUint(counter, 10)
			*field = &text
		}
	}
	return result, nil
}

func parseBoundedCacheClusterCounter(value string, maximum uint64) (uint64, error) {
	text, err := cacheStatusCounter(value, maximum)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(*text, 10, 64)
}

func parseRedisClusterNodes(raw string) ([]CacheClusterNode, error) {
	if len(raw) > maxRedisClusterNodesBytes {
		return nil, errors.New("oversized cluster nodes")
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > maxRedisClusterNodes+1 {
		return nil, errors.New("too many cluster nodes")
	}
	seenIDs := map[string]bool{}
	ownedSlots := make([]bool, redisClusterSlotCount)
	nodes := make([]CacheClusterNode, 0, len(lines))
	transitionCount := 0
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if len(line) > maxRedisClusterNodeLineBytes {
			return nil, errors.New("oversized cluster node line")
		}
		fields := strings.Fields(line)
		if len(fields) < 8 || len(fields) > redisClusterSlotCount+8 || !redisClusterNodeID.MatchString(fields[0]) || len(fields[1]) > 256 {
			return nil, errors.New("invalid cluster node fields")
		}
		nodeID := strings.ToLower(fields[0])
		if seenIDs[nodeID] {
			return nil, errors.New("duplicate cluster node id")
		}
		seenIDs[nodeID] = true
		flags, err := parseRedisClusterFlags(fields[2])
		if err != nil {
			return nil, err
		}
		role := ""
		for _, flag := range flags {
			switch flag {
			case "master":
				if role != "" {
					return nil, errors.New("ambiguous cluster node role")
				}
				role = "master"
			case "slave", "replica":
				if role != "" {
					return nil, errors.New("ambiguous cluster node role")
				}
				role = "replica"
			}
		}
		if role == "" || (fields[3] != "-" && !redisClusterNodeID.MatchString(fields[3])) {
			return nil, errors.New("invalid cluster node role or master id")
		}
		ping, err := parseBoundedCacheClusterCounter(fields[4], maxCacheStatusCounter)
		if err != nil {
			return nil, err
		}
		pong, err := parseBoundedCacheClusterCounter(fields[5], maxCacheStatusCounter)
		if err != nil {
			return nil, err
		}
		epoch, err := parseBoundedCacheClusterCounter(fields[6], maxCacheStatusCounter)
		if err != nil {
			return nil, err
		}
		if fields[7] != "connected" && fields[7] != "disconnected" {
			return nil, errors.New("invalid cluster link state")
		}
		status := "ok"
		for _, flag := range flags {
			switch flag {
			case "fail":
				status = "fail"
			case "pfail", "fail?":
				if status == "ok" {
					status = "pfail"
				}
			case "handshake":
				if status == "ok" {
					status = "handshake"
				}
			case "noaddr":
				if status == "ok" {
					status = "no_address"
				}
			}
		}
		if fields[7] == "disconnected" && status == "ok" {
			status = "disconnected"
		}
		node := CacheClusterNode{
			NodeID: strings.ToLower(fields[0]), Role: role, Status: status,
			Self: cacheClusterContainsString(flags, "myself"), LinkState: fields[7],
			PingSent: strconv.FormatUint(ping, 10), PongReceived: strconv.FormatUint(pong, 10), ConfigEpoch: strconv.FormatUint(epoch, 10),
			Slots: []CacheClusterSlotRange{},
		}
		if fields[3] != "-" {
			node.MasterID = strings.ToLower(fields[3])
		}
		seenTransitions := map[string]bool{}
		for _, token := range fields[8:] {
			if strings.HasPrefix(token, "[") || strings.HasSuffix(token, "]") {
				transition, parseErr := parseRedisClusterTransition(token)
				if parseErr != nil || role != "master" {
					return nil, errors.New("invalid migrating cluster slot")
				}
				key := transition.Direction + ":" + strconv.Itoa(transition.Slot)
				if seenTransitions[key] {
					return nil, errors.New("duplicate cluster slot transition")
				}
				seenTransitions[key] = true
				transitionCount++
				if transitionCount > redisClusterSlotCount {
					return nil, errors.New("too many cluster slot transitions")
				}
				node.Transitions = append(node.Transitions, transition)
				continue
			}
			if role != "master" {
				return nil, errors.New("replica reports stable slots")
			}
			rangeValue, parseErr := parseRedisClusterSlotRange(token)
			if parseErr != nil {
				return nil, parseErr
			}
			for slot := rangeValue.Start; slot <= rangeValue.End; slot++ {
				if ownedSlots[slot] {
					return nil, errors.New("duplicate cluster slot owner")
				}
				ownedSlots[slot] = true
			}
			if n := len(node.Slots); n > 0 && node.Slots[n-1].End+1 == rangeValue.Start {
				node.Slots[n-1].End = rangeValue.End
			} else {
				node.Slots = append(node.Slots, rangeValue)
			}
		}
		nodes = append(nodes, node)
	}
	if len(nodes) == 0 || len(nodes) > maxRedisClusterNodes {
		return nil, errors.New("empty or oversized cluster node list")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	return nodes, nil
}

func parseRedisClusterFlags(raw string) ([]string, error) {
	allowed := map[string]bool{"myself": true, "master": true, "slave": true, "replica": true, "fail": true, "fail?": true, "pfail": true, "handshake": true, "noaddr": true, "nofailover": true}
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || len(parts) > 16 {
		return nil, errors.New("invalid cluster flags")
	}
	seen := map[string]bool{}
	for _, flag := range parts {
		if !allowed[flag] || seen[flag] {
			return nil, errors.New("unknown or duplicate cluster flag")
		}
		seen[flag] = true
	}
	sort.Strings(parts)
	return parts, nil
}

func parseRedisClusterSlotRange(raw string) (CacheClusterSlotRange, error) {
	startText, endText, hasRange := strings.Cut(raw, "-")
	if hasRange && strings.Contains(endText, "-") {
		return CacheClusterSlotRange{}, errors.New("invalid cluster slot range")
	}
	if !hasRange {
		endText = startText
	}
	start, err := parseBoundedCacheClusterCounter(startText, redisClusterSlotCount-1)
	if err != nil {
		return CacheClusterSlotRange{}, err
	}
	end, err := parseBoundedCacheClusterCounter(endText, redisClusterSlotCount-1)
	if err != nil || end < start {
		return CacheClusterSlotRange{}, errors.New("invalid cluster slot range")
	}
	return CacheClusterSlotRange{Start: int(start), End: int(end)}, nil
}

func parseRedisClusterTransition(raw string) (CacheClusterSlotTransition, error) {
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]") {
		return CacheClusterSlotTransition{}, errors.New("invalid cluster slot transition")
	}
	body := raw[1 : len(raw)-1]
	direction, target := "", ""
	var slotText string
	if slot, peer, ok := strings.Cut(body, "->-"); ok {
		slotText, target, direction = slot, peer, "migrating"
	} else if slot, peer, ok := strings.Cut(body, "-<-"); ok {
		slotText, target, direction = slot, peer, "importing"
	} else {
		return CacheClusterSlotTransition{}, errors.New("invalid cluster slot transition")
	}
	slot, err := parseBoundedCacheClusterCounter(slotText, redisClusterSlotCount-1)
	if err != nil || !redisClusterNodeID.MatchString(target) {
		return CacheClusterSlotTransition{}, errors.New("invalid cluster slot transition")
	}
	return CacheClusterSlotTransition{Slot: int(slot), Direction: direction}, nil
}

func cacheClusterContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
