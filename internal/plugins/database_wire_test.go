package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func TestSQLExactIntegerEncodingAndParameters(t *testing.T) {
	for _, n := range []any{int64(9007199254740992), int64(-9007199254740992), int64(math.MinInt64), int64(math.MaxInt64), uint64(math.MaxUint64)} {
		encoded, err := json.Marshal(exactSQLInteger(n))
		if err != nil || !bytes.Contains(encoded, []byte(`"encoding":"integer"`)) {
			t.Fatal("unsafe SQL integer was exposed as a lossy JSON number")
		}
	}
	for _, n := range []int64{-9007199254740991, 9007199254740991, 0} {
		if exactSQLInteger(n) != n {
			t.Fatal("safe integer representation changed")
		}
	}
	for _, text := range []string{"9007199254740993", "-9223372036854775808", "18446744073709551615"} {
		value, err := exactSQLParameter(map[string]any{"encoding": "integer", "value": text})
		if err != nil || value != text {
			t.Fatal("typed SQL integer parameter lost precision")
		}
	}
	for _, text := range []string{"9007199254740993", "18446744073709551615", "999999999999999999999.123456789", "1.23456789123456789e-100"} {
		value, err := exactSQLParameter(json.Number(text))
		if err != nil || value != text {
			t.Fatal("numeric SQL parameter was converted through float64")
		}
	}
	for _, text := range []string{"18446744073709551616", "-9223372036854775809", "1e3", "1.5", "+1", "01"} {
		if _, err := exactSQLParameter(map[string]any{"encoding": "integer", "value": text}); core.ErrorCode(err) != "invalid_input" {
			t.Fatal("invalid typed integer was accepted")
		}
	}
	if got := integerColumnText("UNSIGNED BIGINT", "18446744073709551615").(map[string]any); got["value"] != "18446744073709551615" {
		t.Fatal("unsigned integer byte-string lost precision")
	}
}

func TestPublicConnectionProfilesExposeAuthoritativeResourcesWithoutSecretReferences(t *testing.T) {
	s, admin, m := databaseManagerFixture(t)
	ctx := context.Background()
	profiles := []DatabaseConnection{
		{ID: "one", Engine: "postgresql", Database: "public", Access: "database"},
		{ID: "two", Engine: "mariadb", Database: "public", Access: "instance"},
		{ID: "three", InstanceID: "owned", Engine: "postgresql", Database: "selected", Access: "database"},
	}
	for _, p := range profiles {
		p.CredentialRef, p.Resource = "opaque-secret-reference", "db:forged"
		raw, _ := json.Marshal(p)
		if _, err := m.deps.State.CompareAndSwap(ctx, "database_connections", p.ID, 0, raw); err != nil {
			t.Fatal(err)
		}
		data := liveAction(t, s, admin, "database_connection_get", IDInput{ID: p.ID}).(DatabaseConnection)
		encoded, _ := json.Marshal(data)
		if data.Resource != databaseProfileResource(p) || bytes.Contains(encoded, []byte("credential_ref")) || bytes.Contains(encoded, []byte("opaque-secret")) {
			t.Fatal("public profile omitted its resource or exposed its private secret binding")
		}
		private, err := m.databaseProfile(ctx, p.ID)
		if err != nil || private.CredentialRef != p.CredentialRef {
			t.Fatal("public serialization destroyed the internal credential binding")
		}
	}
	p := CacheConnection{ID: "cache", Engine: "redis", Prefix: "isolated", Resource: "cache:forged", CredentialRef: "opaque-cache-secret"}
	raw, _ := json.Marshal(p)
	if _, err := m.deps.State.CompareAndSwap(ctx, "cache_connections", p.ID, 0, raw); err != nil {
		t.Fatal(err)
	}
	data := liveAction(t, s, admin, "cache_connection_get", IDInput{ID: p.ID}).(CacheConnection)
	encoded, _ := json.Marshal(data)
	if data.Resource != CacheResource(p) || bytes.Contains(encoded, []byte("credential_ref")) || bytes.Contains(encoded, []byte("opaque-cache")) {
		t.Fatal("public cache profile did not enforce its authoritative resource")
	}
	token, err := s.CreateToken(ctx, admin, core.TokenRequest{Name: "one profile", Scopes: []string{"databases:read"}, Resources: []string{databaseProfileResource(profiles[0])}})
	if err != nil {
		t.Fatal(err)
	}
	actor, err := s.AuthenticateToken(ctx, token.Token)
	if err != nil {
		t.Fatal(err)
	}
	listed := liveAction(t, s, actor, "database_connections_list", map[string]any{}).(map[string]any)["items"].([]DatabaseConnection)
	if len(listed) != 1 || listed[0].Resource != databaseProfileResource(profiles[0]) || listed[0].CredentialRef != "" {
		t.Fatal("profile list bypassed resource filtering or retained a credential reference")
	}
}

func TestLiveSQLExactBIGINTAndDecimalParameters(t *testing.T) {
	s, a, _, fixtures := liveDriverFixture(t)
	for _, engine := range []string{"mariadb", "postgresql"} {
		t.Run(engine, func(t *testing.T) {
			var in DatabaseRegisterInput
			if json.Unmarshal(fixtures[engine], &in) != nil {
				t.Fatal("SQL fixture unavailable")
			}
			in.Name = "Exact SQL integers"
			profile := liveAction(t, s, a, "database_connection_register", in).(DatabaseConnection)
			query := "SELECT CAST(? AS SIGNED) AS exact_value"
			if engine == "postgresql" {
				query = "SELECT CAST($1 AS BIGINT) AS exact_value"
			}
			for _, value := range []string{"9007199254740993", "9223372036854775807", "-9223372036854775808"} {
				for _, parameter := range []any{json.Number(value), map[string]any{"encoding": "integer", "value": value}} {
					rows := liveAction(t, s, a, "database_query", DatabaseQueryInput{ID: profile.ID, SQL: query, Parameters: []any{parameter}}).(map[string]any)["items"].([]map[string]any)
					encoded, _ := json.Marshal(rows)
					if len(rows) != 1 || rows[0]["exact_value"].(map[string]any)["value"] != value || !bytes.Contains(encoded, []byte(`"encoding":"integer"`)) {
						t.Fatal("actual SQL wire lost BIGINT precision")
					}
				}
			}
			query = "SELECT CAST(? AS DECIMAL(38,9)) AS exact_decimal"
			if engine == "postgresql" {
				query = "SELECT CAST($1 AS NUMERIC(38,9)) AS exact_decimal"
			}
			value := "999999999999999999999.123456789"
			rows := liveAction(t, s, a, "database_query", DatabaseQueryInput{ID: profile.ID, SQL: query, Parameters: []any{json.Number(value)}}).(map[string]any)["items"].([]map[string]any)
			if len(rows) != 1 || rows[0]["exact_decimal"] != value {
				t.Fatal("actual SQL wire lost exact decimal precision")
			}
			if engine == "mariadb" {
				rows = liveAction(t, s, a, "database_query", DatabaseQueryInput{ID: profile.ID, SQL: "SELECT CAST(? AS UNSIGNED) AS exact_value", Parameters: []any{map[string]any{"encoding": "integer", "value": "18446744073709551615"}}}).(map[string]any)["items"].([]map[string]any)
				if len(rows) != 1 || rows[0]["exact_value"].(map[string]any)["value"] != "18446744073709551615" {
					t.Fatal("actual MySQL unsigned BIGINT wire lost precision")
				}
			}
		})
	}
}
