package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

type leasedCopyExecutor struct {
	runner *agent.Runner
	marker string
}

func (e *leasedCopyExecutor) Validate(r agent.Request) error { return e.runner.Validate(r) }
func (e *leasedCopyExecutor) Run(ctx context.Context, r agent.Request) (agent.Result, error) {
	if err := os.WriteFile(e.marker, []byte(r.ActorID), 0600); err != nil {
		return agent.Result{}, err
	}
	return e.runner.Run(ctx, r)
}
func TestRegisteredRunningTaskStopsAfterTokenRevocation(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("requires real Unix zero device")
	}
	s, admin := testService(t)
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "running")
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{{Name: "fixture_continuous_copy", Program: "/bin/cat", BuildArgs: func(json.RawMessage) ([]string, error) { return []string{"/dev/zero"}, nil }, Timeout: time.Minute}}, MaxOutputBytes: 1024, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterExecutor([]string{"fixture_continuous_copy"}, &leasedCopyExecutor{runner: runner, marker: marker}); err != nil {
		t.Fatal(err)
	}
	deps := s.Runtime("leasefixture")
	binding := fixtureBinding("fixture_copy", "fixture:execute", "file:leased", func(ctx context.Context, a Actor, raw json.RawMessage) (ActionResult, error) {
		job, err := deps.Tasks.Submit(ctx, a, "fixture_continuous_copy", raw, []string{"file:leased"})
		id, _ := job["id"].(string)
		return ActionResult{Data: job, TaskID: id, Resources: []string{"file:leased"}}, err
	})
	binding.Action.Task = true
	if err = s.RegisterProvider(Bindings{binding}); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(ctx, admin, TokenRequest{Name: "revocable live copy", Scopes: []string{"fixture:execute", "tasks:read"}, Resources: []string{"file:leased"}})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.AuthenticateToken(ctx, token.Token)
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.Execute(ctx, actor, "fixture_copy", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	id := data.(map[string]any)["id"].(string)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if body, err := os.ReadFile(marker); err == nil {
			if string(body) != actor.ID {
				t.Fatal("execution actor was not issued by the job manager")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real copy process did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	if err = s.RevokeToken(ctx, admin, token.ID); err != nil {
		t.Fatal(err)
	}
	for time.Since(started) < 1500*time.Millisecond {
		job, err := s.jobs.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == "failed" || job.Status == "canceled" {
			if job.Result != nil && len(job.Result.Stdout) > 1024 {
				t.Fatal("revoked process produced unbounded output")
			}
			return
		}
		if job.Status == "succeeded" {
			t.Fatal("infinite copy completed despite revocation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("running task continued beyond its one-second revocation lease")
}
