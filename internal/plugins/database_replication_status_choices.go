package plugins

// databaseReplicationRecoveryChoices is the UI contract derived from the
// protected journal, not from client input. Empty choices deliberately mean
// the UI must not guess what recovery action is safe.
func databaseReplicationRecoveryChoices(record databaseReplicationRecord) []string {
	choices := []string{}
	if !validReplicationOperation(record.Operation) || !validReplicationPhase(record.Phase) || record.State == "removed" {
		return choices
	}
	if record.Phase == "settled" && record.State != "recovery_required" {
		return choices
	}
	switch record.Operation {
	case "configure":
		switch record.Phase {
		case "config_pending", "source_config_pending", "source_configured", "target_config_pending", "target_configured",
			"seed_dump_pending", "seed_dumped", "seed_import_pending", "seed_imported", "account_create_pending", "account_created",
			"account_grant_pending", "account_granted", "channel_config_pending", "channel_configured", "channel_created", "channel_start_pending":
			choices = append(choices, "apply_pending")
		case "filter_config_pending":
			if record.Engine == "mysql" {
				choices = append(choices, "apply_pending")
			}
		}
		switch record.Phase {
		case "config_pending", "source_config_pending", "source_configured", "target_config_pending", "target_configured":
			choices = append(choices, "restore_previous")
		}
	case "start":
		if record.Phase == "channel_start_pending" {
			choices = append(choices, "apply_pending")
		}
	case "stop":
		if record.Phase == "channel_stop_pending" {
			choices = append(choices, "apply_pending")
		}
	case "remove":
		switch record.Phase {
		case "channel_stop_pending", "remove_channel_pending", "channel_removed", "remove_account_pending", "account_removed",
			"restore_target_config_pending", "restore_source_config_pending", "config_restored", "seed_cleanup_pending", "seed_cleaned":
			choices = append(choices, "apply_pending")
		}
	case "recover":
		switch record.Phase {
		case "restore_target_config_pending", "restore_source_config_pending", "config_restored":
			choices = append(choices, "restore_previous")
		}
	}
	return choices
}
