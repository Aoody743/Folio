package systemops

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

// Alerts are opt-in host rules. Persisted cooldown reservations prevent a
// restart from immediately duplicating an uncertain notification submission.
type AlertRule struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Metric          string     `json:"metric"`
	Threshold       int        `json:"threshold"`
	DurationSeconds int        `json:"duration_seconds"`
	CooldownSeconds int        `json:"cooldown_seconds"`
	ChannelID       string     `json:"channel_id,omitempty"`
	Enabled         bool       `json:"enabled"`
	CreatedBy       core.Actor `json:"created_by"`
	Revision        int64      `json:"revision"`
}
type AlertEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Phase     string    `json:"phase"`
	Value     *float64  `json:"value"`
}
type AlertState struct {
	Phase             string       `json:"phase"`
	Since             *time.Time   `json:"since,omitempty"`
	LastSample        time.Time    `json:"last_sample"`
	Value             *float64     `json:"value"`
	LastAttempt       *time.Time   `json:"last_attempt,omitempty"`
	NotificationState string       `json:"notification_state,omitempty"`
	TaskID            string       `json:"task_id,omitempty"`
	ErrorCode         string       `json:"error_code,omitempty"`
	Events            []AlertEvent `json:"events"`
}
type AlertView struct {
	AlertRule
	State AlertState `json:"state"`
}
type alertInput struct {
	ID               string `json:"id,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
	Name             string `json:"name"`
	Metric           string `json:"metric"`
	Threshold        int    `json:"threshold"`
	DurationSeconds  int    `json:"duration_seconds"`
	CooldownSeconds  int    `json:"cooldown_seconds"`
	ChannelID        string `json:"channel_id,omitempty"`
	Enabled          bool   `json:"enabled"`
}

func (p *Provider) alertBindings() []core.Binding {
	fields := map[string]any{"id": text(96), "expected_revision": integer(1, 1<<30), "name": text(160), "metric": enum("cpu", "memory", "disk"), "threshold": integer(1, 100), "duration_seconds": integer(60, 3600), "cooldown_seconds": integer(60, 86400), "channel_id": text(96), "enabled": boolean()}
	return []core.Binding{
		p.bind("monitor_alerts_list", "Read opt-in host alert rules, current state and recent transitions", "system:read", true, object(map[string]any{}), hostResources, p.listAlerts),
		p.bind("monitor_alerts_save", "Create or revision-check a host alert rule; notifications recheck the owner's authority", "system:write", false, object(fields, "name", "metric", "threshold", "duration_seconds", "cooldown_seconds", "enabled"), hostResources, p.saveAlert),
		p.bind("monitor_alerts_delete", "Delete the selected alert rule and its local transition history", "system:write", false, object(map[string]any{"id": text(96), "expected_revision": integer(1, 1<<30)}, "id", "expected_revision"), hostResources, p.deleteAlert),
	}
}
func (p *Provider) loadAlert(ctx context.Context, id string) (AlertRule, error) {
	raw, rev, err := p.deps.State.GetVersioned(ctx, "alert_rule", id)
	if err != nil {
		return AlertRule{}, err
	}
	var rule AlertRule
	if err = json.Unmarshal(raw, &rule); err != nil {
		return rule, err
	}
	rule.Revision = rev
	return rule, nil
}
func (p *Provider) alertState(ctx context.Context, id string) (AlertState, error) {
	raw, err := p.deps.State.Get(ctx, "alert_state", id)
	if core.ErrorStatus(err) == 404 {
		return AlertState{Phase: "unknown", Events: []AlertEvent{}}, nil
	}
	if err != nil {
		return AlertState{}, err
	}
	var state AlertState
	err = json.Unmarshal(raw, &state)
	return state, err
}
func (p *Provider) listAlerts(ctx context.Context, _ core.Actor, _ json.RawMessage) (core.ActionResult, error) {
	records, err := p.deps.State.List(ctx, "alert_rule")
	if err != nil {
		return core.ActionResult{}, err
	}
	out := []AlertView{}
	for id := range records {
		rule, err := p.loadAlert(ctx, id)
		if err != nil {
			return core.ActionResult{}, err
		}
		state, err := p.alertState(ctx, id)
		if err != nil {
			return core.ActionResult{}, err
		}
		out = append(out, AlertView{rule, state})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return core.ActionResult{Data: out}, nil
}
func (p *Provider) saveAlert(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in alertInput
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	if strings.TrimSpace(in.Name) == "" || strings.ContainsAny(in.Name, "\r\n") || (in.Metric != "cpu" && in.Metric != "memory" && in.Metric != "disk") || in.Threshold < 1 || in.Threshold > 100 || in.DurationSeconds < 60 || in.DurationSeconds > 3600 || in.CooldownSeconds < 60 || in.CooldownSeconds > 86400 {
		return core.ActionResult{}, invalid("Invalid alert thresholds or timing")
	}
	if in.Enabled && !p.deps.Authorize(a, "system:read", "host:local") {
		return core.ActionResult{}, denied()
	}
	if in.ChannelID != "" {
		if !p.deps.Authorize(a, "notifications:write", "notification:"+in.ChannelID) {
			return core.ActionResult{}, denied()
		}
		if _, err := p.loadChannel(ctx, in.ChannelID); err != nil {
			return core.ActionResult{}, err
		}
	}
	p.alertMu.Lock()
	defer p.alertMu.Unlock()
	if in.ID == "" {
		if in.ExpectedRevision != 0 {
			return core.ActionResult{}, invalid("New alerts cannot have an expected revision")
		}
		var err error
		in.ID, err = newID("alert_")
		if err != nil {
			return core.ActionResult{}, err
		}
	} else if _, err := p.loadAlert(ctx, in.ID); err != nil {
		return core.ActionResult{}, err
	}
	rule := AlertRule{ID: in.ID, Name: strings.TrimSpace(in.Name), Metric: in.Metric, Threshold: in.Threshold, DurationSeconds: in.DurationSeconds, CooldownSeconds: in.CooldownSeconds, ChannelID: in.ChannelID, Enabled: in.Enabled, CreatedBy: a.Public()}
	err := p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		if in.ExpectedRevision == 0 {
			records, err := store.List(ctx, "alert_rule")
			if err != nil {
				return err
			}
			if len(records) >= 100 {
				return invalid("At most 100 alert rules are supported")
			}
		}
		data, _ := json.Marshal(rule)
		rev, err := store.CompareAndSwap(ctx, "alert_rule", rule.ID, in.ExpectedRevision, data)
		if err != nil {
			return err
		}
		rule.Revision = rev
		// Editing a policy starts a fresh observation window and preserves no
		// previous task as evidence for the new configuration.
		return store.Delete(ctx, "alert_state", rule.ID)
	})
	return core.ActionResult{Data: rule}, err
}
func (p *Provider) deleteAlert(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ID               string `json:"id"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	p.alertMu.Lock()
	defer p.alertMu.Unlock()
	err := p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		_, rev, err := store.GetVersioned(ctx, "alert_rule", in.ID)
		if err != nil {
			return err
		}
		if rev != in.ExpectedRevision {
			return conflict("Alert rule revision changed")
		}
		if err = store.Delete(ctx, "alert_rule", in.ID); err != nil {
			return err
		}
		return store.Delete(ctx, "alert_state", in.ID)
	})
	return core.ActionResult{Data: map[string]any{"deleted": err == nil}}, err
}
func alertValue(rule AlertRule, sample HistorySample) *float64 {
	value := sample.CPUPercent
	switch rule.Metric {
	case "memory":
		value = usagePercent(sample.Memory)
	case "disk":
		value = usagePercent(sample.Disk)
	}
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 || *value > 100 {
		return nil
	}
	v := *value
	return &v
}
func usagePercent(usage *core.Usage) *float64 {
	if usage == nil || usage.TotalBytes <= 0 || usage.UsedBytes < 0 || usage.UsedBytes > usage.TotalBytes {
		return nil
	}
	v := float64(usage.UsedBytes) / float64(usage.TotalBytes) * 100
	return &v
}
func observeAlert(rule AlertRule, state AlertState, sample HistorySample, interval time.Duration) AlertState {
	if !state.LastSample.IsZero() && !sample.Timestamp.After(state.LastSample) {
		return state
	}
	oldPhase := state.Phase
	gap := !state.LastSample.IsZero() && sample.Timestamp.Sub(state.LastSample) > interval+interval/4
	if gap {
		state.Phase = "unknown"
		state.Since = nil
	}
	state.LastSample = sample.Timestamp
	state.Value = alertValue(rule, sample)
	if state.Value == nil {
		state.Phase = "unknown"
		state.Since = nil
	} else if *state.Value >= float64(rule.Threshold) {
		if gap || state.Since == nil || state.Phase == "unknown" {
			at := sample.Timestamp
			state.Since = &at
		}
		state.Phase = "pending"
		if sample.Timestamp.Sub(*state.Since) >= time.Duration(rule.DurationSeconds)*time.Second {
			state.Phase = "firing"
		}
	} else if state.Phase != "firing" || *state.Value <= float64(max(0, rule.Threshold-5)) {
		state.Phase = "normal"
		state.Since = nil
	}
	if state.Phase != oldPhase {
		state.Events = append(state.Events, AlertEvent{sample.Timestamp, state.Phase, state.Value})
		if len(state.Events) > 30 {
			state.Events = state.Events[len(state.Events)-30:]
		}
	}
	return state
}
func (p *Provider) evaluateAlerts(ctx context.Context, sample HistorySample) error {
	p.alertMu.Lock()
	defer p.alertMu.Unlock()
	records, err := p.deps.State.List(ctx, "alert_rule")
	if err != nil {
		return err
	}
	for id := range records {
		rule, err := p.loadAlert(ctx, id)
		if err != nil {
			return err
		}
		if !rule.Enabled {
			continue
		}
		state, err := p.alertState(ctx, id)
		if err != nil {
			return err
		}
		if !state.LastSample.IsZero() && !sample.Timestamp.After(state.LastSample) {
			continue
		}
		actor, authErr := p.deps.ResolveActor(ctx, rule.CreatedBy)
		if authErr != nil || !p.deps.Authorize(actor, "system:read", "host:local") || !p.deps.Authorize(actor, "system:write", "host:local") {
			state.Phase = "authorization_lost"
			state.Since = nil
			state.Value = nil
			state.LastSample = sample.Timestamp
			state.ErrorCode = "identity_or_permission_revoked"
		} else {
			state = observeAlert(rule, state, sample, p.options.SampleInterval)
			state.ErrorCode = ""
			eligible := state.Phase == "firing" && rule.ChannelID != "" && (state.LastAttempt == nil || sample.Timestamp.Sub(*state.LastAttempt) >= time.Duration(rule.CooldownSeconds)*time.Second)
			if eligible {
				at := sample.Timestamp
				state.LastAttempt = &at
				state.NotificationState = "submission_unknown"
				state.TaskID = ""
				// Persist the reservation before any external dispatch; unknown outcomes
				// wait for the cooldown instead of being resent immediately on restart.
				data, _ := json.Marshal(state)
				if err = p.deps.State.Save(ctx, "alert_state", id, data); err != nil {
					return err
				}
				payload, _ := json.Marshal(map[string]any{"channel_id": rule.ChannelID, "subject": "Folio alert: " + rule.Name, "content": fmt.Sprintf("%s: %.1f%% (threshold %d%%, sustained %ds). Sample: %s. Rule: %s", rule.Metric, *state.Value, rule.Threshold, rule.DurationSeconds, sample.Timestamp.Format(time.RFC3339), rule.ID)})
				result, sendErr := p.deps.Dispatch(ctx, actor, "notifications_send", payload)
				if sendErr != nil {
					state.NotificationState = "failed"
					state.ErrorCode = core.ErrorCode(sendErr)
				} else {
					state.TaskID = scheduleTaskID(result)
					state.NotificationState = "submitted"
				}
			}
		}
		data, _ := json.Marshal(state)
		if err = p.deps.State.Save(ctx, "alert_state", id, data); err != nil {
			return err
		}
	}
	return nil
}
