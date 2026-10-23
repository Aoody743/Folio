package systemops

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func numberPointer(v float64) *float64 { return &v }
func TestAlertObservationsRequireContinuousRealData(t *testing.T) {
	now := time.Now().UTC()
	rule := AlertRule{Metric: "cpu", Threshold: 90, DurationSeconds: 120}
	state := AlertState{Phase: "unknown"}
	sample := func(offset int, value *float64) {
		state = observeAlert(rule, state, HistorySample{Timestamp: now.Add(time.Duration(offset) * time.Second), CPUPercent: value}, time.Minute)
	}
	sample(0, numberPointer(95))
	sample(60, numberPointer(96))
	if state.Phase != "pending" {
		t.Fatal(state)
	}
	sample(120, numberPointer(97))
	if state.Phase != "firing" {
		t.Fatal(state)
	}
	sample(180, numberPointer(87))
	if state.Phase != "firing" {
		t.Fatal("hysteresis was lost", state)
	}
	sample(240, numberPointer(85))
	if state.Phase != "normal" || state.Since != nil {
		t.Fatal(state)
	}
	sample(300, numberPointer(95))
	sample(420, numberPointer(95))
	if state.Phase != "pending" || !state.Since.Equal(now.Add(420*time.Second)) {
		t.Fatal("one missing sample counted toward duration", state)
	}
	sample(480, nil)
	sample(540, numberPointer(95))
	sample(600, numberPointer(95))
	if state.Phase != "pending" {
		t.Fatal("unknown data counted toward duration", state)
	}
	sample(900, numberPointer(95))
	if state.Phase != "pending" || !state.Since.Equal(now.Add(900*time.Second)) {
		t.Fatal("gap counted as sustained data", state)
	}
	sample(960, numberPointer(math.NaN()))
	if state.Phase != "unknown" || state.Value != nil {
		t.Fatal("invalid metric became healthy", state)
	}
	rule.Metric = "memory"
	if alertValue(rule, HistorySample{Memory: &core.Usage{TotalBytes: 100, UsedBytes: 120}}) != nil {
		t.Fatal("invalid capacity accepted")
	}
	rule.Metric = "cpu"
	rule.Threshold = 1
	state = AlertState{Phase: "firing", Since: &now}
	sample(1020, numberPointer(0))
	if state.Phase != "normal" {
		t.Fatal("zero must clear low-threshold alerts", state)
	}
}
func TestAlertPersistenceCooldownAuthorityAndRevision(t *testing.T) {
	service, p, admin := actualProvider(t, nil)
	p.Close()
	ctx := context.Background()
	channel := action(t, service, admin, "notifications_channels_save", map[string]any{"name": "No network fixture", "kind": "telegram", "chat_id": "1234", "secret": "fake-secret-not-sent"}).(Channel)
	token, err := service.CreateToken(ctx, admin, core.TokenRequest{Name: "alert-owner", Kind: "mcp", Scopes: []string{"system:read", "system:write", "notifications:write"}, Resources: []string{"*"}, ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.AuthenticateToken(ctx, token.Token)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"name": "CPU sustained", "metric": "cpu", "threshold": 90, "duration_seconds": 60, "cooldown_seconds": 600, "channel_id": channel.ID, "enabled": true}
	rule := action(t, service, actor, "monitor_alerts_save", values).(AlertRule)
	calls := 0
	p.deps.Dispatch = func(context.Context, core.Actor, string, json.RawMessage) (any, error) {
		calls++
		return map[string]any{"task_id": "notification-fixture"}, nil
	}
	now := time.Now().UTC().Add(time.Hour)
	eval := func(offset int) {
		t.Helper()
		if err := p.evaluateAlerts(ctx, HistorySample{Timestamp: now.Add(time.Duration(offset) * time.Second), CPUPercent: numberPointer(95)}); err != nil {
			t.Fatal(err)
		}
	}
	eval(0)
	eval(60)
	eval(120)
	if calls != 1 {
		t.Fatal("cooldown failed", calls)
	}
	state, err := p.alertState(ctx, rule.ID)
	if err != nil || state.Phase != "firing" || state.NotificationState != "submitted" || state.TaskID != "notification-fixture" {
		t.Fatal(state, err)
	}
	// A fresh provider over the same namespace must honor the persisted attempt.
	p2, err := New(p.deps, Options{SampleInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	p2.Close()
	if err = p2.evaluateAlerts(ctx, HistorySample{Timestamp: now.Add(180 * time.Second), CPUPercent: numberPointer(95)}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("restart resent notification")
	}
	stale := map[string]any{}
	for k, v := range values {
		stale[k] = v
	}
	stale["id"] = rule.ID
	stale["expected_revision"] = 9
	raw, _ := json.Marshal(stale)
	if _, err = service.Execute(ctx, actor, "monitor_alerts_save", raw); core.ErrorStatus(err) != 412 {
		t.Fatal("stale rule update accepted", err)
	}
	if err = service.RevokeToken(ctx, admin, token.ID); err != nil {
		t.Fatal(err)
	}
	eval(900)
	if calls != 1 {
		t.Fatal("revoked rule owner sent a notification")
	}
	state, err = p.alertState(ctx, rule.ID)
	if err != nil || state.Phase != "authorization_lost" || state.ErrorCode == "" {
		t.Fatal(state, err)
	}
	listed := action(t, service, admin, "monitor_alerts_list", map[string]any{}).([]AlertView)
	if len(listed) != 1 || listed[0].Revision != rule.Revision {
		t.Fatal(listed)
	}
	readonly, err := service.CreateToken(ctx, admin, core.TokenRequest{Name: "read", Kind: "mcp", Scopes: []string{"system:read"}, Resources: []string{"host:other"}, ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := service.AuthenticateToken(ctx, readonly.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.Execute(ctx, outsider, "monitor_alerts_list", json.RawMessage(`{}`)); core.ErrorStatus(err) != 403 {
		t.Fatal("host-scoped rules leaked", err)
	}
}
