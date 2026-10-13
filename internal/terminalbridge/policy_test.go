package terminalbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/Aoody743/PowerLinux/internal/terminal"
)

// Metadata-only policy probe. Real PTY and transport behavior is separately
// exercised above; this does not create or pretend to create a root process.
type rootProbe struct {
	owner   string
	effects atomic.Int32
}

func (probe *rootProbe) Validate(agent.Request) error { return nil }
func (probe *rootProbe) Run(_ context.Context, r agent.Request) (agent.Result, error) {
	var payload terminal.AgentPayload
	if json.Unmarshal(r.Input, &payload) != nil {
		return agent.Result{}, invalid()
	}
	info := terminal.SessionInfo{ID: "root-test", TargetID: "local", ActorID: probe.owner, Root: true}
	if r.Operation != "terminal_info" {
		if !payload.RootAuthorized || !payload.AuthorizedShared && r.ActorID != probe.owner {
			return agent.Result{}, denied()
		}
		probe.effects.Add(1)
	}
	return agent.Result{Data: encoded(info)}, nil
}
func TestRootHostPermissionRecheckedForEverySessionOperation(t *testing.T) {
	f := setup(t)
	probe := &rootProbe{owner: f.admin.ID}
	f.bridge.options.Agent = probe
	ctx := context.Background()
	cases := []map[string]any{{"name": "terminal_read"}, {"name": "terminal_input", "data": base64.StdEncoding.EncodeToString([]byte("x"))}, {"name": "terminal_resize", "rows": 24, "cols": 80}, {"name": "terminal_renew"}, {"name": "terminal_close"}}
	for _, permissions := range []struct{ scopes, resources []string }{{[]string{"terminal:connect", "terminal:share"}, []string{Resource("local"), "host:local"}}, {[]string{"terminal:connect", "terminal:share", "terminal:root"}, []string{Resource("local")}}} {
		actor, _ := token(t, f, permissions.scopes, permissions.resources)
		for _, operation := range cases {
			name := operation["name"].(string)
			in := map[string]any{"id": "root-test"}
			for key, value := range operation {
				if key != "name" {
					in[key] = value
				}
			}
			if _, err := f.service.Execute(ctx, actor, name, encoded(in)); core.ErrorCode(err) != "forbidden" {
				t.Fatalf("%s skipped root/host gate: %v", name, err)
			}
		}
	}
	if probe.effects.Load() != 0 {
		t.Fatal("root-side operation ran before all permissions passed")
	}
	allowed, _ := token(t, f, []string{"terminal:connect", "terminal:share", "terminal:root"}, []string{Resource("local"), "host:local"})
	for _, operation := range cases {
		name := operation["name"].(string)
		in := map[string]any{"id": "root-test"}
		for key, value := range operation {
			if key != "name" {
				in[key] = value
			}
		}
		run(t, f, allowed, name, in)
	}
	if probe.effects.Load() != int32(len(cases)) {
		t.Fatal("fully authorized root operations did not reach shared engine")
	}
}
func TestRemoteErrorRetainsStatus(t *testing.T) {
	err := bridgeError(&agent.RemoteError{Code: "conflict", Message: "revision changed", Status: 409})
	if core.ErrorStatus(err) != 409 || core.ErrorCode(err) != "conflict" {
		t.Fatal("remote domain error was downgraded", err)
	}
}
