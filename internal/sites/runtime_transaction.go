package sites

import "context"

type mutationRuntimeContextKey struct{}

func mutationRuntimeActive(ctx context.Context, executor *AgentExecutor) bool {
	active, ok := ctx.Value(mutationRuntimeContextKey{}).(*AgentExecutor)
	return ok && active == executor
}

// Production composition implements this through the protected software
// instance locks. Test runtimes may omit it without claiming lifecycle proof.
type RuntimeTransactions interface {
	WithRuntimeTransaction(context.Context, []string, func(context.Context) error) error
}

func (e *AgentExecutor) mutationRuntimes(operation string, in request) ([]string, error) {
	ids := []string{}
	appendSite := func(site Site) {
		ids = append(ids, site.WebInstance)
		if site.PHPInstance != "" {
			ids = append(ids, site.PHPInstance)
		}
	}
	if operation == "site_php_cli_set" {
		return []string{in.ID}, nil
	}
	if operation == "certificate_import" {
		all, err := e.list()
		if err != nil {
			return nil, err
		}
		for _, site := range all {
			if site.TLS.Enabled && site.TLS.CertificateID == in.ID {
				ids = append(ids, site.WebInstance)
			}
		}
		return ids, nil
	}
	switch operation {
	case "site_apply", "site_delete", "site_config_set", "site_default_set", "site_backup_restore", "site_backup_create":
	default:
		return ids, nil
	}
	if in.Site != nil {
		appendSite(*in.Site)
	}
	current, err := e.get(in.ID)
	if err == nil {
		appendSite(current)
	} else if !isNotFound(err) || operation != "site_apply" {
		return nil, err
	}
	if operation == "site_default_set" && in.PreviousSiteID != "" {
		previous, err := e.get(in.PreviousSiteID)
		if err != nil {
			return nil, err
		}
		appendSite(previous)
	}
	if operation == "site_backup_restore" {
		manifest, err := e.backupMetadata(in)
		if err != nil {
			return nil, err
		}
		appendSite(manifest.Site)
	}
	return ids, nil
}

func (e *AgentExecutor) withMutationRuntimes(ctx context.Context, operation string, in request, callback func(context.Context) error) error {
	run := func(locked context.Context) error {
		return callback(context.WithValue(locked, mutationRuntimeContextKey{}, e))
	}
	transaction, ok := e.runtimes.(RuntimeTransactions)
	if !ok {
		return run(ctx)
	}
	ids, err := e.mutationRuntimes(operation, in)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return run(ctx)
	}
	return transaction.WithRuntimeTransaction(ctx, ids, run)
}

func websiteJournalOperation(operation string) bool {
	switch operation {
	case "site_apply", "site_delete", "site_config_set", "site_default_set", "site_backup_restore", "certificate_import", "certificate_delete":
		return true
	default:
		return false
	}
}
