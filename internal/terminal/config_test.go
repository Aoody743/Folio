package terminal

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestValidateConfigHasNoFilesystemSideEffects(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-created", "terminal")
	if err := ValidateConfig(Config{StateDir: path}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("read-only config validation created directories")
	}
	if err := ValidateConfig(Config{StateDir: path, Profiles: []LocalProfile{{ID: "local"}}}); err == nil {
		t.Fatal("invalid local profile accepted")
	}
}

func TestProfileIdentityRejectsRootGroupDespiteIndependentUID(t *testing.T) {
	for _, enableRoot := range []bool{false, true} {
		for _, unsafeTest := range []bool{false, true} {
			if err := validateProfileIdentity(60001, 0, 60002, 60002, enableRoot, unsafeTest); err == nil {
				t.Fatal("a non-root UID with group 0 bypassed isolation through root enablement or the Go-only test option")
			}
		}
	}
	if err := validateProfileIdentity(60001, 60001, 60002, 60002, false, false); err != nil {
		t.Fatal("independent unprivileged UID/GID was rejected", err)
	}
	if err := validateProfileIdentity(60001, 60002, 60002, 60002, false, false); err == nil {
		t.Fatal("API primary group was allowed")
	}
	if err := validateProfileIdentity(0, 0, 60002, 60002, false, false); err == nil {
		t.Fatal("root profile was implicitly enabled")
	}
	if err := validateProfileIdentity(0, 0, 60002, 60002, true, false); err != nil {
		t.Fatal("explicit root profile was rejected", err)
	}
}

func TestRejectedProfileConstructorDoesNotCreateState(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	state := filepath.Join(home, "rejected-state")
	manager, err := New(Options{Config: Config{
		APIUser: account.Username, StateDir: state,
		Profiles: []LocalProfile{{ID: "local", User: account.Username, Shell: "/bin/bash", Home: home, WorkDir: home}},
	}})
	if err == nil {
		manager.Close()
		t.Fatal("an API/root identity was allowed as an ordinary local terminal")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("rejected terminal identity created durable state", err)
	}
}
