package systemops

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

type HistorySample struct {
	Timestamp   time.Time      `json:"timestamp"`
	CPUPercent  *float64       `json:"cpu_percent"`
	Memory      *core.Usage    `json:"memory"`
	Disk        *core.Usage    `json:"disk"`
	LoadAverage []float64      `json:"load_average"`
	Network     []NetworkUsage `json:"network,omitempty"`
}

type NetworkUsage struct {
	Interface              string   `json:"interface"`
	ReceiveBytes           uint64   `json:"receive_bytes"`
	TransmitBytes          uint64   `json:"transmit_bytes"`
	ReceiveBytesPerSecond  *float64 `json:"receive_bytes_per_second"`
	TransmitBytesPerSecond *float64 `json:"transmit_bytes_per_second"`
}

// Counters are read-only kernel statistics; each interface is kept separate
// so bridges and virtual links are never silently summed twice.
func (p *Provider) sampleNetwork(now time.Time) []NetworkUsage {
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil
	}
	defer file.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	previous := p.networkPrevious
	p.networkPrevious = map[string]NetworkUsage{}
	seconds := now.Sub(p.networkAt).Seconds()
	p.networkAt = now
	out := []NetworkUsage{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, text, found := strings.Cut(scanner.Text(), ":")
		name = strings.TrimSpace(name)
		fields := strings.Fields(text)
		if !found || name == "lo" || len(fields) != 16 {
			continue
		}
		rx, er := strconv.ParseUint(fields[0], 10, 64)
		tx, et := strconv.ParseUint(fields[8], 10, 64)
		if er != nil || et != nil {
			continue
		}
		usage := NetworkUsage{Interface: name, ReceiveBytes: rx, TransmitBytes: tx}
		if prior, ok := previous[name]; ok && seconds > 0 && rx >= prior.ReceiveBytes && tx >= prior.TransmitBytes {
			rr := float64(rx-prior.ReceiveBytes) / seconds
			tr := float64(tx-prior.TransmitBytes) / seconds
			usage.ReceiveBytesPerSecond, usage.TransmitBytesPerSecond = &rr, &tr
		}
		p.networkPrevious[name] = usage
		out = append(out, usage)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Interface < out[j].Interface })
	return out
}

func (p *Provider) historyBindings() []core.Binding {
	return []core.Binding{
		p.bind("monitor_history", "Read actual persisted CPU/memory/disk/load samples in a bounded time range", "system:read", true, object(map[string]any{"start": text(64), "end": text(64), "limit": integer(1, 1000)}), hostResources, p.history),
		p.bind("monitor_history_prune", "Delete history older than the selected retention boundary", "system:write", false, object(map[string]any{"before": text(64)}, "before"), hostResources, func(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var in struct {
				Before string `json:"before"`
			}
			if err := input(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			before, err := time.Parse(time.RFC3339, in.Before)
			if err != nil || before.After(time.Now()) {
				return core.ActionResult{}, invalid("Invalid history deletion boundary")
			}
			count, err := p.pruneHistory(ctx, before)
			return core.ActionResult{Data: map[string]any{"buckets_deleted": count}}, err
		}),
	}
}
func (p *Provider) sample(ctx context.Context, now time.Time) error {
	status, err := p.options.Status(ctx)
	if err != nil {
		return err
	}
	sample := HistorySample{Timestamp: now.UTC(), CPUPercent: status.CPUPercent, Memory: status.Memory, Disk: status.Disk, LoadAverage: status.LoadAverage}
	sample.Network = p.sampleNetwork(now)
	id := now.UTC().Format("20060102T15")
	err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		var samples []HistorySample
		value, err := store.Get(ctx, "metric_bucket", id)
		newBucket := core.ErrorStatus(err) == 404
		if err == nil {
			if err = json.Unmarshal(value, &samples); err != nil {
				return err
			}
		} else if core.ErrorStatus(err) != 404 {
			return err
		}
		samples = append(samples, sample)
		if len(samples) > 3600 {
			samples = samples[len(samples)-3600:]
		}
		raw, _ := json.Marshal(samples)
		for len(raw) > 900<<10 && len(samples) > 1 {
			out := make([]HistorySample, 0, len(samples)/2+1)
			for i := 0; i < len(samples); i += 2 {
				out = append(out, samples[i])
			}
			if !out[len(out)-1].Timestamp.Equal(samples[len(samples)-1].Timestamp) {
				out = append(out, samples[len(samples)-1])
			}
			samples = out
			raw, _ = json.Marshal(samples)
		}
		if err = store.Save(ctx, "metric_bucket", id, raw); err != nil {
			return err
		}
		if !newBucket {
			return nil
		}
		ids, err := historyIndex(ctx, store)
		if err != nil {
			return err
		}
		for _, existing := range ids {
			if existing == id {
				return nil
			}
		}
		ids = append(ids, id)
		sort.Strings(ids)
		index, _ := json.Marshal(ids)
		return store.Save(ctx, "metric_index", "hours", index)
	})
	if err != nil {
		return err
	}
	p.mu.Lock()
	prune := now.Sub(p.historyPrunedAt) >= time.Hour
	if prune {
		p.historyPrunedAt = now
	}
	p.mu.Unlock()
	if prune {
		_, err = p.pruneHistory(ctx, now.AddDate(0, 0, -p.options.HistoryRetentionDays))
	}
	return err
}
func (p *Provider) history(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		Start string `json:"start"`
		End   string `json:"end"`
		Limit int    `json:"limit"`
	}
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	end := time.Now().UTC()
	start := end.Add(-24 * time.Hour)
	var err error
	if in.Start != "" {
		start, err = time.Parse(time.RFC3339, in.Start)
		if err != nil {
			return core.ActionResult{}, invalid("Invalid history start")
		}
	}
	if in.End != "" {
		end, err = time.Parse(time.RFC3339, in.End)
		if err != nil {
			return core.ActionResult{}, invalid("Invalid history end")
		}
	}
	if end.Before(start) || end.Sub(start) > 31*24*time.Hour {
		return core.ActionResult{}, invalid("History range must be at most 31 days")
	}
	if in.Limit == 0 {
		in.Limit = 500
	}
	if in.Limit < 1 || in.Limit > 1000 {
		return core.ActionResult{}, invalid("History limit is outside 1..1000")
	}
	keys := []string{}
	for stamp := start.UTC().Truncate(time.Hour); !stamp.After(end); stamp = stamp.Add(time.Hour) {
		keys = append(keys, stamp.Format("20060102T15"))
	}
	sort.Strings(keys)
	samples := []HistorySample{}
	lastBin := -1
	window := end.Sub(start)
	for _, key := range keys {
		var bucket []HistorySample
		value, getErr := p.deps.State.Get(ctx, "metric_bucket", key)
		if core.ErrorStatus(getErr) == 404 {
			continue
		}
		if getErr != nil {
			return core.ActionResult{}, getErr
		}
		if err = json.Unmarshal(value, &bucket); err != nil {
			return core.ActionResult{}, err
		}
		sort.Slice(bucket, func(i, j int) bool { return bucket[i].Timestamp.Before(bucket[j].Timestamp) })
		for _, sample := range bucket {
			if !sample.Timestamp.Before(start) && !sample.Timestamp.After(end) {
				bin := 0
				if window > 0 {
					bin = min(int(float64(sample.Timestamp.Sub(start))/float64(window)*float64(in.Limit)), in.Limit-1)
				}
				if bin > lastBin && len(samples) < in.Limit {
					samples = append(samples, sample)
					lastBin = bin
				}
			}
		}
	}
	return core.ActionResult{Data: map[string]any{"samples": samples, "source": "persisted_host_sampler", "sample_interval_seconds": int(p.options.SampleInterval / time.Second)}}, nil
}
func (p *Provider) pruneHistory(ctx context.Context, before time.Time) (int, error) {
	count := 0
	err := p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		ids, err := historyIndex(ctx, store)
		if err != nil {
			return err
		}
		keep := []string{}
		for _, id := range ids {
			timestamp, err := time.Parse("20060102T15", id)
			if err == nil && !timestamp.Add(time.Hour).After(before) {
				if err = store.Delete(ctx, "metric_bucket", id); err != nil {
					return err
				}
				count++
			} else {
				keep = append(keep, id)
			}
		}
		value, _ := json.Marshal(keep)
		return store.Save(ctx, "metric_index", "hours", value)
	})
	return count, err
}

func historyIndex(ctx context.Context, store core.MetadataStore) ([]string, error) {
	value, err := store.Get(ctx, "metric_index", "hours")
	if core.ErrorStatus(err) == 404 {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	err = json.Unmarshal(value, &ids)
	return ids, err
}
