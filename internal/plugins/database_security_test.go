package plugins

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func databaseManagerFixture(t *testing.T) (*core.Service, core.Actor, *Manager) {
	t.Helper()
	s, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	secret, err := os.ReadFile(s.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Setup(context.Background(), strings.TrimSpace(string(secret)), "admin", "isolated database security password")
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(s.Runtime("plugins"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(m); err != nil {
		t.Fatal(err)
	}
	return s, a, m
}

func TestConnectionReplacementAuthorizesOldAndNewResourcesBeforeDial(t *testing.T) {
	s, admin, m := databaseManagerFixture(t)
	ctx := context.Background()
	old := DatabaseConnection{ID: "scoped", Engine: "postgresql", Database: "private_before", Access: "database", Host: "127.0.0.1", Port: 1, Username: "scoped_user"}
	encoded, _ := json.Marshal(old)
	if _, err := m.deps.State.CompareAndSwap(ctx, "database_connections", old.ID, 0, encoded); err != nil {
		t.Fatal(err)
	}
	token, err := s.CreateToken(ctx, admin, core.TokenRequest{Name: "destination only", Scopes: []string{"databases:admin"}, Resources: []string{"db:scoped", "db:scoped:destination"}})
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AuthenticateToken(ctx, token.Token)
	if err != nil {
		t.Fatal(err)
	}
	input := DatabaseRegisterInput{ID: old.ID, Name: "replacement", Engine: old.Engine, Host: old.Host, Port: old.Port, Database: "destination", Username: old.Username, Password: "unused credential", Access: "database", ExpectedRevision: 1}
	raw, _ := json.Marshal(input)
	if _, err = s.Execute(ctx, a, "database_connection_register", raw); core.ErrorCode(err) != "forbidden" {
		t.Fatal("old database authorization was bypassed", core.ErrorCode(err))
	}
	// Supplying a stale revision must fail before contacting the server too.
	input.ExpectedRevision = 0
	raw, _ = json.Marshal(input)
	if _, err = s.Execute(ctx, admin, "database_connection_register", raw); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale registration reached the wire", core.ErrorCode(err))
	}
	old.InstanceID = "owned-native"
	encoded, _ = json.Marshal(old)
	if _, err = m.deps.State.CompareAndSwap(ctx, "database_connections", old.ID, 1, encoded); err != nil {
		t.Fatal(err)
	}
	input.ExpectedRevision = 2
	raw, _ = json.Marshal(input)
	if _, err = s.Execute(ctx, admin, "database_connection_register", raw); core.ErrorCode(err) != "managed_profile" {
		t.Fatal("external registration replaced a managed credential", core.ErrorCode(err))
	}
}
