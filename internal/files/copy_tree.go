package files

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

type treeCopyBudget struct {
	Bytes   int64
	Entries int
}

// copyTree only writes into a private stage. Each file is streamed, synced and
// checked for ordinary concurrent source changes before the stage is published.
func (s *Service) copyTree(ctx context.Context, source *managedRoot, from string, destination *os.Root, to string, owner *syscall.Stat_t, budget *treeCopyBudget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.guardRootPath(source, from, false); err != nil {
		return err
	}
	info, err := strictLstat(source.root, from)
	if err != nil {
		return err
	}
	budget.Entries++
	if budget.Entries > 100000 {
		return failure("too_large", "Copy exceeds 100000 entries", 413)
	}
	identity, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("filesystem identity metadata unavailable")
	}
	chosen := owner
	if chosen == nil {
		chosen = identity
	}
	if info.IsDir() {
		if err := strictMkdir(destination, to, 0700); err != nil {
			return err
		}
		output, err := strictOpen(destination, to, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		defer output.Close()
		input, err := strictOpen(source.root, from, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		defer input.Close()
		for {
			entries, readErr := input.ReadDir(128)
			for _, entry := range entries {
				if err := s.copyTree(ctx, source, filepath.Join(from, entry.Name()), destination, filepath.Join(to, entry.Name()), owner, budget); err != nil {
					return err
				}
			}
			if errors.Is(readErr, io.EOF) {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		if err := setCopiedMetadata(output, chosen, info.Mode().Perm()); err != nil {
			return err
		}
		return output.Sync()
	}
	input, actual, err := openRegular(source.root, from, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer input.Close()
	budget.Bytes += actual.Size()
	if budget.Bytes > s.options.MaxTransferBytes {
		return failure("too_large", "Copy exceeds transfer budget", 413)
	}
	output, err := strictOpen(destination, to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	n, err := io.CopyBuffer(output, io.LimitReader(contextReader{ctx, input}, actual.Size()+1), make([]byte, 32<<10))
	if err != nil {
		return err
	}
	after, err := input.Stat()
	if err != nil {
		return err
	}
	if n != actual.Size() || after.Size() != actual.Size() || !after.ModTime().Equal(actual.ModTime()) {
		return failure("conflict", "Source changed during copy", 409)
	}
	if err := setCopiedMetadata(output, chosen, actual.Mode().Perm()); err != nil {
		return err
	}
	return output.Sync()
}

func setCopiedMetadata(file *os.File, identity *syscall.Stat_t, mode fs.FileMode) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	current, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("filesystem identity metadata unavailable")
	}
	if current.Uid != identity.Uid || current.Gid != identity.Gid {
		if err := file.Chown(int(identity.Uid), int(identity.Gid)); err != nil {
			return err
		}
	}
	return file.Chmod(mode.Perm())
}

func (s *Service) copyDirectoryAtomic(ctx context.Context, actorID string, source *managedRoot, sourcePath string, destination *managedRoot, destinationPath string, move bool) (any, error) {
	if _, err := strictLstat(destination.root, destinationPath); err == nil {
		return nil, failure("conflict", "Directory destination exists", 409)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	parent, err := strictLstat(destination.root, filepath.Dir(destinationPath))
	if err != nil {
		return nil, err
	}
	owner, ok := parent.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("destination ownership metadata unavailable")
	}
	if move {
		owner = nil
	}
	stagePath := filepath.Join(filepath.Dir(destinationPath), ".powerlinux-copy-"+newID())
	if err := strictMkdir(destination.root, stagePath, 0700); err != nil {
		return nil, err
	}
	defer strictRemove(destination.root, stagePath, true)
	stage, err := strictSubroot(destination.root, stagePath)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	budget := &treeCopyBudget{}
	if err := s.copyTree(ctx, source, sourcePath, stage, "payload", owner, budget); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := renameBetweenNoReplace(stage, "payload", destination.root, destinationPath); err != nil {
		return nil, err
	}
	if move {
		if err := strictRemove(source.root, sourcePath, true); err != nil {
			return map[string]any{"path": destinationPath, "copied": true, "moved": false}, &Error{Code: "source_cleanup_failed", Message: "Destination is complete but source removal failed; both copies were retained", Status: 409}
		}
	}
	return map[string]any{"path": destinationPath, "bytes": budget.Bytes, "entries": budget.Entries, "moved": move}, nil
}
