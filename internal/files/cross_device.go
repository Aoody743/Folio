package files

import (
	"context"
	"path/filepath"
)

// copyAcrossDevices publishes a complete file/tree from a private container on
// the destination filesystem. It is also used for recycle/restore transfers.
func (s *Service) copyAcrossDevices(ctx context.Context, source *managedRoot, from string, destination *managedRoot, to string, overwrite bool) (*treeCopyBudget, error) {
	container := filepath.Join(filepath.Dir(to), ".powerlinux-transfer-"+newID())
	if err := strictMkdir(destination.root, container, 0700); err != nil {
		return nil, err
	}
	defer strictRemove(destination.root, container, true)
	stage, err := strictSubroot(destination.root, container)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	budget := &treeCopyBudget{}
	if err := s.copyTree(ctx, source, from, stage, "payload", nil, budget); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if overwrite {
		err = renameBetween(stage, "payload", destination.root, to)
	} else {
		err = renameBetweenNoReplace(stage, "payload", destination.root, to)
	}
	return budget, err
}

func (s *Service) moveAcrossDevices(ctx context.Context, actorID string, source *managedRoot, from string, destination *managedRoot, to string, in Input) (any, error) {
	// Detach the source by an atomic rename on its own filesystem. If copying
	// fails, restore it without overwriting a new path another process created.
	container := filepath.Join(filepath.Dir(from), ".powerlinux-move-"+newID())
	if err := strictMkdir(source.root, container, 0700); err != nil {
		return nil, err
	}
	stage, err := strictSubroot(source.root, container)
	if err != nil {
		strictRemove(source.root, container, true)
		return nil, err
	}
	defer stage.Close()
	if err := renameBetweenNoReplace(source.root, from, stage, "payload"); err != nil {
		strictRemove(source.root, container, true)
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			strictRemove(source.root, container, true)
		}
	}()
	privateSource := &managedRoot{root: stage, spec: RootSpec{ID: source.spec.ID, Path: filepath.Join(source.spec.Path, container)}}
	restore := func(cause error) (any, error) {
		if err := renameBetweenNoReplace(stage, "payload", source.root, from); err != nil {
			keep = true
			data := map[string]any{"source_root": source.spec.ID, "source_path": from, "recovery_path": filepath.Join(container, "payload"), "destination_root": destination.spec.ID, "destination_path": to}
			return data, &Error{Code: "rollback_required", Message: "Move failed and the original path was recreated; the complete source is retained in its private recovery path", Status: 409, Details: data}
		}
		return nil, cause
	}
	// A destination may be overwritten only with the same expected hash/explicit
	// decision used before attempting the native cross-filesystem rename.
	info, err := strictLstat(stage, "payload")
	if err != nil {
		return restore(err)
	}
	if !info.IsDir() {
		if err := s.checkExpected(ctx, Ref{destination.spec.ID, to}, in.ExpectedHash, in.Overwrite); err != nil {
			return restore(err)
		}
	}
	budget, err := s.copyAcrossDevices(ctx, privateSource, "payload", destination, to, !info.IsDir() && (in.ExpectedHash != "" || in.Overwrite))
	if err != nil {
		return restore(err)
	}
	if !info.IsDir() {
		if _, err := s.snapshot(ctx, actorID, Ref{destination.spec.ID, to}, ""); err != nil {
			keep = true
			return map[string]any{"path": to, "moved": true, "recovery_path": filepath.Join(container, "payload")}, err
		}
	}
	if err := stage.RemoveAll("payload"); err != nil {
		keep = true
		data := map[string]any{"path": to, "moved": true, "cleanup_path": filepath.Join(container, "payload")}
		return data, &Error{Code: "source_cleanup_failed", Message: "Move destination is complete; the private source backup needs cleanup", Status: 409, Details: data}
	}
	return map[string]any{"path": to, "moved": true, "bytes": budget.Bytes, "entries": budget.Entries}, nil
}

func (s *Service) privateManagedRoot() *managedRoot {
	return &managedRoot{root: s.state, spec: RootSpec{Path: s.options.StateDir}}
}

func (s *Service) recycleAcrossDevices(ctx context.Context, source *managedRoot, path, trashName string) error {
	if _, err := s.copyAcrossDevices(ctx, source, path, s.privateManagedRoot(), trashName, false); err != nil {
		return err
	}
	// Never discard the verified backup after a partial source-removal failure.
	return strictRemove(source.root, path, true)
}

func (s *Service) restoreAcrossDevices(ctx context.Context, trashName string, destination *managedRoot, path string) error {
	if _, err := s.copyAcrossDevices(ctx, s.privateManagedRoot(), trashName, destination, path, false); err != nil {
		return err
	}
	return s.state.RemoveAll(trashName)
}
