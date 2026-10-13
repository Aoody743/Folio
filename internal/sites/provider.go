package sites

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func New(deps core.Dependencies, options Options) (*Provider, error) {
	if deps.State == nil || deps.Secrets == nil || deps.Tasks == nil || deps.Authorize == nil || deps.ResolveActor == nil {
		return nil, errors.New("website provider requires shared persistent authorization, secrets and tasks")
	}
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return &Provider{deps: deps, options: options, categoryGate: gate}, nil
}

// lockCategoryMutations coordinates category create/update/delete with site
// operations that can commit or roll back category-bearing site metadata. The
// gate is intentionally scoped to this registered Provider: the supported CLI
// runtime holds server.lock before creating its one Core/Provider per data
// directory. It is not a cross-process or arbitrary embedded-Core coordinator.
func (p *Provider) lockCategoryMutations(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.categoryGate:
		var once sync.Once
		release := func() { once.Do(func() { p.categoryGate <- struct{}{} }) }
		if err := ctx.Err(); err != nil {
			release()
			return nil, err
		}
		return release, nil
	}
}

func appendResource(resources []string, resource string) []string {
	if resource == "" {
		return resources
	}
	for _, existing := range resources {
		if existing == resource {
			return resources
		}
	}
	return append(resources, resource)
}

func (p *Provider) authorizeCategory(ctx context.Context, actor core.Actor, id string) error {
	if id == "" {
		return nil
	}
	if !safeID.MatchString(id) {
		return failure("category_invalid", "Website category identity needs recovery inspection", 409)
	}
	resource := "site-category:" + id
	if !p.deps.Authorize(actor, "sites:read", resource) {
		return denied()
	}
	_, err := p.deps.State.Get(ctx, "categories", id)
	return err
}

func object(fields map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": fields, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
func text(max int) map[string]any { return map[string]any{"type": "string", "maxLength": max} }
func idField() map[string]any {
	return map[string]any{"type": "string", "pattern": `^[a-z][a-z0-9_-]{0,95}$`}
}
func number(min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max}
}
func boolean() map[string]any { return map[string]any{"type": "boolean"} }
func array(item map[string]any, max int) map[string]any {
	return map[string]any{"type": "array", "items": item, "maxItems": max}
}
func choice(values ...string) map[string]any { return map[string]any{"type": "string", "enum": values} }
func (p *Provider) binding(name, description, scope, operation string, read, task bool, schema map[string]any, resource func(context.Context, core.Actor, json.RawMessage) ([]string, error)) core.Binding {
	return core.Binding{Action: core.Action{Name: name, Description: description, Scope: scope, ReadOnly: read, Task: task, Audit: strings.Contains(name, "export") || strings.Contains(name, "logs"), InputSchema: schema}, Resources: resource, Execute: func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		return p.perform(ctx, actor, name, operation, task, raw)
	}}
}

func siteResource(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &in) != nil || !safeID.MatchString(in.ID) {
		return nil, invalid("A website ID is required")
	}
	return []string{Resource(in.ID)}, nil
}
func certResource(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &in) != nil || !safeID.MatchString(in.ID) {
		return nil, invalid("A certificate ID is required")
	}
	return []string{CertificateResource(in.ID)}, nil
}
func hostResource(context.Context, core.Actor, json.RawMessage) ([]string, error) {
	return []string{"host:local"}, nil
}
func noResources(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }

func (p *Provider) Bindings() []core.Binding {
	revision := number(0, 1<<30)
	common := map[string]any{"id": idField(), "expected_revision": revision}
	domains := array(text(300), 100)
	proxy := object(map[string]any{"id": idField(), "path": text(1024), "upstream": text(2048), "enabled": boolean(), "preserve_host": boolean(), "cache_seconds": number(0, 86400), "websocket": boolean(), "headers": map[string]any{"type": "object", "additionalProperties": text(1024), "maxProperties": 32}}, "id", "path", "upstream", "enabled")
	redirect := object(map[string]any{"id": idField(), "path": text(1024), "destination": text(2048), "status": map[string]any{"type": "integer", "enum": []int{301, 302, 307, 308}}, "enabled": boolean(), "preserve_uri": boolean()}, "id", "path", "destination", "status", "enabled")
	binding := object(map[string]any{"id": idField(), "domains": domains, "path": text(1024), "rewrite": text(16 << 10)}, "id", "domains", "path")
	fields := map[string]any{
		"reset_advanced_config": boolean(),
		"id":                    idField(), "expected_revision": revision, "name": text(160), "domains": domains, "web_instance": idField(), "php_instance": text(96), "run_path": text(1024), "category_id": text(96), "status": choice("running", "stopped"), "default": boolean(), "ipv6": boolean(), "index": array(text(128), 16), "description": text(4000), "expires_at": text(64), "rewrite": text(16 << 10), "access_log": boolean(),
		"rate_limit":         object(map[string]any{"enabled": boolean(), "connections": number(0, 100000), "bytes_per_second": number(0, 1<<40), "requests_per_second": number(0, 100000)}),
		"protection":         object(map[string]any{"enabled": boolean(), "username": text(64), "password": text(72), "deny_paths": array(text(1024), 64), "anti_hotlink": boolean(), "allowed_referers": domains}),
		"php":                object(map[string]any{"open_basedir": boolean(), "values": map[string]any{"type": "object", "additionalProperties": text(1024), "maxProperties": 32}}),
		"directory_bindings": array(binding, 64), "proxies": array(proxy, 64), "redirects": array(redirect, 64),
	}
	bindings := []core.Binding{
		p.binding("sites_list", "List actual websites with pagination and resource filtering", "sites:read", "site_list", true, false, object(map[string]any{"limit": number(1, 500), "offset": number(0, 1<<30), "query": text(256), "category_id": text(96)}), noResources),
		p.binding("sites_get", "Read one actual website, configuration revision and runtime status", "sites:read", "site_get", true, false, object(map[string]any{"id": idField()}, "id"), siteResource),
		p.binding("sites_create", "Create an isolated website, validate Web/PHP config and reload with rollback", "sites:write", "site_apply", false, true, object(fields, "id", "name", "domains", "web_instance"), p.applyResources),
		p.binding("sites_update", "Update typed website settings using its expected revision", "sites:write", "site_apply", false, true, object(fields, "id", "expected_revision"), p.applyResources),
		p.binding("sites_delete", "Remove only the owned website configuration; retain content by default", "sites:write", "site_delete", false, true, object(map[string]any{"id": idField(), "expected_revision": revision, "keep_data": boolean()}, "id", "expected_revision"), siteResource),
		p.binding("sites_retained_list", "List owned content retained after website deletion, including its file root and revision", "sites:read", "site_retained_list", true, false, object(map[string]any{}), noResources),
		p.binding("sites_retained_purge", "Permanently remove selected retained website content after draining file operations", "sites:write", "site_retained_purge", false, true, object(common, "id", "expected_revision"), siteResource),
		p.binding("sites_default_set", "Select a default website with explicit revisions for both the new and previous defaults", "sites:write", "site_default_set", false, true, object(map[string]any{"id": idField(), "expected_revision": revision, "previous_site_id": text(96), "previous_revision": revision}, "id", "expected_revision"), p.defaultResources),
		p.binding("sites_default_get", "Read the selected and current default websites after checking access to both resources", "sites:read", "site_default_get", true, false, object(map[string]any{"id": idField()}, "id"), siteResource),
		p.binding("sites_logs", "Read bounded actual Web/PHP log lines", "sites:read", "site_logs", true, false, object(map[string]any{"id": idField(), "kind": choice("access", "error", "php"), "offset": number(0, 1<<30), "limit": number(1, 1000)}, "id", "kind"), siteResource),
		p.binding("sites_config_get", "Read the actual owned virtual-host configuration", "sites:read", "site_config_get", true, false, object(map[string]any{"id": idField()}, "id"), siteResource),
		p.binding("sites_config_set", "Apply advanced Web configuration only with explicitly delegated root authority and a trusted policy opt-in", "sites:config", "site_config_set", false, true, object(map[string]any{"id": idField(), "expected_revision": revision, "config": text(48 << 10)}, "id", "expected_revision", "config"), func(ctx context.Context, a core.Actor, raw json.RawMessage) ([]string, error) {
			if !p.deps.Authorize(a, "terminal:root", "host:local") {
				return nil, denied()
			}
			resources, err := siteResource(ctx, a, raw)
			return append(resources, "host:local"), err
		}),
		p.binding("sites_backups_list", "List owned website content/configuration backups", "sites:read", "site_backups_list", true, false, object(map[string]any{"id": idField()}, "id"), siteResource),
		p.binding("sites_backup_create", "Create a real verified website content and settings backup", "sites:backup", "site_backup_create", false, true, object(common, "id", "expected_revision"), siteResource),
		p.binding("sites_backup_restore", "Restore a verified backup after staging, conflict checks and config validation", "sites:backup", "site_backup_restore", false, true, object(map[string]any{"id": idField(), "expected_revision": revision, "backup_id": idField()}, "id", "expected_revision", "backup_id"), p.restoreResources),
		p.binding("sites_backup_delete", "Delete only the selected owned backup", "sites:backup", "site_backup_delete", false, false, object(map[string]any{"id": idField(), "backup_id": idField()}, "id", "backup_id"), siteResource),
		p.binding("sites_rewrite_templates", "Discover safe rewrite templates and required runtime", "sites:read", "", true, false, object(map[string]any{}), noResources),
		p.binding("certificates_list", "List actual certificates within authorized resources, excluding private keys", "certificates:read", "certificate_list", true, false, object(map[string]any{}), noResources),
		p.binding("certificates_get", "Read X.509 subject, issuer, SANs, validity and fingerprint", "certificates:read", "certificate_get", true, false, object(map[string]any{"id": idField()}, "id"), certResource),
		p.binding("certificates_import", "Import a matching PEM chain/private key into protected Agent storage", "certificates:write", "certificate_import", false, false, object(map[string]any{"id": idField(), "name": text(160), "cert_pem": text(32 << 10), "key_pem": text(8 << 10), "expected_revision": revision}, "id", "name", "cert_pem", "key_pem"), certResource),
		p.binding("certificates_delete", "Remove an unused owned certificate; reject live site references", "certificates:write", "certificate_delete", false, false, object(common, "id", "expected_revision"), certResource),
		p.binding("certificates_export", "Export certificate/private key only with the explicit export scope", "certificates:export", "certificate_export", true, false, object(map[string]any{"id": idField()}, "id"), certResource),
		p.binding("sites_tls_set", "Bind a certificate after SAN/validity checks and validated reload", "sites:write", "site_tls_set", false, true, object(map[string]any{"id": idField(), "expected_revision": revision, "certificate_id": idField(), "enabled": boolean(), "redirect": boolean()}, "id", "expected_revision", "enabled"), p.tlsResources),
		p.binding("sites_php_cli_get", "Read the explicitly managed CLI PHP selection", "software:read", "site_php_cli_get", true, false, object(map[string]any{}), hostResource),
		p.binding("sites_php_cli_set", "Set the owned PHP CLI wrapper without replacing distribution executables", "software:manage", "site_php_cli_set", false, false, object(map[string]any{"php_instance": idField()}, "php_instance"), hostResource),
	}
	bindings = append(bindings, p.categoryBindings()...)
	bindings = append(bindings, p.BackupDownloadBindings()...)
	return append(bindings, p.acmeBindings()...)
}

func (p *Provider) call(ctx context.Context, actor core.Actor, operation string, in request, out any) error {
	if p.options.Agent == nil {
		return failure("agent_unavailable", "Website management requires an explicitly configured privileged Agent", 503)
	}
	in.Actor = actor.Public()
	result, err := p.options.Agent.Run(ctx, agent.Request{Operation: operation, ActorID: actor.ID, Input: encode(in)})
	if err != nil {
		return providerError(err)
	}
	data := result.Data
	if len(data) == 0 && result.Stdout != "" {
		data = json.RawMessage(result.Stdout)
	}
	if len(data) == 0 {
		return failure("agent_response", "Website Agent returned no verified result", 502)
	}
	if err = json.Unmarshal(data, out); err != nil {
		return failure("agent_response", "Website Agent returned an invalid result", 502)
	}
	return nil
}

func (p *Provider) get(ctx context.Context, actor core.Actor, id string) (Site, error) {
	var site Site
	err := p.call(ctx, actor, "site_get", request{ID: id}, &site)
	return site, err
}
func (p *Provider) applyResources(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID       string `json:"id"`
		Web      string `json:"web_instance"`
		PHP      string `json:"php_instance"`
		Default  bool   `json:"default"`
		Category string `json:"category_id"`
	}
	if json.Unmarshal(raw, &in) != nil || !safeID.MatchString(in.ID) {
		return nil, invalid("Invalid website identity")
	}
	resources := []string{Resource(in.ID)}
	if !p.deps.Authorize(actor, "sites:write", resources[0]) {
		return nil, denied()
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if _, update := fields["expected_revision"]; update {
		current, err := p.get(ctx, actor, in.ID)
		if err != nil && !isNotFound(err) {
			return nil, err
		}
		if err == nil {
			if _, present := fields["web_instance"]; !present {
				in.Web = current.WebInstance
			}
			if _, present := fields["php_instance"]; !present {
				in.PHP = current.PHPInstance
			}
			if current.Default {
				resources = append(resources, "host:local")
			}
			if _, present := fields["category_id"]; !present {
				in.Category = current.CategoryID
			}
		}
	}
	if in.Web != "" {
		if !p.deps.Authorize(actor, "software:read", "software:"+in.Web) {
			return nil, denied()
		}
		resources = append(resources, "software:"+in.Web)
	}
	if in.PHP != "" {
		if !p.deps.Authorize(actor, "software:read", "software:"+in.PHP) {
			return nil, denied()
		}
		resources = append(resources, "software:"+in.PHP)
	}
	if in.Category != "" {
		if err := p.authorizeCategory(ctx, actor, in.Category); err != nil {
			return nil, err
		}
		resources = appendResource(resources, "site-category:"+in.Category)
	}
	if in.Default {
		resources = append(resources, "host:local")
	}
	return resources, nil
}
func providerError(err error) error {
	if err == nil {
		return nil
	}
	var remote *agent.RemoteError
	if errors.As(err, &remote) {
		return &core.Error{Code: remote.Code, Message: remote.Message, Status: remote.Status}
	}
	return err
}
func (p *Provider) tlsResources(_ context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
	var in struct {
		ID            string `json:"id"`
		CertificateID string `json:"certificate_id"`
	}
	if json.Unmarshal(raw, &in) != nil || !safeID.MatchString(in.ID) {
		return nil, invalid("Invalid TLS binding")
	}
	resources := []string{Resource(in.ID)}
	if in.CertificateID != "" {
		if !p.deps.Authorize(actor, "certificates:read", CertificateResource(in.CertificateID)) {
			return nil, denied()
		}
		resources = append(resources, CertificateResource(in.CertificateID))
	}
	return resources, nil
}

func (p *Provider) perform(ctx context.Context, actor core.Actor, name, operation string, task bool, raw json.RawMessage) (core.ActionResult, error) {
	if name == "sites_rewrite_templates" {
		return core.ActionResult{Data: map[string]any{"items": []map[string]string{{"id": "static", "name": "Static", "content": "try_files $uri $uri/ =404;"}, {"id": "php-front-controller", "name": "PHP front controller", "content": "try_files $uri $uri/ /index.php?$query_string;"}, {"id": "single-page-app", "name": "Single-page application", "content": "try_files $uri $uri/ /index.html;"}}}}, nil
	}
	if name == "sites_list" {
		var in struct {
			Limit, Offset int
			Query         string
			CategoryID    string `json:"category_id"`
		}
		if err := strict(raw, &in); err != nil {
			return core.ActionResult{}, err
		}
		if in.Limit == 0 {
			in.Limit = 100
		}
		var all []Site
		if err := p.call(ctx, actor, operation, request{}, &all); err != nil {
			return core.ActionResult{}, err
		}
		filtered := []Site{}
		for _, site := range all {
			if !p.deps.Authorize(actor, "sites:read", Resource(site.ID)) {
				continue
			}
			if in.CategoryID != "" && site.CategoryID != in.CategoryID {
				continue
			}
			if in.Query != "" && !strings.Contains(strings.ToLower(site.Name+" "+strings.Join(site.Domains, " ")), strings.ToLower(in.Query)) {
				continue
			}
			filtered = append(filtered, site)
		}
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })
		total := len(filtered)
		start := min(in.Offset, total)
		end := min(start+in.Limit, total)
		return core.ActionResult{Data: map[string]any{"items": filtered[start:end], "total": total, "limit": in.Limit, "offset": in.Offset}}, nil
	}
	if name == "certificates_list" {
		var all []Certificate
		if err := p.call(ctx, actor, operation, request{}, &all); err != nil {
			return core.ActionResult{}, err
		}
		items := []Certificate{}
		for _, cert := range all {
			if p.deps.Authorize(actor, "certificates:read", CertificateResource(cert.ID)) {
				items = append(items, cert)
			}
		}
		return core.ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
	}
	if name == "sites_retained_list" {
		var all []RetainedSite
		if err := p.call(ctx, actor, operation, request{}, &all); err != nil {
			return core.ActionResult{}, err
		}
		items := []map[string]any{}
		for _, value := range all {
			if p.deps.Authorize(actor, "sites:read", Resource(value.Site.ID)) {
				items = append(items, map[string]any{"id": value.Site.ID, "name": value.Site.Name, "revision": value.Site.Revision, "root_id": "site-" + value.Site.ID, "retained_at": value.RetainedAt})
			}
		}
		return core.ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	in := request{Actor: actor.Public(), KeepData: true}
	_ = json.Unmarshal(fields["id"], &in.ID)
	_ = json.Unmarshal(fields["expected_revision"], &in.ExpectedRevision)
	_ = json.Unmarshal(fields["kind"], &in.Kind)
	_ = json.Unmarshal(fields["offset"], &in.Offset)
	_ = json.Unmarshal(fields["limit"], &in.Limit)
	_ = json.Unmarshal(fields["backup_id"], &in.BackupID)
	_ = json.Unmarshal(fields["config"], &in.Config)
	_ = json.Unmarshal(fields["reset_advanced_config"], &in.ResetAdvancedConfig)
	_ = json.Unmarshal(fields["previous_site_id"], &in.PreviousSiteID)
	_ = json.Unmarshal(fields["previous_revision"], &in.PreviousRevision)
	if keep, ok := fields["keep_data"]; ok {
		_ = json.Unmarshal(keep, &in.KeepData)
	}
	resources := []string{Resource(in.ID)}
	if name == "sites_default_set" {
		var err error
		resources, err = p.defaultResources(ctx, actor, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
	}
	if name == "sites_backup_restore" {
		var err error
		resources, err = p.restoreResources(ctx, actor, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
	}
	if name == "sites_default_get" {
		return p.defaultView(ctx, actor, in)
	}
	if strings.HasPrefix(name, "certificates_") {
		resources = []string{CertificateResource(in.ID)}
	}
	if name == "sites_create" || name == "sites_update" {
		site := Site{ID: in.ID, RunPath: ".", Status: "running", AccessLog: true, PHP: PHPSettings{OpenBasedir: true, Values: map[string]string{}}, DirectoryBindings: []DirectoryBinding{}, Proxies: []Proxy{}, Redirects: []Redirect{}, Index: []string{"index.html", "index.htm", "index.php"}}
		if name == "sites_update" {
			current, err := p.get(ctx, actor, in.ID)
			if err != nil {
				return core.ActionResult{}, err
			}
			site = current
			if site.Revision != in.ExpectedRevision {
				return core.ActionResult{}, failure("conflict", "Website revision changed; reload before saving", 409)
			}
		} else {
			if _, present := fields["expected_revision"]; present && in.ExpectedRevision != 0 {
				return core.ActionResult{}, invalid("A new website starts at revision zero")
			}
		}
		for key, value := range fields {
			if key == "id" || key == "expected_revision" || key == "protection" || key == "reset_advanced_config" {
				continue
			}
			patch := map[string]json.RawMessage{key: value}
			if err := json.Unmarshal(encode(patch), &site); err != nil {
				return core.ActionResult{}, invalid("Invalid website setting")
			}
		}
		if protected, ok := fields["protection"]; ok {
			var values map[string]json.RawMessage
			_ = json.Unmarshal(protected, &values)
			passwordValue := values["password"]
			delete(values, "password")
			if err := json.Unmarshal(encode(values), &site.Protection); err != nil {
				return core.ActionResult{}, invalid("Invalid protection setting")
			}
			if passwordValue != nil {
				var password string
				_ = json.Unmarshal(passwordValue, &password)
				if len(password) < 8 || len(password) > 72 {
					return core.ActionResult{}, invalid("HTTP protection password must contain eight to 72 bytes")
				}
				ref, err := p.deps.Secrets.Put(ctx, actor, Resource(site.ID), "site-http-password", []byte(password))
				if err != nil {
					return core.ActionResult{}, err
				}
				site.Protection.PasswordRef = ref
			}
		}
		if err := validateSite(&site); err != nil {
			return core.ActionResult{}, err
		}
		site.UpdatedBy = actor.Public()
		site.UpdatedAt = time.Now().UTC()
		in.Site = &site
		var err error
		resources, err = p.applyResources(ctx, actor, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
	}
	if name == "certificates_import" {
		var nameValue, cert, key string
		_ = json.Unmarshal(fields["name"], &nameValue)
		_ = json.Unmarshal(fields["cert_pem"], &cert)
		_ = json.Unmarshal(fields["key_pem"], &key)
		meta, err := inspectCertificate(in.ID, nameValue, cert, key)
		if err != nil {
			return core.ActionResult{}, err
		}
		in.Certificate = &meta
		in.CertPEM = cert
		in.KeyPEM = key
	}
	if name == "sites_tls_set" {
		site, err := p.get(ctx, actor, in.ID)
		if err != nil {
			return core.ActionResult{}, err
		}
		if site.Revision != in.ExpectedRevision {
			return core.ActionResult{}, failure("conflict", "Website revision changed", 409)
		}
		_ = json.Unmarshal(fields["enabled"], &site.TLS.Enabled)
		_ = json.Unmarshal(fields["redirect"], &site.TLS.Redirect)
		_ = json.Unmarshal(fields["certificate_id"], &site.TLS.CertificateID)
		if site.TLS.Enabled && !safeID.MatchString(site.TLS.CertificateID) {
			return core.ActionResult{}, invalid("TLS needs a certificate")
		}
		in.Site = &site
		operation = "site_apply"
		resources, err = p.tlsResources(ctx, actor, raw)
		if err != nil {
			return core.ActionResult{}, err
		}
	}
	if name == "sites_php_cli_get" || name == "sites_php_cli_set" {
		resources = []string{"host:local"}
		_ = json.Unmarshal(fields["php_instance"], &in.ID)
	}
	if task {
		job, err := p.deps.Tasks.Submit(ctx, actor, operation, encode(in), resources)
		id, _ := job["id"].(string)
		return core.ActionResult{Data: job, Resources: resources, TaskID: id}, err
	}
	var data any
	err := p.call(ctx, actor, operation, in, &data)
	return core.ActionResult{Data: data, Resources: resources, AuditSummary: map[string]any{"website_id": in.ID}}, err
}

// All slow website mutations use the shared durable four-slot manager.
func JobOperationNames() []string {
	return []string{"site_apply", "site_delete", "site_retained_purge", "site_default_set", "site_config_set", "site_backup_create", "site_backup_restore", "certificate_acme_issue", "certificate_acme_order"}
}
func (p *Provider) Validate(job agent.Request) error {
	known := false
	for _, name := range JobOperationNames() {
		if name == job.Operation {
			known = true
		}
	}
	if !known {
		return agent.ErrUnauthorized
	}
	var in request
	if err := strict(job.Input, &in); err != nil {
		return err
	}
	if !safeID.MatchString(in.ID) || in.Actor.ID == "" {
		return agent.ErrInvalidInput
	}
	return nil
}

func categoryMutationOperation(operation string) bool {
	switch operation {
	case "site_apply", "site_delete", "site_default_set", "site_config_set", "site_backup_restore":
		return true
	default:
		return false
	}
}

func (p *Provider) recheckMutationCategories(ctx context.Context, actor core.Actor, operation string, in request) error {
	switch operation {
	case "site_apply":
		if in.Site == nil {
			return invalid("Website task is missing its persisted target settings")
		}
		return p.authorizeCategory(ctx, actor, in.Site.CategoryID)
	case "site_delete", "site_default_set", "site_config_set":
		// These operations preserve or remove the current category assignment.
		// The shared gate keeps category deletion from racing their commit/rollback;
		// they do not newly depend on access to a category the caller did not target.
		return ctx.Err()
	case "site_backup_restore":
		_, err := p.authorizedRestore(ctx, actor, in)
		return err
	default:
		return nil
	}
}

func (p *Provider) Run(ctx context.Context, job agent.Request) (agent.Result, error) {
	actor, err := p.deps.ResolveActor(ctx, core.Actor{ID: job.ActorID})
	if err != nil {
		return agent.Result{}, err
	}
	var in request
	if err = strict(job.Input, &in); err != nil {
		return agent.Result{}, err
	}
	if in.Actor.ID != actor.ID {
		return agent.Result{}, denied()
	}
	var restoreSettings *backupManifest
	if categoryMutationOperation(job.Operation) {
		release, lockErr := p.lockCategoryMutations(ctx)
		if lockErr != nil {
			return agent.Result{}, lockErr
		}
		defer release()
		actor, err = p.deps.ResolveActor(ctx, core.Actor{ID: job.ActorID})
		if err != nil {
			return agent.Result{}, err
		}
		if in.Actor.ID != actor.ID {
			return agent.Result{}, denied()
		}
		if job.Operation == "site_backup_restore" {
			settings, restoreErr := p.authorizedRestore(ctx, actor, in)
			if restoreErr != nil {
				return agent.Result{}, restoreErr
			}
			restoreSettings = &settings
		} else if err = p.recheckMutationCategories(ctx, actor, job.Operation, in); err != nil {
			return agent.Result{}, err
		}
	}
	if job.Operation == "site_default_set" {
		if _, err = p.defaultResources(ctx, actor, encode(in)); err != nil {
			return agent.Result{}, err
		}
	}
	if job.Operation == "site_config_set" && !p.deps.Authorize(actor, "terminal:root", "host:local") {
		return agent.Result{}, denied()
	}
	if err = p.resolveACMEJob(ctx, actor, &in); err != nil {
		return agent.Result{}, err
	}
	if job.Operation == "site_backup_restore" {
		settings := *restoreSettings
		if settings.Site.Protection.Enabled {
			secret, err := p.deps.Secrets.Get(ctx, actor, Resource(in.ID), settings.Site.Protection.PasswordRef)
			if err != nil {
				return agent.Result{}, err
			}
			in.Password = string(secret)
		}
	}
	if in.Site != nil {
		for _, id := range []string{in.Site.WebInstance, in.Site.PHPInstance} {
			if id != "" && !p.deps.Authorize(actor, "software:read", "software:"+id) {
				return agent.Result{}, denied()
			}
		}
	}
	if in.Site != nil && in.Site.Protection.Enabled {
		secret, err := p.deps.Secrets.Get(ctx, actor, Resource(in.ID), in.Site.Protection.PasswordRef)
		if err != nil {
			return agent.Result{}, err
		}
		in.Password = string(secret)
	}
	if p.options.Agent == nil {
		return agent.Result{}, failure("agent_unavailable", "Privileged website Agent is not configured", 503)
	}
	result, err := p.options.Agent.Run(ctx, agent.Request{Operation: job.Operation, ActorID: actor.ID, JobID: job.JobID, Input: encode(in), Output: job.Output})
	return result, providerError(err)
}
func (p *Provider) authorizedRestore(ctx context.Context, actor core.Actor, in request) (backupManifest, error) {
	var settings backupManifest
	if !p.deps.Authorize(actor, "sites:write", Resource(in.ID)) {
		return settings, denied()
	}
	if err := p.call(ctx, actor, "site_backup_get", in, &settings); err != nil {
		return settings, err
	}
	if settings.Site.ID != in.ID {
		return settings, failure("backup_invalid", "Backup belongs to another website", 409)
	}
	if err := p.authorizeCategory(ctx, actor, settings.Site.CategoryID); err != nil {
		return settings, err
	}
	for _, id := range []string{settings.Site.WebInstance, settings.Site.PHPInstance} {
		if id != "" && !p.deps.Authorize(actor, "software:read", "software:"+id) {
			return settings, denied()
		}
	}
	if settings.Site.Default && !p.deps.Authorize(actor, "sites:write", "host:local") {
		return settings, denied()
	}
	return settings, nil
}
func (p *Provider) restoreResources(ctx context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
	resources, err := siteResource(ctx, actor, raw)
	if err != nil {
		return nil, err
	}
	if !p.deps.Authorize(actor, "sites:backup", resources[0]) {
		return nil, denied()
	}
	var in request
	_ = json.Unmarshal(raw, &in)
	settings, err := p.authorizedRestore(ctx, actor, in)
	if err != nil {
		return nil, err
	}
	if settings.Site.CategoryID != "" {
		resources = appendResource(resources, "site-category:"+settings.Site.CategoryID)
	}
	for _, id := range []string{settings.Site.WebInstance, settings.Site.PHPInstance} {
		if id != "" {
			resources = appendResource(resources, "software:"+id)
		}
	}
	if settings.Site.Default {
		resources = appendResource(resources, "host:local")
	}
	return resources, nil
}
