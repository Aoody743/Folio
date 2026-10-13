package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

// Explicitly enabled integration suite: credentials stay in a protected local
// file; normal unit runs do not depend on or alter an unrelated database.
func liveDriverFixture(t *testing.T) (*core.Service, core.Actor, *Manager, map[string]json.RawMessage) {
	t.Helper()
	path := os.Getenv("POWERLINUX_DB_FIXTURES")
	if path == "" {
		t.Skip("set POWERLINUX_DB_FIXTURES for the isolated real-service suite")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read protected database fixture file")
	}
	var fixtures map[string]json.RawMessage
	if json.Unmarshal(raw, &fixtures) != nil {
		t.Fatal("invalid database fixture metadata")
	}
	if mongoPath := os.Getenv("POWERLINUX_MONGO_FIXTURES"); mongoPath != "" {
		mongoRaw, err := os.ReadFile(mongoPath)
		if err != nil || json.Unmarshal(mongoRaw, &fixtures) != nil {
			t.Fatal("cannot read protected MongoDB fixture metadata")
		}
	}
	s, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	secret, err := os.ReadFile(s.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "isolated driver fixture password")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(s.Runtime("plugins"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(manager); err != nil {
		t.Fatal(err)
	}
	return s, a, manager, fixtures
}
func liveAction(t *testing.T, s *core.Service, a core.Actor, name string, input any) any {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.Execute(context.Background(), a, name, raw)
	if err != nil {
		t.Fatalf("%s: %s (status %d)", name, core.ErrorCode(err), core.ErrorStatus(err))
	}
	return data
}
func TestLiveSQLScopedProfilesCRUDAndAdminDenial(t *testing.T) {
	s, a, _, fixtures := liveDriverFixture(t)
	for _, engine := range []string{"mariadb", "postgresql"} {
		t.Run(engine, func(t *testing.T) {
			var in DatabaseRegisterInput
			if json.Unmarshal(fixtures[engine], &in) != nil {
				t.Fatal("missing SQL fixture")
			}
			in.Name = "Isolated " + engine
			in.ExpectedRevision = 0
			profile := liveAction(t, s, a, "database_connection_register", in).(DatabaseConnection)
			token, err := s.CreateToken(context.Background(), a, core.TokenRequest{Name: "scoped database QA", Scopes: []string{"databases:read", "databases:query"}, Resources: []string{databaseProfileResource(profile)}})
			if err != nil {
				t.Fatal(err)
			}
			actor, err := s.AuthenticateToken(context.Background(), token.Token)
			if err != nil {
				t.Fatal(err)
			}
			table := "qa_records"
			quote := quoteDatabaseIdentifier(engine, table)
			base := 1_200_000_000 + int(time.Now().UnixNano()%100_000_000)
			defer func() {
				raw, _ := json.Marshal(DatabaseQueryInput{ID: profile.ID, SQL: "DELETE FROM " + quote + " WHERE id IN (" + strconv.Itoa(base+1) + "," + strconv.Itoa(base+2) + ")", Mode: "execute"})
				s.Execute(context.Background(), actor, "database_query", raw)
			}()
			placeholders := "?,?"
			if engine == "postgresql" {
				placeholders = "$1,$2"
			}
			for i := 1; i <= 2; i++ {
				liveAction(t, s, actor, "database_query", DatabaseQueryInput{ID: profile.ID, SQL: "INSERT INTO " + quote + " (id,label) VALUES (" + placeholders + ")", Parameters: []any{base + i, "actual driver value"}, Mode: "execute"})
			}
			result := liveAction(t, s, actor, "database_query", DatabaseQueryInput{ID: profile.ID, SQL: "SELECT id,label FROM " + quote + " WHERE id IN (" + strconv.Itoa(base+1) + "," + strconv.Itoa(base+2) + ") ORDER BY id", Limit: 1}).(map[string]any)
			items := result["items"].([]map[string]any)
			if len(items) != 1 || items[0]["label"] != "actual driver value" || result["truncated"] != true {
				t.Fatal("real SQL result/pagination mismatch")
			}
			if status := liveAction(t, s, actor, "database_status", IDInput{ID: profile.ID}).(map[string]any); status["connected"] != true {
				t.Fatal("actual database status failed")
			}
			if engine == "postgresql" {
				wireValues := liveAction(t, s, actor, "database_query", DatabaseQueryInput{ID: profile.ID, SQL: "SELECT 'NaN'::float8 AS special, decode('00ff','hex') AS binary_value"}).(map[string]any)["items"].([]map[string]any)
				if len(wireValues) != 1 || wireValues[0]["special"] != "NaN" || wireValues[0]["binary_value"].(map[string]any)["data"] != "AP8=" {
					t.Fatal("SQL wire values lost their representation")
				}
			}
			adminKey := engine + "_admin"
			var adminIn DatabaseRegisterInput
			if json.Unmarshal(fixtures[adminKey], &adminIn) != nil {
				t.Fatal("missing admin fixture")
			}
			adminIn.Name = "Isolated admin " + engine
			admin := liveAction(t, s, a, "database_connection_register", adminIn).(DatabaseConnection)
			raw, _ := json.Marshal(DatabaseQueryInput{ID: admin.ID, SQL: "SELECT 1"})
			_, err = s.Execute(context.Background(), a, "database_query", raw)
			if core.ErrorCode(err) != "credential_scope" {
				t.Fatal("instance credential accepted for per-database query", core.ErrorCode(err))
			}
			_, err = s.Execute(context.Background(), actor, "database_instance_query", raw)
			if core.ErrorCode(err) != "forbidden" {
				t.Fatal("database-only actor reached instance query", core.ErrorCode(err))
			}
			raw, _ = json.Marshal(DatabaseQueryInput{ID: profile.ID, SQL: map[string]string{"mariadb": "SELECT User FROM mysql.user LIMIT 1", "postgresql": "SELECT rolname FROM pg_authid LIMIT 1"}[engine]})
			_, err = s.Execute(context.Background(), actor, "database_query", raw)
			if core.ErrorCode(err) != "query_failed" {
				t.Fatal("database identity obtained privileged catalog data", core.ErrorCode(err))
			}
		})
	}
}
func strconvTime() string { return time.Now().UTC().Format("150405000000000") }
func TestLivePostgreSQLDangerousMembershipDenied(t *testing.T) {
	s, a, _, fixtures := liveDriverFixture(t)
	var in DatabaseRegisterInput
	if json.Unmarshal(fixtures["postgresql_danger"], &in) != nil {
		t.Fatal("missing dangerous role fixture")
	}
	in.Name = "Dangerous predefined membership fixture"
	raw, _ := json.Marshal(in)
	_, err := s.Execute(context.Background(), a, "database_connection_register", raw)
	if core.ErrorCode(err) != "credential_scope" || core.ErrorStatus(err) != 403 {
		t.Fatal("dangerous predefined role membership was accepted", core.ErrorCode(err))
	}
}
func TestLivePostgreSQLQuotedBackslashPasswordAuthenticatesAndCleansRole(t *testing.T) {
	_, _, _, fixtures := liveDriverFixture(t)
	var input DatabaseRegisterInput
	if json.Unmarshal(fixtures["postgresql_admin"], &input) != nil {
		t.Fatal("missing PostgreSQL admin fixture")
	}
	ctx := context.Background()
	admin, err := openSQLProfile(ctx, DatabaseConnection{Engine: "postgresql", Host: input.Host, Port: input.Port, Username: input.Username}, input.Password, "postgres")
	if err != nil {
		t.Fatal("PG admin fixture unavailable", core.ErrorCode(err))
	}
	defer admin.Close()
	role := "qa_pl_" + strconvTime()
	quoted := quoteDatabaseIdentifier("postgresql", role)
	password := "isolated'quoted\\backslash\\password:fixture"
	if _, err = admin.ExecContext(ctx, "CREATE ROLE "+quoted+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS NOINHERIT PASSWORD "+pgSQLLiteral(password)); err != nil {
		t.Fatal("isolated PG role creation failed")
	}
	defer func() {
		if _, err := admin.ExecContext(context.Background(), "DROP ROLE "+quoted); err != nil {
			t.Error("isolated PG role cleanup failed")
		}
		var remains int
		if err := admin.QueryRowContext(context.Background(), "SELECT count(*) FROM pg_roles WHERE rolname=$1", role).Scan(&remains); err != nil || remains != 0 {
			t.Error("isolated PG role remained after cleanup")
		}
	}()
	profile := DatabaseConnection{Engine: "postgresql", Host: input.Host, Port: input.Port, Username: role, Database: "postgres", Access: "database"}
	for _, password := range []string{password, "rotated'with\\a\\backslash:fixture"} {
		if _, err = admin.ExecContext(ctx, "ALTER ROLE "+quoted+" PASSWORD "+pgSQLLiteral(password)); err != nil {
			t.Fatal("isolated PG password update failed")
		}
		connection, err := openSQLProfile(ctx, profile, password, "postgres")
		if err != nil {
			t.Fatal("quoted/backslash password did not authenticate", core.ErrorCode(err))
		}
		var one int
		err = connection.QueryRowContext(ctx, "SELECT 1").Scan(&one)
		connection.Close()
		if err != nil || one != 1 {
			t.Fatal("actual PG wire query failed")
		}
	}
}
func TestLiveMariaDBWildcardGrantDenied(t *testing.T) {
	s, a, _, fixtures := liveDriverFixture(t)
	var in DatabaseRegisterInput
	if json.Unmarshal(fixtures["mariadb_danger"], &in) != nil {
		t.Fatal("missing wildcard grant fixture")
	}
	in.Name = "Wildcard grant fixture"
	raw, _ := json.Marshal(in)
	_, err := s.Execute(context.Background(), a, "database_connection_register", raw)
	if core.ErrorCode(err) != "credential_scope" || core.ErrorStatus(err) != 403 {
		t.Fatal("database wildcard grant was accepted", core.ErrorCode(err))
	}
}

func cacheRegistration(in CacheRegisterInput) map[string]any {
	encoded, _ := json.Marshal(in)
	var registration map[string]any
	json.Unmarshal(encoded, &registration)
	delete(registration, "revision")
	delete(registration, "credential_ref")
	delete(registration, "endpoint")
	delete(registration, "resource")
	return registration
}
func TestLiveCacheNamespaceAliasAndConcurrentOverlapDenied(t *testing.T) {
	s, a, _, fixtures := liveDriverFixture(t)
	for _, engine := range []string{"redis", "memcached"} {
		t.Run(engine, func(t *testing.T) {
			var in CacheRegisterInput
			if json.Unmarshal(fixtures[engine], &in) != nil {
				t.Fatal("missing cache fixture")
			}
			liveAction(t, s, a, "cache_connection_register", cacheRegistration(in))
			alias := in
			alias.ID += "-alias"
			alias.Host = "localhost"
			alias.Prefix = strings.TrimSuffix(in.Prefix, ":")
			raw, _ := json.Marshal(cacheRegistration(alias))
			_, err := s.Execute(context.Background(), a, "cache_connection_register", raw)
			if core.ErrorCode(err) != "namespace_conflict" || core.ErrorStatus(err) != 409 {
				t.Fatal("overlapping aliased namespace accepted", core.ErrorCode(err))
			}
			// Both probes can succeed, but their conflicting metadata reservations
			// must serialize in the same transaction.
			prefix := "powerlinux_parallel_" + strconvTime()
			var wg sync.WaitGroup
			codes := make(chan string, 2)
			for i := 0; i < 2; i++ {
				candidate := in
				candidate.ID += "-concurrent-" + strconv.Itoa(i)
				candidate.Prefix = prefix
				wg.Add(1)
				go func() {
					defer wg.Done()
					data, _ := json.Marshal(cacheRegistration(candidate))
					_, err := s.Execute(context.Background(), a, "cache_connection_register", data)
					if err == nil {
						codes <- ""
					} else {
						codes <- core.ErrorCode(err)
					}
				}()
			}
			wg.Wait()
			close(codes)
			successes, conflicts := 0, 0
			for code := range codes {
				if code == "" {
					successes++
				} else if code == "namespace_conflict" {
					conflicts++
				} else {
					t.Fatal("unexpected concurrent register result", code)
				}
			}
			if successes != 1 || conflicts != 1 {
				t.Fatal("namespace reservation was not atomic", successes, conflicts)
			}
		})
	}
}
func TestLiveMongoScopedCRUDAndGlobalRoleDenied(t *testing.T) {
	s, a, _, fixtures := liveDriverFixture(t)
	if len(fixtures["mongodb"]) == 0 {
		t.Skip("set POWERLINUX_MONGO_FIXTURES for the isolated MongoDB service")
	}
	var in DatabaseRegisterInput
	if json.Unmarshal(fixtures["mongodb"], &in) != nil {
		t.Fatal("invalid MongoDB fixture")
	}
	in.Name = "Isolated MongoDB"
	profile := liveAction(t, s, a, "database_connection_register", in).(DatabaseConnection)
	token, err := s.CreateToken(context.Background(), a, core.TokenRequest{Name: "scoped MongoDB QA", Scopes: []string{"databases:query", "databases:write"}, Resources: []string{databaseProfileResource(profile)}})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.AuthenticateToken(context.Background(), token.Token)
	if err != nil {
		t.Fatal(err)
	}
	marker := "powerlinux-go-" + strconvTime()
	filter, _ := json.Marshal(map[string]any{"qa_marker": marker})
	defer func() {
		raw, _ := json.Marshal(mongoInput{ID: profile.ID, Collection: "qa_records", Filter: filter})
		s.Execute(context.Background(), actor, "mongodb_documents_delete", raw)
	}()
	doc, _ := json.Marshal(map[string]any{"qa_marker": marker, "label": "inserted"})
	liveAction(t, s, actor, "mongodb_documents_insert", mongoInput{ID: profile.ID, Collection: "qa_records", Documents: []json.RawMessage{doc}})
	update := json.RawMessage(`{"$set":{"label":"updated"}}`)
	liveAction(t, s, actor, "mongodb_documents_update", mongoInput{ID: profile.ID, Collection: "qa_records", Filter: filter, Update: update})
	result := liveAction(t, s, actor, "mongodb_documents_find", mongoInput{ID: profile.ID, Collection: "qa_records", Filter: filter, Limit: 1}).(map[string]any)
	items := result["items"].([]json.RawMessage)
	if len(items) != 1 || !bytes.Contains(items[0], []byte(`"updated"`)) {
		t.Fatal("real MongoDB CRUD mismatch")
	}
	var dangerous DatabaseRegisterInput
	if json.Unmarshal(fixtures["mongodb_global"], &dangerous) != nil {
		t.Fatal("missing global MongoDB role fixture")
	}
	dangerous.Name = "Global MongoDB role fixture"
	raw, _ := json.Marshal(dangerous)
	_, err = s.Execute(context.Background(), a, "database_connection_register", raw)
	if core.ErrorCode(err) != "credential_scope" || core.ErrorStatus(err) != 403 {
		t.Fatal("global MongoDB role accepted", core.ErrorCode(err))
	}
}
func TestLiveRedisMemcachedNamespacesAndCollections(t *testing.T) {
	s, a, m, fixtures := liveDriverFixture(t)
	for _, engine := range []string{"redis", "memcached"} {
		t.Run(engine, func(t *testing.T) {
			var in CacheRegisterInput
			if json.Unmarshal(fixtures[engine], &in) != nil {
				t.Fatal("missing cache fixture")
			}
			profile := liveAction(t, s, a, "cache_connection_register", cacheRegistration(in)).(CacheConnection)
			token, err := s.CreateToken(context.Background(), a, core.TokenRequest{Name: "bounded namespace QA", Scopes: []string{"cache:read", "cache:write", "audit:read"}, Resources: []string{CacheResource(profile)}})
			if err != nil {
				t.Fatal(err)
			}
			actor, err := s.AuthenticateToken(context.Background(), token.Token)
			if err != nil {
				t.Fatal(err)
			}
			key := "go:" + strconvTime()
			liveAction(t, s, actor, "cache_key_set", cacheInput{ID: profile.ID, Key: key, Value: "actual cache value", TTL: 60})
			result := liveAction(t, s, actor, "cache_key_get", cacheInput{ID: profile.ID, Key: key}).(map[string]any)
			if result["found"] != true || result["value"] != "actual cache value" {
				t.Fatal("real cache read did not match write")
			}
			liveAction(t, s, actor, "cache_keys_delete", cacheInput{ID: profile.ID, Keys: []string{key}})
			result = liveAction(t, s, actor, "cache_key_get", cacheInput{ID: profile.ID, Key: key}).(map[string]any)
			if result["found"] != false {
				t.Fatal("deleted key remained visible")
			}
			if engine == "redis" {
				for _, spec := range []struct {
					write, read, key string
					input            cacheInput
				}{{"cache_hash_set", "cache_hash_read", key + ":hash", cacheInput{Fields: map[string]string{"field": "value"}}}, {"cache_set_add", "cache_set_read", key + ":set", cacheInput{Members: []string{"one", "two"}}}, {"cache_list_push", "cache_list_read", key + ":list", cacheInput{Members: []string{"one", "two"}}}} {
					input := spec.input
					input.ID = profile.ID
					input.Key = spec.key
					liveAction(t, s, actor, spec.write, input)
					read := cacheInput{ID: profile.ID, Key: spec.key}
					if spec.read == "cache_list_read" {
						read.Stop = 1
					}
					var readInput any = read
					if spec.read == "cache_list_read" {
						readInput = map[string]any{"id": profile.ID, "key": spec.key, "start": 0, "stop": 1}
					}
					result := liveAction(t, s, actor, spec.read, readInput).(map[string]any)
					if result["items"] == nil {
						t.Fatal("actual collection read missing data")
					}
					liveAction(t, s, actor, "cache_keys_delete", cacheInput{ID: profile.ID, Keys: []string{spec.key}})
				}
				privateProfile, err := m.cacheProfile(context.Background(), profile.ID)
				if err != nil {
					t.Fatal("private cache profile could not be resolved")
				}
				client, err := m.cacheClient(context.Background(), actor, privateProfile)
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				client.Set(context.Background(), physicalCacheKey(profile, key), strings.Repeat("x", 129<<10), time.Minute)
				raw, _ := json.Marshal(cacheInput{ID: profile.ID, Key: key})
				_, err = s.Execute(context.Background(), actor, "cache_key_get", raw)
				if core.ErrorCode(err) != "result_limit" {
					t.Fatal("large value was not bounded", core.ErrorCode(err))
				}
				client.Del(context.Background(), physicalCacheKey(profile, key))
			}
		})
	}
}
