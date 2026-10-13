package files

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

type countedWriter struct {
	writer io.Writer
	bytes  int64
}

func (w *countedWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (s *Service) exportTree(ctx context.Context, root *managedRoot, base string, output io.Writer) (string, int64, error) {
	hash := sha256.New()
	counter := &countedWriter{writer: io.MultiWriter(output, hash)}
	writer := tar.NewWriter(counter)
	budget := &treeCopyBudget{}
	var add func(string) error
	add = func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.guardRootPath(root, path, false); err != nil {
			return err
		}
		info, err := strictLstat(root.root, path)
		if err != nil {
			return err
		}
		budget.Entries++
		if budget.Entries > s.options.MaxArchiveEntries {
			return failure("too_large", "Tree transfer exceeds entry budget", 413)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return failure("unsafe_path", "Tree transfers reject links and special files", 403)
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if relative != "." {
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = filepath.ToSlash(relative)
			header.Format = tar.FormatPAX
			header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
			if err := writer.WriteHeader(header); err != nil {
				return err
			}
		}
		if info.IsDir() {
			directory, err := strictOpen(root.root, path, os.O_RDONLY|syscall.O_DIRECTORY, 0)
			if err != nil {
				return err
			}
			defer directory.Close()
			entries, err := directory.ReadDir(-1)
			if err != nil {
				return err
			}
			sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
			for _, entry := range entries {
				if err := add(filepath.Join(path, entry.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		budget.Bytes += info.Size()
		if budget.Bytes > s.options.MaxTransferBytes {
			return failure("too_large", "Tree transfer exceeds byte budget", 413)
		}
		file, before, err := openRegular(root.root, path, os.O_RDONLY)
		if err != nil {
			return err
		}
		defer file.Close()
		n, err := io.CopyBuffer(writer, io.LimitReader(contextReader{ctx, file}, info.Size()+1), make([]byte, 32<<10))
		if err != nil {
			return err
		}
		after, err := file.Stat()
		if err != nil {
			return err
		}
		if n != info.Size() || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			return failure("conflict", "Source changed during tree transfer", 409)
		}
		return nil
	}
	if err := add(base); err != nil {
		return "", counter.bytes, err
	}
	if err := writer.Close(); err != nil {
		return "", counter.bytes, err
	}
	return hex.EncodeToString(hash.Sum(nil)), counter.bytes, nil
}

func (s *Service) importTree(ctx context.Context, actorID string, in Input, reader io.Reader) (any, error) {
	destination, path, err := s.resolve(Ref{in.Root, in.Path}, true)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx, in.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, err := strictLstat(destination.root, path); err == nil {
		return nil, failure("conflict", "Tree destination already exists", 409)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if in.Mode != nil && *in.Mode > 0777 {
		return nil, failure("invalid_input", "Special directory permission bits are forbidden", 400)
	}
	archivePath := filepath.Join("staging", newID()+".tree.tar")
	archive, err := s.state.OpenFile(archivePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	defer s.state.Remove(archivePath)
	hash := sha256.New()
	limit := s.options.MaxTransferBytes + int64(s.options.MaxArchiveEntries)*1024 + 1024
	n, err := io.CopyBuffer(io.MultiWriter(archive, hash), io.LimitReader(contextReader{ctx, reader}, limit+1), make([]byte, 32<<10))
	if err != nil {
		return nil, err
	}
	if n > limit {
		return nil, failure("too_large", "Tree stream exceeds transfer budget", 413)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if in.SHA256 != "" && in.SHA256 != digest {
		return nil, failure("checksum_mismatch", "Tree stream digest does not match", 400)
	}
	archive.Seek(0, io.SeekStart)
	stageName := filepath.Join("staging", newID()+".tree")
	if err := s.state.Mkdir(stageName, 0700); err != nil {
		return nil, err
	}
	defer removePrivateTree(s.state, stageName)
	stage, err := s.state.OpenRoot(stageName)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	budget := &archiveBudget{}
	if err := s.extractTar(ctx, archive, stage, budget); err != nil {
		return nil, err
	}
	parent, err := strictLstat(destination.root, filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	identity := parent.Sys().(*syscall.Stat_t)
	if err := reownTree(ctx, stage, ".", identity); err != nil {
		return nil, err
	}
	if err := budget.applyDirectoryModes(ctx, stage); err != nil {
		return nil, err
	}
	rootDirectory, err := stage.Open(".")
	if err != nil {
		return nil, err
	}
	mode := destination.spec.DefaultDirMode
	if in.Mode != nil {
		mode = fs.FileMode(*in.Mode)
	}
	err = rootDirectory.Chmod(mode)
	rootDirectory.Close()
	if err != nil {
		return nil, err
	}
	err = renameBetweenNoReplace(s.state, stageName, destination.root, path)
	if errors.Is(err, syscall.EXDEV) {
		_, err = s.copyAcrossDevices(ctx, s.privateManagedRoot(), stageName, destination, path, false)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": path, "bytes": budget.bytes, "entries": budget.entries, "sha256": digest, "actor_id": actorID}, nil
}

func reownTree(ctx context.Context, root *os.Root, path string, identity *syscall.Stat_t) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := strictOpen(root, path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		entries, err := file.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := reownTree(ctx, root, filepath.Join(path, entry.Name()), identity); err != nil {
				return err
			}
		}
	}
	return setCopiedMetadata(file, identity, info.Mode().Perm())
}
