package plugins

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
	"github.com/redis/go-redis/v9"
)

type CacheConnection struct {
	ID            string `json:"id"`
	Engine        string `json:"engine"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username,omitempty"`
	CredentialRef string `json:"credential_ref,omitempty"`
	TLS           bool   `json:"tls"`
	Database      int    `json:"database"`
	Prefix        string `json:"prefix"`
	Revision      int64  `json:"revision"`
	Endpoint      string `json:"endpoint,omitempty"`
	Resource      string `json:"resource,omitempty"`
}
type CacheRegisterInput struct {
	CacheConnection
	Password         string `json:"password,omitempty"`
	ExpectedRevision int64  `json:"expected_revision"`
}
type cacheInput struct {
	ID      string            `json:"id"`
	Key     string            `json:"key,omitempty"`
	Value   string            `json:"value,omitempty"`
	Keys    []string          `json:"keys,omitempty"`
	TTL     int64             `json:"ttl_seconds,omitempty"`
	Cursor  cacheCursor       `json:"cursor,omitempty"`
	Limit   int64             `json:"limit,omitempty"`
	Fields  map[string]string `json:"fields,omitempty"`
	Members []string          `json:"members,omitempty"`
	Start   int64             `json:"start,omitempty"`
	Stop    int64             `json:"stop,omitempty"`
	Side    string            `json:"side,omitempty"`
}

var cachePrefix = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_:.-]{0,63}$`)
var cacheKey = regexp.MustCompile(`^[!-~]{1,240}$`)

func CacheResource(p CacheConnection) string {
	hash := sha256.Sum256([]byte(strconv.Itoa(p.Database) + "|" + p.Prefix))
	return "cache:" + p.ID + ":keys:" + hex.EncodeToString(hash[:8])
}
func publicCacheConnection(p CacheConnection) CacheConnection {
	p.CredentialRef = ""
	p.Resource = CacheResource(p)
	return p
}
func (m *Manager) cacheRegisterResources(ctx context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in CacheRegisterInput
	if e := parse(raw, &in); e != nil {
		return nil, e
	}
	if !validInstanceID(in.ID) || !cachePrefix.MatchString(in.Prefix) {
		return nil, failure("invalid_input", "Cache ID/prefix is invalid", 400)
	}
	resources := []string{"cache:" + in.ID, CacheResource(in.CacheConnection)}
	old, err := m.cacheProfile(ctx, in.ID)
	if err == nil {
		resources = append(resources, CacheResource(old))
	} else if core.ErrorCode(err) != "not_found" {
		return nil, err
	}
	return resources, nil
}
func (m *Manager) cacheProfile(ctx context.Context, id string) (CacheConnection, error) {
	var p CacheConnection
	raw, revision, e := m.deps.State.GetVersioned(ctx, "cache_connections", id)
	if e != nil {
		return p, e
	}
	if json.Unmarshal(raw, &p) != nil {
		return p, failure("invalid_state", "Cache connection profile is corrupt", 500)
	}
	p.Revision = revision
	return p, nil
}
func (m *Manager) cacheResources(ctx context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	var in IDInput
	if json.Unmarshal(raw, &in) != nil || !validInstanceID(in.ID) {
		return nil, failure("invalid_input", "Invalid cache ID", 400)
	}
	p, e := m.cacheProfile(ctx, in.ID)
	if e != nil {
		return nil, e
	}
	return []string{CacheResource(p)}, nil
}
func cacheKeySchema() map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": 240, "pattern": cacheKey.String()}
}
func cacheValueSchema() map[string]any { return map[string]any{"type": "string", "maxLength": 32768} }
func (m *Manager) cacheBindings() []core.Binding {
	id := obj(map[string]any{"id": idSchema()}, "id")
	key := obj(map[string]any{"id": idSchema(), "key": cacheKeySchema()}, "id", "key")
	bindings := []core.Binding{
		m.binding("cache_connections_list", "List only authorized persistent cache namespaces", "cache:read", true, obj(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, m.cacheList),
		m.binding("cache_connection_register", "Probe and pin a cache peer; reject overlapping namespaces and store only bound credentials", "cache:admin", false, obj(map[string]any{"id": idSchema(), "engine": enumSchema("redis", "valkey", "memcached"), "host": textSchema(), "port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "username": map[string]any{"type": "string", "maxLength": 128}, "password": map[string]any{"type": "string", "maxLength": 4096, "format": "password"}, "tls": map[string]any{"type": "boolean"}, "database": map[string]any{"type": "integer", "minimum": 0, "maximum": 1023}, "prefix": map[string]any{"type": "string", "pattern": cachePrefix.String()}, "expected_revision": revisionSchema()}, "id", "engine", "host", "port", "database", "prefix", "expected_revision"), m.cacheRegisterResources, m.cacheRegister),
		m.binding("cache_connection_get", "Read one cache profile without plaintext credentials", "cache:read", true, id, m.cacheResources, m.cacheGetConnection),
		m.binding("cache_connection_delete", "Delete a versioned cache connection and its credential while retaining actual cache data", "cache:admin", false, obj(map[string]any{"id": idSchema(), "expected_revision": revisionSchema()}, "id", "expected_revision"), m.cacheResources, m.cacheDeleteConnection),
		m.binding("cache_status", "Probe actual namespace cache connectivity without exposing credentials", "cache:read", true, id, m.cacheResources, m.cacheStatus),
		m.binding("cache_key_get", "Read one key strictly under the authorized namespace", "cache:read", true, key, m.cacheResources, m.cacheGet),
		m.binding("cache_key_set", "Write one bounded namespace value and TTL", "cache:write", false, obj(map[string]any{"id": idSchema(), "key": cacheKeySchema(), "value": cacheValueSchema(), "ttl_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 2592000}}, "id", "key", "value"), m.cacheResources, m.cacheSet),
		m.binding("cache_keys_delete", "Delete explicit keys in one authorized namespace", "cache:write", false, obj(map[string]any{"id": idSchema(), "keys": map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "uniqueItems": true, "items": cacheKeySchema()}}, "id", "keys"), m.cacheResources, m.cacheDelete),
		m.binding("cache_keys_scan", "Incrementally scan Redis/Valkey keys using an exact decimal-string cursor and a fixed namespace match", "cache:read", true, obj(map[string]any{"id": idSchema(), "cursor": cacheCursorSchema(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "id"), m.cacheResources, m.cacheScan),
		m.binding("cache_key_type", "Read Redis/Valkey key type and TTL within its namespace", "cache:read", true, key, m.cacheResources, m.cacheType),
	}
	return append(bindings, m.cacheCollectionBindings()...)
}
func (m *Manager) cacheList(ctx context.Context, a core.Actor, _ json.RawMessage) (core.ActionResult, error) {
	records, e := m.deps.State.List(ctx, "cache_connections")
	if e != nil {
		return core.ActionResult{}, e
	}
	items := []CacheConnection{}
	for _, raw := range records {
		var p CacheConnection
		if json.Unmarshal(raw, &p) == nil && m.deps.Authorize(a, "cache:read", CacheResource(p)) {
			items = append(items, publicCacheConnection(p))
		}
	}
	return core.ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
}
func (m *Manager) cacheRegister(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in CacheRegisterInput
	if e := parse(raw, &in); e != nil {
		return core.ActionResult{}, e
	}
	p := in.CacheConnection
	declared, err := m.cacheRegisterResources(ctx, a, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	for _, resource := range declared {
		if !m.deps.Authorize(a, "cache:admin", resource) {
			return core.ActionResult{}, failure("forbidden", "Current and replacement cache namespaces must both be authorized", 403)
		}
	}
	previous, previousErr := m.cacheProfile(ctx, p.ID)
	if previousErr == nil && previous.Revision != in.ExpectedRevision || previousErr != nil && in.ExpectedRevision != 0 {
		return core.ActionResult{}, failure("revision_conflict", "Cache profile changed; reload before updating", 412)
	}
	if !validInstanceID(p.ID) || !cachePrefix.MatchString(p.Prefix) || p.Port < 1 || p.Port > 65535 || p.Database < 0 || p.Database > 1023 || p.CredentialRef != "" || p.Revision != 0 || p.Endpoint != "" || p.Resource != "" {
		return core.ActionResult{}, failure("invalid_input", "Invalid cache profile", 400)
	}
	if !p.TLS && !localDatabaseHost(p.Host) {
		return core.ActionResult{}, failure("tls_required", "Remote caches require verified TLS", 400)
	}
	if p.Engine != "redis" && p.Engine != "valkey" && p.Engine != "memcached" {
		return core.ActionResult{}, failure("invalid_input", "Unsupported cache engine", 400)
	}
	probe, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if p.Engine == "memcached" {
		if p.Username != "" || in.Password != "" || p.Database != 0 {
			return core.ActionResult{}, failure("unsupported_authentication", "Memcached ASCII adapter cannot accept SASL credentials or database indexes", 400)
		}
		endpoint, err := probeMemcacheEndpoint(probe, p)
		if err != nil {
			return core.ActionResult{}, err
		}
		p.Endpoint = endpoint
	} else {
		var peer string
		var peerMu sync.Mutex
		client := newRedisProfileWithPeer(p, in.Password, func(endpoint string) { peerMu.Lock(); peer = endpoint; peerMu.Unlock() })
		defer client.Close()
		if e := client.Ping(probe).Err(); e != nil {
			return core.ActionResult{}, failure("connection_failed", "Redis/Valkey TLS, authentication or connection failed", 502)
		}
		peerMu.Lock()
		p.Endpoint = peer
		peerMu.Unlock()
		if p.Endpoint == "" {
			return core.ActionResult{}, failure("connection_failed", "Cache peer identity could not be established", 502)
		}
	}
	if in.Password != "" {
		ref, e := m.deps.Secrets.Put(ctx, a, CacheResource(p), "cache credential", []byte(in.Password))
		if e != nil {
			return core.ActionResult{}, e
		}
		p.CredentialRef = ref
	}
	p.Revision = in.ExpectedRevision + 1
	encoded, _ := json.Marshal(p)
	var revision int64
	e := m.deps.State.Transaction(ctx, func(tx core.MetadataStore) error {
		records, err := tx.List(ctx, "cache_connections")
		if err != nil {
			return err
		}
		for id, raw := range records {
			if id == p.ID {
				continue
			}
			var existing CacheConnection
			if json.Unmarshal(raw, &existing) != nil {
				return failure("invalid_state", "Cache profile collection is corrupt", 500)
			}
			if existing.Database != p.Database {
				continue
			}
			samePeer := canonicalCacheEndpoint(existing.Endpoint) == canonicalCacheEndpoint(p.Endpoint) && existing.Endpoint != ""
			if existing.Endpoint == "" {
				samePeer = cacheHostIdentity(existing.Host) == cacheHostIdentity(p.Host) && existing.Port == p.Port
			}
			if samePeer && (strings.HasPrefix(existing.Prefix+":", p.Prefix+":") || strings.HasPrefix(p.Prefix+":", existing.Prefix+":")) {
				return failure("namespace_conflict", "Cache namespaces on the same verified peer cannot overlap, including connection aliases", 409)
			}
		}
		revision, err = tx.CompareAndSwap(ctx, "cache_connections", p.ID, in.ExpectedRevision, encoded)
		return err
	})
	if e != nil {
		if p.CredentialRef != "" {
			_ = m.deps.Secrets.Delete(ctx, a, CacheResource(p), p.CredentialRef)
		}
		return core.ActionResult{}, e
	}
	p.Revision = revision
	if previousErr == nil && previous.CredentialRef != "" {
		_ = m.deps.Secrets.Delete(ctx, a, CacheResource(previous), previous.CredentialRef)
	}
	return core.ActionResult{Data: publicCacheConnection(p), Resources: declared, AuditSummary: map[string]any{"connection_id": p.ID, "engine": p.Engine, "namespace": p.Prefix}}, nil
}
func newRedisProfile(p CacheConnection, password string) *redis.Client {
	return newRedisProfileWithPeer(p, password, nil)
}
func newRedisProfileWithPeer(p CacheConnection, password string, recordPeer func(string)) *redis.Client {
	o := &redis.Options{Addr: net.JoinHostPort(p.Host, strconv.Itoa(p.Port)), Username: p.Username, Password: password, DB: p.Database, DialTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, PoolSize: 2, MaxRetries: -1}
	if p.TLS {
		o.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: p.Host}
	}
	o.Protocol = 2
	dial := redis.NewDialer(o)
	o.Dialer = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		peer := canonicalCacheEndpoint(conn.RemoteAddr().String())
		if p.Endpoint != "" && peer != canonicalCacheEndpoint(p.Endpoint) {
			conn.Close()
			return nil, failure("connection_changed", "Cache peer changed; an administrator must revalidate its namespace binding", 409)
		}
		if recordPeer != nil {
			recordPeer(peer)
		}
		return &boundedRedisConn{Conn: conn}, nil
	}
	return redis.NewClient(o)
}
func canonicalCacheEndpoint(value string) string {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return value
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, port)
}
func cacheHostIdentity(host string) string {
	if localDatabaseHost(host) {
		return "loopback"
	}
	return strings.ToLower(host)
}

func (m *Manager) cacheGetConnection(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in IDInput
	if err := parse(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	p, err := m.cacheProfile(ctx, in.ID)
	return cacheResult(p, publicCacheConnection(p)), err
}
func (m *Manager) cacheDeleteConnection(ctx context.Context, a core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	var in struct {
		ID       string `json:"id"`
		Expected int64  `json:"expected_revision"`
	}
	if err := parse(raw, &in); err != nil {
		return core.ActionResult{}, err
	}
	p, err := m.cacheProfile(ctx, in.ID)
	if err != nil {
		return core.ActionResult{}, err
	}
	err = m.deps.State.Transaction(ctx, func(tx core.MetadataStore) error {
		_, revision, err := tx.GetVersioned(ctx, "cache_connections", p.ID)
		if err != nil {
			return err
		}
		if revision != in.Expected {
			return failure("revision_conflict", "Cache connection changed; reload before deleting", 412)
		}
		return tx.Delete(ctx, "cache_connections", p.ID)
	})
	if err != nil {
		return core.ActionResult{}, err
	}
	if p.CredentialRef != "" {
		if err = m.deps.Secrets.Delete(ctx, a, CacheResource(p), p.CredentialRef); err != nil {
			return core.ActionResult{}, err
		}
	}
	return cacheResult(p, map[string]any{"id": p.ID, "deleted": true, "cache_data_retained": true}), nil
}
func (m *Manager) cacheClient(ctx context.Context, a core.Actor, p CacheConnection) (*redis.Client, error) {
	if p.Engine == "memcached" {
		return nil, failure("unsupported_operation", "Operation requires Redis or Valkey", 400)
	}
	password := ""
	if p.CredentialRef != "" {
		secret, e := m.deps.Secrets.Get(ctx, a, CacheResource(p), p.CredentialRef)
		if e != nil {
			return nil, e
		}
		password = string(secret)
		for i := range secret {
			secret[i] = 0
		}
	}
	return newRedisProfile(p, password), nil
}
