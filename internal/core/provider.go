package core

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/jobs"
)

// Binding is a trusted in-process domain adapter. Resources resolves every
// actual source and destination before side effects. List implementations must
// filter each returned resource using Dependencies.Authorize.
type Binding struct {
	Action    Action
	Resources func(context.Context, Actor, json.RawMessage) ([]string, error)
	Execute   func(context.Context, Actor, json.RawMessage) (ActionResult, error)
}
type Provider interface{ Bindings() []Binding }
type ActionResult struct {
	Data         any
	Resources    []string
	TaskID       string
	AuditSummary map[string]any
}
type Bindings []Binding

func (b Bindings) Bindings() []Binding { return []Binding(b) }

type MetadataStore interface {
	Save(context.Context, string, string, json.RawMessage) error
	Get(context.Context, string, string) (json.RawMessage, error)
	List(context.Context, string) (map[string]json.RawMessage, error)
	Delete(context.Context, string, string) error
	GetVersioned(context.Context, string, string) (json.RawMessage, int64, error)
	CompareAndSwap(context.Context, string, string, int64, json.RawMessage) (int64, error)
	Transaction(context.Context, func(MetadataStore) error) error
}

// Secret references are random server-issued IDs. Resolving one requires the
// exact namespace/resource and a currently authorized persisted identity.
type SecretStore interface {
	Put(context.Context, Actor, string, string, []byte) (string, error)
	Get(context.Context, Actor, string, string) ([]byte, error)
	Delete(context.Context, Actor, string, string) error
}
type TaskSubmitter interface {
	Submit(context.Context, Actor, string, json.RawMessage, []string) (map[string]any, error)
	// SubmitPrepared atomically stores namespace-bound provider metadata, the
	// queued task and its server-derived authorization before execution starts.
	// The callback must only use its MetadataStore; it must not perform I/O or
	// call Dispatch, Secrets, Tasks or the unbound State store inside the SQL tx.
	SubmitPrepared(context.Context, Actor, string, json.RawMessage, TaskMetadata, func(MetadataStore, jobs.Job) error) (map[string]any, error)
	Get(context.Context, Actor, string) (map[string]any, error)
	Cancel(context.Context, Actor, string) (map[string]any, error)
}

// TaskMetadata cannot override the current action, scope or actor. Resources
// must be concrete resources already resolved and authorized by that action.
type TaskMetadata struct {
	Resources []string
}
type Dependencies struct {
	State        MetadataStore
	Secrets      SecretStore
	Tasks        TaskSubmitter
	Authorize    func(Actor, string, string) bool
	ResolveActor func(context.Context, Actor) (Actor, error)
	Dispatch     func(context.Context, Actor, string, json.RawMessage) (any, error)
	// Fingerprint is a namespace/domain-bound private-key HMAC for sensitive
	// audit content. It grants no ability to retrieve stored secrets.
	Fingerprint func(string, []byte) string
}

var validActionName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,95}$`)
var validScopeName = regexp.MustCompile(`^[a-z][a-z0-9_-]*:[a-z][a-z0-9_-]*$`)
var validNamespace = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

func (s *Service) RegisterProvider(provider Provider) error {
	if provider == nil {
		return fmt.Errorf("nil provider")
	}
	bindings := provider.Bindings()
	pending := map[string]Binding{}
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if s.registrySealed {
		return fmt.Errorf("action registration is sealed; register providers before serving")
	}
	for _, binding := range bindings {
		a := binding.Action
		if !validActionName.MatchString(a.Name) || a.Description == "" || !validScopeName.MatchString(a.Scope) || binding.Resources == nil || binding.Execute == nil {
			return fmt.Errorf("incomplete or invalid binding %q", a.Name)
		}
		if _, exists := s.providers[a.Name]; exists {
			return fmt.Errorf("duplicate action %q", a.Name)
		}
		if _, exists := pending[a.Name]; exists {
			return fmt.Errorf("duplicate action %q", a.Name)
		}
		for _, builtin := range registry() {
			if builtin.Name == a.Name {
				return fmt.Errorf("cannot replace built-in action %q", a.Name)
			}
		}
		if err := validateSchemaDefinition(a.InputSchema, 0); err != nil {
			return fmt.Errorf("%s input schema: %w", a.Name, err)
		}
		if a.InputSchema["type"] != "object" {
			return fmt.Errorf("%s action input must be an object", a.Name)
		}
		if a.OutputSchema != nil {
			if err := validateSchemaDefinition(a.OutputSchema, 0); err != nil {
				return fmt.Errorf("%s output schema: %w", a.Name, err)
			}
		}
		// Freeze descriptors. Mutating the provider's maps cannot alter published
		// schemas after authorization or bypass an established registry contract.
		binding.Action.InputSchema = cloneSchema(a.InputSchema)
		binding.Action.OutputSchema = cloneSchema(a.OutputSchema)
		pending[a.Name] = binding
	}
	for name, binding := range pending {
		s.providers[name] = binding
	}
	return nil
}
func cloneSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	raw, _ := json.Marshal(schema)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}
func (s *Service) SealRegistry() {
	s.registryMu.Lock()
	s.registrySealed = true
	s.registryMu.Unlock()
	s.executors.seal()
}
func (s *Service) allActions() []Action {
	out := registry()
	s.registryMu.RLock()
	defer s.registryMu.RUnlock()
	names := make([]string, 0, len(s.providers))
	for name := range s.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		out = append(out, s.providers[name].Action)
	}
	return out
}
func (s *Service) binding(name string) (Binding, bool) {
	if legacy, ok := s.legacyFileBinding(name); ok {
		return legacy, true
	}
	s.registryMu.RLock()
	defer s.registryMu.RUnlock()
	binding, ok := s.providers[name]
	return binding, ok
}
func (s *Service) scopeKnown(scope string) bool {
	if tokenScopes[scope] {
		return true
	}
	s.registryMu.RLock()
	extra := s.extraScopes[scope]
	s.registryMu.RUnlock()
	if extra {
		return true
	}
	for _, a := range s.allActions() {
		if a.Scope == scope {
			return true
		}
	}
	return false
}

// RegisterScopes advertises additional explicit guards used by trusted domains,
// such as permissions/host-files/root-terminal, without inventing fake actions.
// Merely registering a scope never grants it to any existing token or actor.
func (s *Service) RegisterScopes(scopes ...string) error {
	s.registryMu.Lock()
	defer s.registryMu.Unlock()
	if s.registrySealed {
		return invalid("Scopes must be registered before serving requests")
	}
	for _, scope := range scopes {
		if !regexp.MustCompile(`^[a-z][a-z0-9_-]*:[a-z][a-z0-9_-]*$`).MatchString(scope) {
			return invalid("Invalid explicit scope")
		}
	}
	for _, scope := range scopes {
		s.extraScopes[scope] = true
	}
	return nil
}
func (s *Service) Scopes(a Actor) []string {
	known := map[string]bool{}
	for name := range tokenScopes {
		known[name] = true
	}
	s.registryMu.RLock()
	for name := range s.extraScopes {
		known[name] = true
	}
	s.registryMu.RUnlock()
	for _, action := range s.allActions() {
		if action.Scope != "" {
			known[action.Scope] = true
		}
	}
	out := []string{}
	for name := range known {
		if hasScope(a, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
func (s *Service) Runtime(namespace string) Dependencies {
	if !validNamespace.MatchString(namespace) {
		panic("invalid trusted provider namespace")
	}
	return Dependencies{State: stateStore{s: s, namespace: namespace}, Secrets: boundSecrets{s: s, namespace: namespace}, Tasks: taskSubmitter{s: s, namespace: namespace}, Authorize: s.Authorized, ResolveActor: s.RefreshActor, Fingerprint: func(domain string, data []byte) string {
		return s.idempotencyInputHash("provider:"+namespace+":"+domain, data)
	}, Dispatch: func(ctx context.Context, a Actor, name string, input json.RawMessage) (any, error) {
		current, err := s.RefreshActor(ctx, a)
		if err != nil {
			return nil, err
		}
		return s.Execute(ctx, current, name, input)
	}}
}

// RefreshActor resolves persisted authorization for scheduled/queued work.
// Scope/resource snapshots supplied by a provider are never trusted here.
func (s *Service) RefreshActor(ctx context.Context, prior Actor) (Actor, error) {
	if strings.HasPrefix(prior.ID, "grant:") {
		return s.temporaryActor(ctx, strings.TrimPrefix(prior.ID, "grant:"))
	}
	if strings.HasPrefix(prior.ID, "token:") {
		var a Actor
		var scopes, resources string
		now := unixNow()
		err := s.db.QueryRowContext(ctx, "SELECT t.id,t.name,t.kind,t.scopes,t.resources FROM tokens t JOIN users u ON u.id=t.owner_id WHERE t.id=? AND t.revoked=0 AND t.expires>? AND (t.parent_id='' OR EXISTS(SELECT 1 FROM tokens p WHERE p.id=t.parent_id AND p.revoked=0 AND p.expires>?))", strings.TrimPrefix(prior.ID, "token:"), now, now).Scan(&a.ID, &a.Name, &a.Kind, &scopes, &resources)
		if err != nil {
			return Actor{}, unauthorized()
		}
		a.ID = "token:" + a.ID
		if json.Unmarshal([]byte(scopes), &a.Scopes) != nil || json.Unmarshal([]byte(resources), &a.Resources) != nil {
			return Actor{}, internal()
		}
		if err = s.checkTokenPolicy(ctx, strings.TrimPrefix(a.ID, "token:"), a, false); err != nil {
			return Actor{}, err
		}
		return a, nil
	}
	var id, name string
	if err := s.db.QueryRowContext(ctx, "SELECT id,username FROM users WHERE id=?", prior.ID).Scan(&id, &name); err != nil {
		return Actor{}, unauthorized()
	}
	return adminActor(id, name), nil
}

// A single dispatcher keeps all domains inside the durable job manager's four
// execution slots. Executors are registered by exact operation names.
type executorRegistry struct {
	mu        sync.RWMutex
	executors map[string]jobs.Executor
	sealed    bool
	service   *Service
}

func newExecutorRegistry() *executorRegistry {
	return &executorRegistry{executors: map[string]jobs.Executor{}}
}
func (e *executorRegistry) register(names []string, executor jobs.Executor) error {
	if executor == nil || len(names) == 0 {
		return fmt.Errorf("executor and operations required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return fmt.Errorf("executor registration sealed")
	}
	seen := map[string]bool{}
	for _, name := range names {
		if !validActionName.MatchString(name) || seen[name] {
			return fmt.Errorf("invalid or repeated executor operation %q", name)
		}
		if _, ok := e.executors[name]; ok {
			return fmt.Errorf("duplicate executor operation %q", name)
		}
		seen[name] = true
	}
	for _, name := range names {
		e.executors[name] = executor
	}
	return nil
}
func (e *executorRegistry) seal() { e.mu.Lock(); e.sealed = true; e.mu.Unlock() }
func (e *executorRegistry) get(name string) (jobs.Executor, error) {
	e.mu.RLock()
	executor := e.executors[name]
	e.mu.RUnlock()
	if executor == nil {
		return nil, fmt.Errorf("operation is not registered")
	}
	return executor, nil
}
func (e *executorRegistry) Validate(r agent.Request) error {
	executor, err := e.get(r.Operation)
	if err != nil {
		return err
	}
	return executor.Validate(r)
}
func (e *executorRegistry) Run(ctx context.Context, r agent.Request) (agent.Result, error) {
	if e.service != nil {
		actor, err := e.service.RefreshActor(ctx, Actor{ID: r.ActorID})
		if err != nil {
			return agent.Result{}, err
		}
		if meta, ok := e.service.loadTaskMetadata(ctx, r.JobID); ok {
			if meta.Actor.ID != r.ActorID || !hasScope(actor, meta.Scope) {
				return agent.Result{}, denied()
			}
			if err = authorizeResources(actor, meta.Resources); err != nil {
				return agent.Result{}, err
			}
			ctx = context.WithValue(ctx, authorizedActionKey{}, authorizedAction{Name: meta.Action, Scope: meta.Scope, Resources: meta.Resources, ActorID: actor.ID})
		} else {
			var input agent.FileInput
			if json.Unmarshal(r.Input, &input) != nil {
				return agent.Result{}, denied()
			}
			scope := "files:write"
			if r.Operation == "file_hash" {
				scope = "files:read"
			}
			if r.Operation != "file_write" && r.Operation != "file_hash" && r.Operation != "file_backup" {
				return agent.Result{}, denied()
			}
			resources := []string{"files:" + input.Root}
			if input.DestinationRoot != "" {
				resources = append(resources, "files:"+input.DestinationRoot)
			}
			if !hasScope(actor, scope) {
				return agent.Result{}, denied()
			}
			if err = authorizeResources(actor, resources); err != nil {
				return agent.Result{}, err
			}
			ctx = context.WithValue(ctx, authorizedActionKey{}, authorizedAction{Name: r.Operation, Scope: scope, Resources: resources, ActorID: actor.ID})
		}
		var release context.CancelFunc
		ctx, release = e.service.taskLeaseContext(ctx)
		defer release()
	}
	executor, err := e.get(r.Operation)
	if err != nil {
		return agent.Result{}, err
	}
	return executor.Run(ctx, r)
}
func (s *Service) RegisterExecutor(names []string, executor jobs.Executor) error {
	return s.executors.register(names, executor)
}
