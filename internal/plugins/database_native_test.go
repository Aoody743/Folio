package plugins

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in release acceptance against an actual installed API/root Agent pair.
// The protected fixture and immutable version are required, so a temporary
// in-process server or an older release cannot stand in for native acceptance.
type nativeSQLClient struct {
	t         *testing.T
	url, csrf string
	client    *http.Client
}

func (c *nativeSQLClient) request(path string, input any, want int) json.RawMessage {
	c.t.Helper()
	body, _ := json.Marshal(input)
	request, _ := http.NewRequest("POST", c.url+path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", c.url)
	if c.csrf != "" {
		request.Header.Set("X-CSRF-Token", c.csrf)
	}
	response, err := c.client.Do(request)
	if err != nil {
		c.t.Fatalf("native request %s failed to complete", path)
	}
	defer response.Body.Close()
	var result struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 2<<20)).Decode(&result) != nil {
		c.t.Fatalf("native request %s returned invalid JSON", path)
	}
	if response.StatusCode != want {
		c.t.Fatalf("native request %s returned HTTP%d code=%s", path, response.StatusCode, result.Error.Code)
	}
	return result.Data
}
func (c *nativeSQLClient) action(name string, input any) json.RawMessage {
	return c.request("/api/v1/actions/"+name, input, 200)
}
func (c *nativeSQLClient) job(name string, input any) json.RawMessage {
	c.t.Helper()
	raw := c.action(name, input)
	var submitted struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &submitted) != nil || submitted.ID == "" {
		c.t.Fatal("native durable task receipt missing")
	}
	deadline := time.Now().Add(25 * time.Minute)
	for time.Now().Before(deadline) {
		var job struct {
			State  string `json:"state"`
			Error  string `json:"error"`
			Result struct {
				Stdout string          `json:"stdout"`
				Data   json.RawMessage `json:"data"`
			} `json:"result"`
		}
		if json.Unmarshal(c.action("tasks_get", map[string]any{"id": submitted.ID}), &job) != nil {
			c.t.Fatal("native task result was invalid")
		}
		if job.State == "succeeded" {
			if len(job.Result.Data) > 0 {
				return job.Result.Data
			}
			return json.RawMessage(job.Result.Stdout)
		}
		if job.State == "failed" || job.State == "canceled" || job.State == "interrupted" {
			c.t.Fatalf("native task %s ended %s: %s", name, job.State, job.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.t.Fatalf("native task %s exceeded its execution bound", name)
	return nil
}
func (c *nativeSQLClient) software(plugin, label, operation string, port int, keep bool) Instance {
	c.t.Helper()
	var plan Plan
	input := map[string]any{"plugin_id": plugin, "instance_id": label, "operation": operation, "version": "distribution", "keep_data": keep}
	if operation == "install" {
		input["config"] = map[string]any{"bind": "127.0.0.1", "port": port}
	}
	if json.Unmarshal(c.action("software_plan", input), &plan) != nil || plan.ID == "" {
		c.t.Fatal("native reviewed software plan missing")
	}
	c.t.Logf("native %s %s plan=%s reviewed_packages=%d", plugin, operation, plan.ID, len(plan.Packages))
	c.job("software_apply", map[string]any{"plan_id": plan.ID, "expected_revision": plan.ExpectedRevision, "idempotency_key": "native-sql-" + plan.ID})
	var instance Instance
	if json.Unmarshal(c.action("plugins_get", map[string]any{"id": plugin + "-" + label}), &instance) != nil {
		c.t.Fatal("native instance receipt missing")
	}
	return instance
}
func (c *nativeSQLClient) query(id, sql, mode string) json.RawMessage {
	return c.action("database_query", map[string]any{"id": id, "sql": sql, "mode": mode})
}

func TestNativeReleaseSQLDaemonDumpRestorePermissionsAndRetainedUninstall(t *testing.T) {
	path := os.Getenv("POWERLINUX_NATIVE_SQL_PANEL")
	if path == "" {
		t.Skip("requires an immutable release on the isolated native SQL VM")
	}
	version := os.Getenv("POWERLINUX_NATIVE_SQL_VERSION")
	if version == "" {
		t.Fatal("actual immutable version is required")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("native panel fixture must be private")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("native panel fixture could not be read")
	}
	var fixture struct{ URL, Username, Password string }
	if json.Unmarshal(raw, &fixture) != nil || fixture.URL == "" || fixture.Password == "" {
		t.Fatal("invalid native panel fixture")
	}
	jar, _ := cookiejar.New(nil)
	c := &nativeSQLClient{t: t, url: strings.TrimRight(fixture.URL, "/"), client: &http.Client{Jar: jar, Timeout: 30 * time.Minute}}
	response, err := c.client.Get(c.url + "/healthz")
	if err != nil {
		t.Fatal("native production API is unavailable")
	}
	health, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	var runtime struct {
		Version string `json:"version"`
		Ready   bool   `json:"ready"`
	}
	if err != nil || response.StatusCode != 200 || json.Unmarshal(health, &runtime) != nil || runtime.Version != version || !runtime.Ready {
		t.Fatal("native API version does not match required immutable release")
	}
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	if json.Unmarshal(c.request("/api/v1/auth/login", map[string]any{"username": fixture.Username, "password": fixture.Password}, 200), &login) != nil || login.CSRF == "" {
		t.Fatal("native panel login failed")
	}
	c.csrf = login.CSRF
	for _, spec := range []struct {
		engine string
		port   int
	}{{"mariadb", 13306}, {"postgresql", 15432}} {
		t.Run(spec.engine, func(t *testing.T) {
			c := *c
			c.t = t
			label := os.Getenv("POWERLINUX_NATIVE_SQL_LABEL")
			if label == "" {
				label = "qa-native"
			}
			instance := c.software(spec.engine, label, "install", spec.port, true)
			if instance.Status != "running" || instance.Health.Status != "healthy" || instance.Ownership == "" || len(instance.Packages) == 0 {
				t.Fatal("native daemon did not reach an owned healthy state")
			}
			name := uniqueManagedName()
			var database ManagedDatabase
			if json.Unmarshal(c.action("managed_database_create", map[string]any{"instance_id": instance.ID, "database": name, "permission": "read_write"}), &database) != nil || database.Revision != 1 {
				t.Fatal("native dedicated database creation failed")
			}
			id := "managed_" + hex.EncodeToString(sha256Digest(instance.ID + "|" + name)[:16])
			blobType, blobExpr := "BLOB", "UNHEX('00ff02')"
			if spec.engine == "postgresql" {
				blobType, blobExpr = "BYTEA", "decode('00ff02','hex')"
			}
			c.query(id, "CREATE TABLE native_records(id BIGINT PRIMARY KEY,payload "+blobType+",value BIGINT)", "execute")
			c.query(id, "INSERT INTO native_records VALUES(1,"+blobExpr+",9007199254740993)", "execute")
			var backup DatabaseBackup
			if json.Unmarshal(c.job("managed_database_backup", map[string]any{"instance_id": instance.ID, "database": name}), &backup) != nil || backup.Bytes == 0 || len(backup.SHA256) != 64 {
				t.Fatal("native dump did not produce a durable integrity receipt")
			}
			c.query(id, "DELETE FROM native_records", "execute")
			c.job("managed_database_restore", map[string]any{"instance_id": instance.ID, "database": name, "backup_id": backup.ID})
			var rows struct {
				Items []struct {
					Payload struct{ Encoding, Data string }
					Value   struct{ Encoding, Value string }
				} `json:"items"`
			}
			if json.Unmarshal(c.query(id, "SELECT payload,value FROM native_records WHERE id=1", "query"), &rows) != nil || len(rows.Items) != 1 || rows.Items[0].Value.Encoding != "integer" || rows.Items[0].Value.Value != "9007199254740993" {
				t.Fatal("native SQL restore did not preserve BIGINT row data")
			}
			binary, err := base64.StdEncoding.DecodeString(rows.Items[0].Payload.Data)
			if err != nil || rows.Items[0].Payload.Encoding != "base64" || !bytes.Equal(binary, []byte{0, 255, 2}) {
				t.Fatal("native SQL restore did not preserve binary bytes")
			}
			for revision, permission := range []string{"read", "data_write", "read_write"} {
				c.action("managed_database_set_permission", map[string]any{"instance_id": instance.ID, "database": name, "permission": permission, "expected_revision": revision + 1})
				if permission == "read" {
					c.request("/api/v1/actions/database_query", map[string]any{"id": id, "sql": "INSERT INTO native_records VALUES(2,NULL,1)", "mode": "execute"}, 400)
				}
				if permission == "data_write" {
					c.query(id, "INSERT INTO native_records VALUES(2,NULL,1)", "execute")
					c.query(id, "DELETE FROM native_records WHERE id=2", "execute")
					c.request("/api/v1/actions/database_query", map[string]any{"id": id, "sql": "ALTER TABLE native_records ADD COLUMN forbidden INT", "mode": "execute"}, 400)
				}
				if permission == "read_write" {
					c.query(id, "ALTER TABLE native_records ADD COLUMN restored INT", "execute")
				}
			}
			retained := c.software(spec.engine, label, "uninstall", spec.port, true)
			if retained.Status != "uninstalled" {
				t.Fatal("native controlled uninstall did not stop its owned service")
			}
			t.Logf("native %s owned database=%s dump=%s bytes=%d sha256=%s; reinstall will verify retained data", spec.engine, name, backup.ID, backup.Bytes, backup.SHA256)
			instance = c.software(spec.engine, label, "install", spec.port, true)
			if instance.Status != "running" || instance.Health.Status != "healthy" {
				t.Fatal("native retained-data reinstall did not restore a healthy daemon")
			}
			c.query(id, "SELECT value FROM native_records WHERE id=1", "query")
			c.action("managed_database_backup_delete", map[string]any{"instance_id": instance.ID, "database": name, "backup_id": backup.ID})
			c.job("managed_database_delete", map[string]any{"instance_id": instance.ID, "database": name, "expected_revision": 4})
			c.software(spec.engine, label, "uninstall", spec.port, false)
			t.Logf("native %s SQL daemon/dump/restore/three permissions/retained reinstall completed; owned QA database removed", spec.engine)
		})
	}
}
