package systemops

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func actualProvider(t *testing.T, roots *x509.CertPool) (*core.Service, *Provider, core.Actor) {
	t.Helper()
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { service.Close() })
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "fixture-test-password-12345")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := New(service.Runtime("systemops"), Options{TLSRoots: roots, AllowedScheduleActions: []string{"system_get_status", "notifications_send"}, SampleInterval: time.Minute, Status: func(context.Context) (core.SystemStatus, error) { return service.StatusSnapshot(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { provider.Close() })
	if err = service.RegisterExecutor(JobOperationNames, provider); err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterProvider(provider); err != nil {
		t.Fatal(err)
	}
	return service, provider, actor
}
func action(t *testing.T, service *core.Service, actor core.Actor, name string, in any) any {
	t.Helper()
	raw, _ := json.Marshal(in)
	result, err := service.Execute(context.Background(), actor, name, raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}
func TestSchedulesPersistConflictAndRefreshAuthority(t *testing.T) {
	service, provider, admin := actualProvider(t, nil)
	created, err := service.CreateToken(context.Background(), admin, core.TokenRequest{Name: "scheduler", Kind: "mcp", Scopes: []string{"schedules:read", "schedules:write", "system:read"}, Resources: []string{"*"}, ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	result := action(t, service, actor, "schedules_create", map[string]any{"name": "real-status", "mode": "interval", "timezone": "UTC", "interval_seconds": 15, "action": "system_get_status", "input": map[string]any{}, "enabled": true}).(Schedule)
	if result.Revision != 1 || !result.NextRunAt.After(time.Now()) {
		t.Fatal(result)
	}
	read := action(t, service, actor, "schedules_list", map[string]any{}).([]Schedule)
	if len(read) != 1 || read[0].CreatedBy.ID != actor.ID {
		t.Fatal(read)
	}
	run := action(t, service, actor, "schedules_run", map[string]any{"id": result.ID}).(ScheduleRun)
	if run.Status != "completed" {
		t.Fatal(run)
	}
	raw, _ := json.Marshal(map[string]any{"id": result.ID, "expected_revision": 9, "name": "changed", "mode": "interval", "timezone": "UTC", "interval_seconds": 15, "action": "system_get_status", "input": map[string]any{}, "enabled": true})
	if _, err = service.Execute(context.Background(), actor, "schedules_update", raw); core.ErrorStatus(err) != 409 {
		t.Fatal("stale revision accepted", err)
	}
	if err = service.RevokeToken(context.Background(), admin, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.runSchedule(context.Background(), result, time.Now(), false); err == nil {
		t.Fatal("revoked schedule identity executed")
	}
	persisted, err := provider.loadSchedule(context.Background(), result.ID)
	if err != nil || persisted.Enabled || persisted.LastError == "" {
		t.Fatal("revoked schedule not stopped", persisted, err)
	}
}
func TestCronTimezoneAndCalendarSemantics(t *testing.T) {
	location, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		t.Fatal(err)
	}
	cron, err := parseCron("*/15 9-17 * * 1-5")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 10, 12, 0, 59, 0, 0, time.UTC)
	next, err := cron.next(after, location)
	if err != nil || next.In(location).Hour() != 9 || next.In(location).Minute() != 0 {
		t.Fatal(next, err)
	}
	for _, value := range []string{"* * * *", "61 * * * *", "*/0 * * * *", "* -1 * * *", "1 1 1 13 1"} {
		if _, err = parseCron(value); err == nil {
			t.Fatal("invalid cron accepted", value)
		}
	}
	leap, err := parseCron("0 0 29 2 *")
	if err != nil {
		t.Fatal(err)
	}
	next, err = leap.next(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.UTC)
	if err != nil || next.Year() != 2028 || next.Day() != 29 {
		t.Fatal(next, err)
	}
}
func TestHistoryRecordsActualSnapshotAndBoundedQuery(t *testing.T) {
	service, provider, actor := actualProvider(t, nil)
	now := time.Now().UTC()
	if err := provider.sample(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	result := action(t, service, actor, "monitor_history", map[string]any{"limit": 10}).(map[string]any)
	samples := result["samples"].([]HistorySample)
	if len(samples) == 0 || samples[len(samples)-1].Memory == nil {
		t.Fatal(result)
	}
	raw, _ := json.Marshal(map[string]any{"start": "2020-01-01T00:00:00Z", "end": "2026-01-01T00:00:00Z"})
	if _, err := service.Execute(context.Background(), actor, "monitor_history", raw); core.ErrorStatus(err) != 400 {
		t.Fatal("unbounded history accepted", err)
	}
}

func smtpTLSFixture(t *testing.T) (string, int, *x509.CertPool, <-chan string) {
	t.Helper()
	web := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	config := web.TLS.Clone()
	pool := x509.NewCertPool()
	pool.AddCert(web.Certificate())
	web.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	messages := make(chan string, 2)
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
				reader := bufio.NewReader(connection)
				writer := bufio.NewWriter(connection)
				send := func(text string) { _, _ = writer.WriteString(text + "\r\n"); _ = writer.Flush() }
				send("220 fixture ESMTP")
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					line = strings.TrimSpace(line)
					switch {
					case strings.HasPrefix(line, "EHLO"):
						send("250-fixture\r\n250 AUTH PLAIN")
					case strings.HasPrefix(line, "AUTH"):
						send("235 authenticated")
					case strings.HasPrefix(line, "MAIL"), strings.HasPrefix(line, "RCPT"):
						send("250 accepted")
					case line == "DATA":
						send("354 send message")
						var body strings.Builder
						for {
							part, err := reader.ReadString('\n')
							if err != nil {
								return
							}
							if strings.TrimSpace(part) == "." {
								break
							}
							body.WriteString(part)
						}
						messages <- body.String()
						send("250 queued")
					case line == "QUIT":
						send("221 closing")
						return
					default:
						send("500 unsupported")
					}
				}
			}()
		}
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	return host, number, pool, messages
}
func TestNotificationsUseEncryptedReferencesRealTLSAndDurableTasks(t *testing.T) {
	host, port, pool, messages := smtpTLSFixture(t)
	service, provider, actor := actualProvider(t, pool)
	channel := action(t, service, actor, "notifications_channels_save", map[string]any{"name": "tls-fixture", "kind": "smtp", "smtp": map[string]any{"host": host, "port": port, "tls_mode": "implicit", "username": "fixture-user", "from": "sender@example.test", "recipients": []string{"receiver@example.test"}}, "secret": "fixture-password-do-not-log"}).(Channel)
	list := action(t, service, actor, "notifications_channels_list", map[string]any{}).([]Channel)
	if len(list) != 1 || list[0].SecretRef != "" {
		t.Fatal("secret exposed", list)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), "fixture-password") {
		t.Fatal("secret in channel response")
	}
	stored, err := provider.loadChannel(context.Background(), channel.ID)
	if err != nil || stored.SecretRef == "" {
		t.Fatal(stored, err)
	}
	task := action(t, service, actor, "notifications_send", map[string]any{"channel_id": channel.ID, "subject": "真实TLS通知", "content": "hello delivery"}).(map[string]any)
	id, _ := task["id"].(string)
	if id == "" {
		t.Fatal(task)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		result := action(t, service, actor, "tasks_get", map[string]any{"id": id}).(map[string]any)
		if result["status"] == "succeeded" {
			break
		}
		if result["status"] == "failed" {
			t.Fatal(result)
		}
		time.Sleep(30 * time.Millisecond)
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "hello delivery") || !strings.Contains(message, "Content-Type: text/plain") {
			t.Fatal(message)
		}
	case <-time.After(time.Second):
		t.Fatal("actual SMTP protocol did not accept message")
	}
	deliveries := action(t, service, actor, "notifications_deliveries", map[string]any{"channel_id": channel.ID}).([]Delivery)
	if len(deliveries) != 1 || deliveries[0].Status != "accepted" {
		t.Fatal(deliveries)
	}
	if err := provider.smtpSend(context.Background(), SMTPConfig{Host: host, Port: port, TLSMode: "implicit", From: "a@example.test", Recipients: []string{"b@example.test"}}, "", "test", "body"); err != nil {
		t.Fatal(err)
	}
	other := &Provider{options: Options{}}
	if err := other.smtpSend(context.Background(), SMTPConfig{Host: host, Port: port, TLSMode: "implicit", From: "a@example.test", Recipients: []string{"b@example.test"}}, "", "test", "body"); err == nil {
		t.Fatal("untrusted TLS certificate accepted")
	}
}
func TestAllSystemBindingsAreUniqueAndPublished(t *testing.T) {
	service, provider, actor := actualProvider(t, nil)
	names := []string{}
	seen := map[string]bool{}
	for _, binding := range provider.Bindings() {
		if seen[binding.Action.Name] {
			t.Fatal("duplicate binding")
		}
		seen[binding.Action.Name] = true
		names = append(names, binding.Action.Name)
	}
	sort.Strings(names)
	actions := service.Actions(actor)
	for _, name := range names {
		found := false
		for _, entry := range actions {
			if entry.Name == name {
				found = true
			}
		}
		if !found {
			t.Fatal("not discoverable", name)
		}
	}
	raw := json.RawMessage(`{"change":{"kind":"service_state","service_id":"test","service_state":"start","command":"rm"}}`)
	if _, err := service.Execute(context.Background(), actor, "system_change_plan", raw); core.ErrorStatus(err) != 400 {
		t.Fatal("unknown nested input accepted", err)
	}
}

func TestManualScheduleCannotBorrowAuthorPrivileges(t *testing.T) {
	service, _, admin := actualProvider(t, nil)
	schedule := action(t, service, admin, "schedules_create", map[string]any{"name": "privileged-status", "mode": "interval", "timezone": "UTC", "interval_seconds": 15, "action": "system_get_status", "input": map[string]any{}, "enabled": false}).(Schedule)
	created, err := service.CreateToken(context.Background(), admin, core.TokenRequest{Name: "manual-trigger-only", Kind: "mcp", Scopes: []string{"schedules:write"}, Resources: []string{"schedule:" + schedule.ID}, ExpiresInSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	caller, err := service.AuthenticateToken(context.Background(), created.Token)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"id": schedule.ID})
	if _, err = service.Execute(context.Background(), caller, "schedules_run", raw); core.ErrorStatus(err) != 403 {
		t.Fatalf("author privileges borrowed: %v", err)
	}
}

func TestScheduleRestartMarksUncertainRunInsteadOfReplaying(t *testing.T) {
	service, provider, admin := actualProvider(t, nil)
	schedule := action(t, service, admin, "schedules_create", map[string]any{"name": "restart-fixture", "mode": "interval", "timezone": "UTC", "interval_seconds": 15, "action": "system_get_status", "input": map[string]any{}, "enabled": false}).(Schedule)
	run := ScheduleRun{ID: "run_fixture", ScheduleID: schedule.ID, StartedAt: time.Now().Add(-time.Minute), Status: "running"}
	if err := provider.deps.State.Transaction(context.Background(), func(store core.MetadataStore) error { return saveScheduleRun(context.Background(), store, run) }); err != nil {
		t.Fatal(err)
	}
	if err := provider.recoverScheduleRuns(context.Background()); err != nil {
		t.Fatal(err)
	}
	runs := action(t, service, admin, "schedules_history", map[string]any{"id": schedule.ID}).([]ScheduleRun)
	if len(runs) != 1 || runs[0].Status != "interrupted" || runs[0].ErrorCode != "server_restarted" {
		t.Fatal(runs)
	}
}

func TestHistoryIndexPrunesWithoutLoadingAllBuckets(t *testing.T) {
	_, provider, _ := actualProvider(t, nil)
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -60)
	if err := provider.sample(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if err := provider.sample(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	count, err := provider.pruneHistory(context.Background(), now.AddDate(0, 0, -30))
	if err != nil || count != 0 {
		t.Fatal(count, err)
	}
	ids, err := historyIndex(context.Background(), provider.deps.State)
	if err != nil || len(ids) != 1 || ids[0] != now.Format("20060102T15") {
		t.Fatal(ids, err)
	}
	if _, err = provider.deps.State.Get(context.Background(), "metric_bucket", old.Format("20060102T15")); core.ErrorStatus(err) != 404 {
		t.Fatal("old bucket retained", err)
	}
}
