package systemops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type Script struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	ProfileID string     `json:"profile_id"`
	Revision  int64      `json:"revision"`
	BodyHash  string     `json:"body_hash"`
	SecretRef string     `json:"secret_ref,omitempty"`
	CreatedBy core.Actor `json:"created_by"`
}
type scriptMutation struct {
	ID               string `json:"id,omitempty"`
	Name             string `json:"name"`
	ProfileID        string `json:"profile_id"`
	Body             string `json:"body"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
}
type scriptJob struct {
	Actor            core.Actor `json:"actor"`
	ID               string     `json:"id"`
	ExpectedRevision int64      `json:"expected_revision"`
}

func (p *Provider) scriptResources(ctx context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID        string `json:"id"`
		ProfileID string `json:"profile_id"`
	}
	_ = json.Unmarshal(raw, &in)
	resources := []string{"host:local"}
	if in.ID != "" {
		if !p.deps.Authorize(a, "scripts:read", "script:"+in.ID) && !p.deps.Authorize(a, "scripts:write", "script:"+in.ID) && !p.deps.Authorize(a, "scripts:execute", "script:"+in.ID) {
			return nil, denied()
		}
		script, err := p.loadScript(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		in.ProfileID = script.ProfileID
		resources = []string{"script:" + in.ID}
	}
	if in.ProfileID != "" {
		profile, err := p.scriptProfile(ctx, a, in.ProfileID)
		if err != nil {
			return nil, err
		}
		if profile.Root {
			resources = append(resources, "host:local")
		}
	}
	return resources, nil
}
func (p *Provider) scriptReadResources(_ context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &in)
	if !identifier.MatchString(in.ID) {
		return nil, invalid("Script ID required")
	}
	resource := "script:" + in.ID
	if !p.deps.Authorize(a, "scripts:read", resource) {
		return nil, denied()
	}
	return []string{resource}, nil
}
func (p *Provider) scriptBindings() []core.Binding {
	bindings := []core.Binding{
		p.bind("scripts_profiles", "List trusted, isolated execution profile choices without accepting shell paths", "scripts:read", true, object(map[string]any{}), hostResources, func(ctx context.Context, a core.Actor, _ json.RawMessage) (core.ActionResult, error) {
			views, err := p.scriptProfiles(ctx, a)
			return core.ActionResult{Data: views}, err
		}),
		p.bind("scripts_list", "List private saved script metadata without body or secret references", "scripts:read", true, object(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, p.listScripts),
		p.bind("scripts_get", "Read script metadata and optionally its resource-bound private body", "scripts:read", true, object(map[string]any{"id": text(96), "include_body": boolean()}, "id"), p.scriptReadResources, p.getScript),
		p.bind("scripts_save", "Save encrypted script body with expected revision and a trusted execution profile", "scripts:write", false, object(map[string]any{"id": text(96), "name": text(160), "profile_id": text(96), "body": text(32768), "expected_revision": integer(1, 1<<30)}, "name", "profile_id", "body"), p.scriptResources, p.saveScript),
		p.bind("scripts_delete", "Delete a script using expected revision and release its encrypted body", "scripts:write", false, object(map[string]any{"id": text(96), "expected_revision": integer(1, 1<<30)}, "id", "expected_revision"), p.scriptResources, p.deleteScript),
	}
	for _, spec := range []struct{ name, scope string }{{"scripts_run", "scripts:execute"}, {"scripts_run_root", "terminal:root"}} {
		binding := p.bind(spec.name, "Submit an isolated script task pinned to its reviewed revision; root requires extra explicit policy and scope", "scripts:execute", false, object(map[string]any{"id": text(96), "expected_revision": integer(1, 1<<30)}, "id", "expected_revision"), p.scriptResources, p.runScript)
		binding.Action.Scope = spec.scope
		binding.Action.Task = true
		bindings = append(bindings, binding)
	}
	return bindings
}
func (p *Provider) scriptProfiles(ctx context.Context, a core.Actor) ([]ScriptProfileView, error) {
	if p.options.Agent == nil {
		return nil, unavailable("Script execution requires an explicitly configured privileged host executor")
	}
	raw, _ := json.Marshal(scriptAgentInput{ActorID: a.ID})
	result, err := p.options.Agent.Run(ctx, agent.Request{Operation: "system_script_profiles", Input: raw})
	if err != nil {
		return nil, hostActionError(err)
	}
	var profiles []ScriptProfileView
	if err = json.Unmarshal([]byte(result.Stdout), &profiles); err != nil {
		return nil, err
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return profiles, nil
}
func (p *Provider) scriptProfile(ctx context.Context, a core.Actor, id string) (ScriptProfileView, error) {
	profiles, err := p.scriptProfiles(ctx, a)
	if err != nil {
		return ScriptProfileView{}, err
	}
	for _, profile := range profiles {
		if profile.ID == id {
			return profile, nil
		}
	}
	return ScriptProfileView{}, missing("Trusted script execution profile")
}
func (p *Provider) scriptRootAuthorize(a core.Actor, profile ScriptProfileView, scope, resource string) error {
	if !p.deps.Authorize(a, scope, resource) {
		return denied()
	}
	if profile.Root && (!scriptHasRootGrant(a.Scopes) || !p.deps.Authorize(a, "terminal:root", "host:local")) {
		return denied()
	}
	return nil
}
func (p *Provider) loadScript(ctx context.Context, id string) (Script, error) {
	raw, revision, err := p.deps.State.GetVersioned(ctx, "script", id)
	if err != nil {
		return Script{}, err
	}
	var out Script
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	out.Revision = revision
	return out, nil
}
func (p *Provider) listScripts(ctx context.Context, a core.Actor, _ json.RawMessage) (core.ActionResult, error) {
	records, err := p.deps.State.List(ctx, "script")
	if err != nil {
		return core.ActionResult{}, err
	}
	out := []Script{}
	for id := range records {
		if p.deps.Authorize(a, "scripts:read", "script:"+id) {
			script, err := p.loadScript(ctx, id)
			if err != nil {
				return core.ActionResult{}, err
			}
			script.SecretRef = ""
			out = append(out, script)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return core.ActionResult{Data: out}, nil
}
func (p *Provider) getScript(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ID          string `json:"id"`
		IncludeBody bool   `json:"include_body"`
	}
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	script, err := p.loadScript(ctx, in.ID)
	if err != nil {
		return core.ActionResult{}, err
	}
	data := map[string]any{"script": script}
	if in.IncludeBody {
		body, err := p.deps.Secrets.Get(ctx, a, "script:"+script.ID, script.SecretRef)
		if err != nil {
			return core.ActionResult{}, err
		}
		data["body"] = string(body)
	}
	script.SecretRef = ""
	data["script"] = script
	return core.ActionResult{Data: data}, nil
}
func (p *Provider) saveScript(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in scriptMutation
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	if in.Name == "" || !validScriptBody(in.Body) {
		return core.ActionResult{}, invalid("Script requires a name and a body of 1..32768 bytes without NUL")
	}
	script := Script{ID: in.ID, Name: in.Name, ProfileID: in.ProfileID, BodyHash: hashBytes([]byte(in.Body)), CreatedBy: a.Public()}
	oldRef := ""
	var err error
	if in.ID == "" {
		if in.ExpectedRevision != 0 {
			return core.ActionResult{}, invalid("New scripts have no expected revision")
		}
		script.ID, err = newID("script_")
		if err != nil {
			return core.ActionResult{}, err
		}
	} else {
		old, err := p.loadScript(ctx, in.ID)
		if err != nil {
			return core.ActionResult{}, err
		}
		if old.Revision != in.ExpectedRevision {
			return core.ActionResult{}, conflict("Script revision changed")
		}
		oldProfile, err := p.scriptProfile(ctx, a, old.ProfileID)
		if err != nil {
			return core.ActionResult{}, err
		}
		if err = p.scriptRootAuthorize(a, oldProfile, "scripts:write", "script:"+old.ID); err != nil {
			return core.ActionResult{}, err
		}
		oldRef = old.SecretRef
		script.CreatedBy = old.CreatedBy
	}
	profile, err := p.scriptProfile(ctx, a, in.ProfileID)
	if err != nil {
		return core.ActionResult{}, err
	}
	if err = p.scriptRootAuthorize(a, profile, "scripts:write", "script:"+script.ID); err != nil {
		return core.ActionResult{}, err
	}
	script.SecretRef, err = p.deps.Secrets.Put(ctx, a, "script:"+script.ID, "script_body", []byte(in.Body))
	if err != nil {
		return core.ActionResult{}, err
	}
	value, _ := json.Marshal(script)
	err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		if in.ID == "" {
			records, err := store.List(ctx, "script")
			if err != nil {
				return err
			}
			if len(records) >= 256 {
				return invalid("Script limit (256) reached")
			}
		}
		var err error
		script.Revision, err = store.CompareAndSwap(ctx, "script", script.ID, in.ExpectedRevision, value)
		return err
	})
	if err != nil {
		_ = p.deps.Secrets.Delete(context.WithoutCancel(ctx), a, "script:"+script.ID, script.SecretRef)
		return core.ActionResult{}, err
	}
	if oldRef != "" {
		_ = p.deps.Secrets.Delete(context.WithoutCancel(ctx), a, "script:"+script.ID, oldRef)
	}
	script.SecretRef = ""
	resource := "script:" + script.ID
	if in.ID == "" {
		resource = "host:local"
	}
	return core.ActionResult{Data: script, Resources: []string{resource}, AuditSummary: map[string]any{"id": script.ID, "revision": script.Revision, "profile_id": profile.ID}}, nil
}
func (p *Provider) deleteScript(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ID               string `json:"id"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	script, err := p.loadScript(ctx, in.ID)
	if err != nil {
		return core.ActionResult{}, err
	}
	profile, err := p.scriptProfile(ctx, a, script.ProfileID)
	if err != nil {
		return core.ActionResult{}, err
	}
	if err = p.scriptRootAuthorize(a, profile, "scripts:write", "script:"+in.ID); err != nil {
		return core.ActionResult{}, err
	}
	err = p.deps.State.Transaction(ctx, func(store core.MetadataStore) error {
		_, revision, err := store.GetVersioned(ctx, "script", in.ID)
		if err != nil {
			return err
		}
		if revision != in.ExpectedRevision {
			return conflict("Script revision changed")
		}
		return store.Delete(ctx, "script", in.ID)
	})
	if err == nil {
		err = p.deps.Secrets.Delete(ctx, a, "script:"+in.ID, script.SecretRef)
	}
	return core.ActionResult{Data: map[string]any{"deleted": err == nil}}, err
}
func (p *Provider) runScript(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ID               string `json:"id"`
		ExpectedRevision int64  `json:"expected_revision"`
	}
	if err := input(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	script, err := p.loadScript(ctx, in.ID)
	if err != nil {
		return core.ActionResult{}, err
	}
	if script.Revision != in.ExpectedRevision {
		return core.ActionResult{}, conflict("Script revision changed")
	}
	profile, err := p.scriptProfile(ctx, a, script.ProfileID)
	if err != nil {
		return core.ActionResult{}, err
	}
	if err = p.scriptRootAuthorize(a, profile, "scripts:execute", "script:"+in.ID); err != nil {
		return core.ActionResult{}, err
	}
	resources := []string{"script:" + in.ID}
	if profile.Root {
		resources = append(resources, "host:local")
	}
	payload, _ := json.Marshal(scriptJob{Actor: a.Public(), ID: script.ID, ExpectedRevision: script.Revision})
	task, err := p.deps.Tasks.Submit(ctx, a, "systemops_script_run", payload, resources)
	id, _ := task["id"].(string)
	return core.ActionResult{Data: map[string]any{"task": task, "task_id": id}, TaskID: id, Resources: resources}, err
}
func (p *Provider) executeScript(ctx context.Context, request agent.Request) (agent.Result, error) {
	var job scriptJob
	if err := strictJSON(request.Input, &job); err != nil {
		return agent.Result{}, err
	}
	a, err := p.deps.ResolveActor(ctx, job.Actor)
	if err != nil {
		return agent.Result{}, err
	}
	script, err := p.loadScript(ctx, job.ID)
	if err != nil {
		return agent.Result{}, err
	}
	if script.Revision != job.ExpectedRevision {
		return agent.Result{}, conflict("Script revision changed while queued")
	}
	profile, err := p.scriptProfile(ctx, a, script.ProfileID)
	if err != nil {
		return agent.Result{}, err
	}
	if err = p.scriptRootAuthorize(a, profile, "scripts:execute", "script:"+script.ID); err != nil {
		return agent.Result{}, err
	}
	streamer, ok := p.options.Agent.(interface {
		Stream(context.Context, agent.Request, agent.IO) (agent.Result, error)
	})
	if !ok {
		return agent.Result{}, unavailable("Agent does not support private script stdin streams")
	}
	body, err := p.deps.Secrets.Get(ctx, a, "script:"+script.ID, script.SecretRef)
	if err != nil {
		return agent.Result{}, err
	}
	if !validScriptBody(string(body)) || hashBytes(body) != script.BodyHash {
		return agent.Result{}, errors.New("private script body integrity check failed")
	}
	defer func() {
		for i := range body {
			body[i] = 0
		}
	}()
	raw, _ := json.Marshal(scriptAgentInput{ActorID: a.ID, ProfileID: script.ProfileID, RootAuthorized: profile.Root})
	result, err := streamer.Stream(ctx, agent.Request{Operation: "system_script_run", Input: raw, Output: request.Output}, agent.IO{Stdin: io.NopCloser(bytes.NewReader(body))})
	result.Operation = "systemops_script_run"
	result.FinishedAt = time.Now().UTC()
	return result, err
}
