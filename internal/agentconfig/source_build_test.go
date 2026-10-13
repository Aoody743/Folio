package agentconfig

import (
	"os/user"
	"strconv"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/plugins"
	"github.com/Aoody743/PowerLinux/internal/systemops"
	"github.com/Aoody743/PowerLinux/internal/terminal"
)

func TestSourceBuilderCannotShareAgentPeerOrInteractiveUID(t *testing.T) {
	builder, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal("Linux nobody fixture required", err)
	}
	uid, err := strconv.ParseUint(builder.Uid, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	base := Config{Plugins: &plugins.PrivilegedConfig{EnableSourceBuild: true, BuildUser: builder.Username}}
	if err = validateSourceBuilderIsolation(base); err != nil {
		t.Fatal("independent builder rejected", err)
	}
	cases := map[string]Config{
		"Agent peer": {Plugins: base.Plugins, AllowedUIDs: []uint32{uint32(uid)}},
		"terminal":   {Plugins: base.Plugins, Terminal: &terminal.Config{Profiles: []terminal.LocalProfile{{User: builder.Username}}}},
		"script":     {Plugins: base.Plugins, Host: &systemops.HostConfig{ScriptProfiles: []systemops.ScriptProfile{{User: builder.Username}}}},
		"host API":   {Plugins: base.Plugins, Host: &systemops.HostConfig{APIUser: builder.Username}},
	}
	for name, config := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateSourceBuilderIsolation(config); err == nil {
				t.Fatal("source builder shares a user-controlled or privileged identity")
			}
		})
	}
}

func TestSourceBuilderCannotReadAnotherModuleThroughSharedOrRootGroup(t *testing.T) {
	builder, err := user.Lookup("daemon")
	if err != nil {
		t.Fatal("Linux daemon fixture required", err)
	}
	other, err := user.Lookup("nobody")
	if err != nil {
		t.Fatal("Linux nobody fixture required", err)
	}
	group, err := user.LookupGroupId(other.Gid)
	if err != nil {
		t.Fatal(err)
	}
	if builder.Uid == other.Uid {
		t.Fatal("isolated UID fixture unavailable")
	}
	config := Config{Plugins: &plugins.PrivilegedConfig{EnableSourceBuild: true, BuildUser: builder.Username, BuildGroup: group.Name}, Host: &systemops.HostConfig{APIUser: other.Username}}
	if err = validateSourceBuilderIsolation(config); err == nil {
		t.Fatal("different builder UID shared the other API's protected group")
	}
	rootGroup, err := user.LookupGroupId("0")
	if err != nil {
		t.Fatal(err)
	}
	config.Host = nil
	config.Plugins.BuildGroup = rootGroup.Name
	if err = validateSourceBuilderIsolation(config); err == nil {
		t.Fatal("builder received root group authority")
	}
}
