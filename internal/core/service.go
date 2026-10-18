package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/jobs"

	_ "github.com/ncruces/go-sqlite3/driver"
)

type Service struct {
	db                 *sql.DB
	dataDir            string
	secret             []byte
	mu                 sync.Mutex
	subs               map[uint64]subscription
	nextSub            uint64
	closed             bool
	done               chan struct{}
	lifetime           context.Context
	cancelLifetime     context.CancelFunc
	closeOnce          sync.Once
	closeErr           error
	secretJanitorDone  chan struct{}
	status             *statusSampler
	jobs               *jobs.Manager
	filesystem         *agent.Filesystem
	registryMu         sync.RWMutex
	providers          map[string]Binding
	extraScopes        map[string]bool
	registrySealed     bool
	executors          *executorRegistry
	legacyFiles        LegacyFileDelegate
	idempotencyMu      sync.Mutex
	idempotencyFlights map[string]*idempotencyFlight
}
type subscription struct {
	actor   Actor
	channel chan Event
}

func New(dataDir string) (*Service, error) {
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if abs == "/" || abs == "/tmp" || abs == "/workspace" {
		return nil, errors.New("choose a dedicated private Folio data directory")
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dataDir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("data directory cannot be a symlink")
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Folio data directory must be private (0700)")
	}
	key, err := loadOrCreateSecret(filepath.Join(abs, "encryption-key"), 32)
	if err != nil {
		return nil, err
	}
	dbpath := filepath.Join(abs, "powerlinux.db")
	if existing, e := os.Lstat(dbpath); e == nil && existing.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("database cannot be a symlink")
	}
	uri := url.URL{Scheme: "file", Path: dbpath}
	uri.RawQuery = "_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Service{db: db, dataDir: abs, secret: key, subs: make(map[uint64]subscription), done: make(chan struct{}), providers: map[string]Binding{}, extraScopes: map[string]bool{}, executors: newExecutorRegistry(), idempotencyFlights: map[string]*idempotencyFlight{}}
	s.executors.service = s
	if err = s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.migrateIdempotencyHashes(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err = s.expireProviderSecrets(context.Background(), time.Now()); err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(dbpath, 0600); err != nil {
		db.Close()
		return nil, err
	}
	if !s.Initialized(context.Background()) {
		if _, err = loadOrCreateSecret(s.SetupSecretFile(), 32); err != nil {
			db.Close()
			return nil, err
		}
	}
	workspace := filepath.Join(abs, "workspace")
	if err = os.MkdirAll(workspace, 0700); err != nil {
		db.Close()
		return nil, err
	}
	workspaceInfo, err := os.Lstat(workspace)
	if err != nil || !workspaceInfo.IsDir() || workspaceInfo.Mode()&os.ModeSymlink != 0 || workspaceInfo.Mode().Perm()&0077 != 0 {
		db.Close()
		return nil, errors.New("workspace must be a private real directory inside Folio data")
	}
	fs, err := agent.NewFilesystem([]agent.RootSpec{{ID: "workspace", Path: workspace}}, 8<<20)
	if err != nil {
		db.Close()
		return nil, err
	}
	s.filesystem = fs
	if err = s.executors.register([]string{"file_write", "file_backup", "file_hash"}, agent.NewFileExecutor(fs)); err != nil {
		fs.Close()
		db.Close()
		return nil, err
	}
	s.lifetime, s.cancelLifetime = context.WithCancel(context.Background())
	manager, err := jobs.NewManager(s.lifetime, s.executors, sqliteJobStore{s: s})
	if err != nil {
		s.cancelLifetime()
		fs.Close()
		db.Close()
		return nil, err
	}
	s.jobs = manager
	s.status = newStatusSampler()
	if err = s.registerIdentityBindings(); err != nil {
		s.Close()
		return nil, err
	}
	s.secretJanitorDone = make(chan struct{})
	go s.runSecretJanitor()
	return s, nil
}

func loadOrCreateSecret(path string, n int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("secret file must be regular and private (0600)")
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		raw, e := base64.RawURLEncoding.DecodeString(strings.TrimSpace(string(b)))
		if e != nil || len(raw) != n {
			return nil, errors.New("invalid secret file")
		}
		return raw, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	raw := make([]byte, n)
	if _, err = rand.Read(raw); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	encoded := []byte(base64.RawURLEncoding.EncodeToString(raw) + "\n")
	n, writeErr := f.Write(encoded)
	if writeErr == nil && n != len(encoded) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	syncErr := dir.Sync()
	closeErr = dir.Close()
	if err = errors.Join(syncErr, closeErr); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *Service) migrate() error {
	_, err := s.db.Exec(`PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY,username TEXT UNIQUE NOT NULL,password_hash TEXT NOT NULL,totp_secret TEXT NOT NULL DEFAULT '',totp_pending TEXT NOT NULL DEFAULT '',totp_last_counter INTEGER NOT NULL DEFAULT -1,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(hash TEXT PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id),csrf TEXT NOT NULL,expires INTEGER NOT NULL,created INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS tokens(id TEXT PRIMARY KEY,hash TEXT UNIQUE NOT NULL,owner_id TEXT NOT NULL REFERENCES users(id),name TEXT NOT NULL,kind TEXT NOT NULL,scopes TEXT NOT NULL,resources TEXT NOT NULL,created INTEGER NOT NULL,expires INTEGER NOT NULL,last_used INTEGER NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0,audience TEXT NOT NULL DEFAULT '',parent_id TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS login_attempts(key TEXT PRIMARY KEY,failures INTEGER NOT NULL,window_start INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS notes(id TEXT PRIMARY KEY,title TEXT NOT NULL,content TEXT NOT NULL,pinned INTEGER NOT NULL,archived INTEGER NOT NULL,target_id TEXT NOT NULL,revision INTEGER NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,created_by TEXT NOT NULL,updated_by TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS note_history(note_id TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,revision INTEGER NOT NULL,snapshot TEXT NOT NULL,PRIMARY KEY(note_id,revision));
CREATE TABLE IF NOT EXISTS audit(id TEXT PRIMARY KEY,action TEXT NOT NULL,actor TEXT NOT NULL,resource TEXT NOT NULL,status TEXT NOT NULL,error_code TEXT NOT NULL,summary TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS audit_time ON audit(created_at DESC);
CREATE INDEX IF NOT EXISTS notes_target ON notes(target_id,archived,pinned,updated_at);
CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY,actor TEXT NOT NULL,resource TEXT NOT NULL,state TEXT NOT NULL,payload TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS provider_state(namespace TEXT NOT NULL,kind TEXT NOT NULL,id TEXT NOT NULL,payload TEXT NOT NULL,revision INTEGER NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(namespace,kind,id));
CREATE TABLE IF NOT EXISTS provider_secrets(ref TEXT PRIMARY KEY,namespace TEXT NOT NULL,resource TEXT NOT NULL,name TEXT NOT NULL,ciphertext TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS provider_secret_expiry(ref TEXT PRIMARY KEY REFERENCES provider_secrets(ref) ON DELETE CASCADE,expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS provider_secret_expiry_time ON provider_secret_expiry(expires_at);
CREATE TABLE IF NOT EXISTS task_metadata(id TEXT PRIMARY KEY REFERENCES tasks(id),actor TEXT NOT NULL,resources TEXT NOT NULL,action TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS idempotency(id TEXT PRIMARY KEY,actor_id TEXT NOT NULL,action TEXT NOT NULL,input_hash TEXT NOT NULL,state TEXT NOT NULL,resources TEXT NOT NULL,result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',task_id TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS identity_settings(id TEXT PRIMARY KEY,payload TEXT NOT NULL,revision INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS applications(id TEXT PRIMARY KEY,owner_id TEXT NOT NULL REFERENCES users(id),name TEXT NOT NULL,secret_hash TEXT NOT NULL,scopes TEXT NOT NULL,resources TEXT NOT NULL,cidrs TEXT NOT NULL,enabled INTEGER NOT NULL,revision INTEGER NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS temporary_grants(id TEXT PRIMARY KEY,owner_id TEXT NOT NULL REFERENCES users(id),name TEXT NOT NULL,hash TEXT UNIQUE NOT NULL,scopes TEXT NOT NULL,resources TEXT NOT NULL,expires INTEGER NOT NULL,session_expires INTEGER NOT NULL,consumed INTEGER NOT NULL DEFAULT 0,revoked INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,consumed_at TEXT NOT NULL DEFAULT '',source TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS captcha_challenges(id TEXT PRIMARY KEY,answer_hash TEXT NOT NULL,source TEXT NOT NULL,expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS basic_gateway(id TEXT PRIMARY KEY,username TEXT NOT NULL,password_hash TEXT NOT NULL,enabled INTEGER NOT NULL,revision INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS gateway_sessions(hash TEXT PRIMARY KEY,revision INTEGER NOT NULL,expires INTEGER NOT NULL);
PRAGMA user_version=1;`)
	if err != nil {
		return err
	}
	// Early development databases predate audience/delegation metadata. Preserve
	// existing identities while adding columns; never reset initialized data.
	rows, err := s.db.Query("PRAGMA table_info(tokens)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primary int
		var name, kind string
		var defaultValue any
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, name := range []string{"audience", "parent_id"} {
		if !columns[name] {
			if _, err = s.db.Exec("ALTER TABLE tokens ADD COLUMN " + name + " TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
	}
	for table, column := range map[string]string{"tokens": "application_id", "sessions": "grant_id"} {
		rows, e := s.db.Query("PRAGMA table_info(" + table + ")")
		if e != nil {
			return e
		}
		found := false
		for rows.Next() {
			var cid, nonnull, pk int
			var name, kind string
			var def any
			if e = rows.Scan(&cid, &name, &kind, &nonnull, &def, &pk); e != nil {
				rows.Close()
				return e
			}
			if name == column {
				found = true
			}
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		if !found {
			if _, e = s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " TEXT NOT NULL DEFAULT ''"); e != nil {
				return e
			}
		}
	}
	_, err = s.db.Exec("UPDATE idempotency SET state='uncertain' WHERE state='running'; PRAGMA user_version=4")
	return err
}

func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		if s.cancelLifetime != nil {
			s.cancelLifetime()
		}
		if s.secretJanitorDone != nil {
			<-s.secretJanitorDone
		}
		if s.jobs != nil {
			_ = s.jobs.Close()
		}
		if s.filesystem != nil {
			_ = s.filesystem.Close()
		}
		if s.status != nil {
			s.status.close()
		}
		s.mu.Lock()
		s.closed = true
		close(s.done)
		for id, sub := range s.subs {
			close(sub.channel)
			delete(s.subs, id)
		}
		s.mu.Unlock()
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}
func (s *Service) SetupSecretFile() string { return filepath.Join(s.dataDir, "bootstrap-token") }
func (s *Service) Initialized(ctx context.Context) bool {
	var n int
	return s.db.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n) == nil && n > 0
}
func randomID(prefix string) string {
	b := make([]byte, 24)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}
func digest(raw string) string { v := sha256.Sum256([]byte(raw)); return hex.EncodeToString(v[:]) }
func nowString() string        { return time.Now().UTC().Format(time.RFC3339Nano) }
func unixNow() int64           { return time.Now().Unix() }
func jsonText(v any) string    { b, _ := json.Marshal(v); return string(b) }

func hasScope(a Actor, scope string) bool {
	for _, v := range a.Scopes {
		if v == "*" || v == scope {
			return true
		}
	}
	return false
}
func hasResource(a Actor, resource string) bool {
	for _, v := range a.Resources {
		for _, candidate := range resourceAliases(resource) {
			if v == "*" || v == candidate {
				return true
			}
			if strings.HasSuffix(v, ":*") && strings.HasPrefix(candidate, strings.TrimSuffix(v, "*")) {
				return true
			}
		}
	}
	return false
}
func (s *Service) Authorized(a Actor, scope, resource string) bool {
	return a.ID != "" && hasScope(a, scope) && (resource == "" || hasResource(a, resource))
}

func (s *Service) Subscribe(ctx context.Context, a Actor) <-chan Event {
	c := make(chan Event, 16)
	s.mu.Lock()
	if s.closed {
		close(c)
		s.mu.Unlock()
		return c
	}
	s.nextSub++
	id := s.nextSub
	s.subs[id] = subscription{a, c}
	s.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
		case <-s.done:
		}
		s.mu.Lock()
		if sub, ok := s.subs[id]; ok {
			delete(s.subs, id)
			close(sub.channel)
		}
		s.mu.Unlock()
	}()
	return c
}
func (s *Service) publish(ev Event, scope, resource string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.subs {
		if s.Authorized(sub.actor, scope, resource) {
			select {
			case sub.channel <- ev:
			default: // A bounded channel intentionally coalesces invalidation; next GET is authoritative.
			}
		}
	}
}

func (s *Service) recordAudit(ctx context.Context, actor Actor, action, resource string, result error, summary map[string]any) {
	status, code := "succeeded", ""
	if _, ok := summary["task_id"]; ok {
		status = "submitted"
	}
	if result != nil {
		status = "failed"
		code = ErrorCode(result)
	}
	a := Audit{ID: randomID("audit_"), Action: action, Actor: actor.Public(), Resource: resource, Status: status, ErrorCode: code, Summary: summary, CreatedAt: nowString()}
	s.persistAudit(ctx, a)
}
func (s *Service) persistAudit(ctx context.Context, a Audit) {
	// Centralize the same protection for authentication, jobs and direct
	// bookkeeping as for Provider summaries.
	a.Summary = safeSummary(a.Summary)
	// Auditing uses its own bounded context so canceled operations still leave evidence.
	ac, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ac, "INSERT INTO audit(id,action,actor,resource,status,error_code,summary,created_at) VALUES(?,?,?,?,?,?,?,?)", a.ID, a.Action, jsonText(a.Actor), a.Resource, a.Status, a.ErrorCode, jsonText(a.Summary), a.CreatedAt)
	if err == nil {
		s.publish(Event{Type: "audit_changed", Data: map[string]any{"id": a.ID}}, "audit:read", a.Resource)
	}
}
