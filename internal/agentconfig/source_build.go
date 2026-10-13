package agentconfig

import (
	"errors"
	"os/user"
	"strconv"
)

// A user-controlled shell must never share the identity that produces future
// root-launched software. Compare OS UIDs, including aliases, not account names.
func validateSourceBuilderIsolation(config Config) error {
	if config.Plugins == nil || !config.Plugins.EnableSourceBuild {
		return nil
	}
	builder, err := user.Lookup(config.Plugins.BuildUser)
	if err != nil {
		return errors.New("source builder user does not exist")
	}
	uid, err := strconv.ParseUint(builder.Uid, 10, 32)
	if err != nil || uid == 0 {
		return errors.New("source builder requires a distinct non-root UID")
	}
	builderGID := builder.Gid
	if config.Plugins.BuildGroup != "" {
		group, err := user.LookupGroup(config.Plugins.BuildGroup)
		if err != nil {
			return errors.New("source builder group does not exist")
		}
		builderGID = group.Gid
	}
	if number, err := strconv.ParseUint(builderGID, 10, 32); err != nil || number == 0 {
		return errors.New("source builder requires a distinct non-root GID")
	}
	for _, peer := range config.AllowedUIDs {
		if uint32(uid) == peer {
			return errors.New("source builder must not be an authorized Agent peer")
		}
	}
	names := []string{config.Plugins.APIUser, config.Plugins.RunUser}
	if config.Terminal != nil {
		names = append(names, config.Terminal.APIUser)
		for _, profile := range config.Terminal.Profiles {
			names = append(names, profile.User)
		}
	}
	if config.Host != nil {
		names = append(names, config.Host.APIUser)
		for _, profile := range config.Host.ScriptProfiles {
			names = append(names, profile.User)
		}
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		account, err := user.Lookup(name)
		if err != nil {
			return errors.New("source builder isolation requires existing runtime and interactive users")
		}
		if account.Uid == builder.Uid {
			return errors.New("source builder UID must differ from API, software, terminal and script users")
		}
		if account.Gid == builderGID {
			return errors.New("source builder GID must differ from API, software, terminal and script groups")
		}
	}
	if config.Plugins.RunGroup != "" {
		group, err := user.LookupGroup(config.Plugins.RunGroup)
		if err != nil {
			return errors.New("software runtime group does not exist")
		}
		if group.Gid == builderGID {
			return errors.New("source builder GID must differ from the configured runtime group")
		}
	}
	return nil
}
