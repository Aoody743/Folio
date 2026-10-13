package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// Temporary roots are trusted implementation details. They are absent from
// discovery and rejected by all public Agent request leases.
func (s *Service) internalArtifactRoot() (*managedRoot, func(), error) {
	name := filepath.Join("staging", newID()+".artifact")
	if err := s.state.Mkdir(name, 0700); err != nil {
		return nil, nil, err
	}
	root, err := s.state.OpenRoot(name)
	if err != nil {
		s.state.RemoveAll(name)
		return nil, nil, err
	}
	id := "private-" + newID()
	managed := &managedRoot{spec: RootSpec{ID: id, Path: filepath.Join(s.options.StateDir, name), DefaultFileMode: 0600, DefaultDirMode: 0700, internal: true}, root: root, gate: make(chan struct{}, 1)}
	s.mu.Lock()
	s.roots[id] = managed
	s.mu.Unlock()
	cleanup := func() { s.mu.Lock(); delete(s.roots, id); s.mu.Unlock(); root.Close(); s.state.RemoveAll(name) }
	return managed, cleanup, nil
}

func (s *Service) streamArchive(ctx context.Context, in Input, output io.WriteCloser) (any, int64, error) {
	private, cleanup, err := s.internalArtifactRoot()
	if err != nil {
		return nil, 0, err
	}
	defer cleanup()
	request := in
	request.DestinationRoot, request.DestinationPath = private.spec.ID, "artifact"
	request.ExpectedHash = ""
	request.Overwrite = false
	data, err := s.Archive(ctx, request)
	if err != nil {
		return nil, 0, err
	}
	file, _, err := openRegular(private.root, "artifact", os.O_RDONLY)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()
	stop := context.AfterFunc(ctx, func() { output.Close(); file.Close() })
	defer stop()
	n, err := io.CopyBuffer(output, contextReader{ctx, file}, make([]byte, 32<<10))
	return data, n, err
}

func (s *Service) receiveArtifact(ctx context.Context, actorID string, in Input, reader io.Reader, extract bool) (any, error) {
	private, cleanup, err := s.internalArtifactRoot()
	if err != nil {
		return nil, err
	}
	defer cleanup()
	name := "artifact"
	if extract && len(in.Items) == 1 {
		name = filepath.Base(in.Items[0])
	}
	if name == "." || name == ".." {
		return nil, failure("invalid_input", "Invalid streamed artifact name", 400)
	}
	file, err := private.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, reader}, s.options.MaxTransferBytes+1), make([]byte, 32<<10))
	if err != nil {
		return nil, err
	}
	if n > s.options.MaxTransferBytes {
		return nil, failure("too_large", "Artifact exceeds transfer budget", 413)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if in.SHA256 != "" && digest != in.SHA256 {
		return nil, failure("checksum_mismatch", "Artifact digest mismatch", 400)
	}
	if extract {
		return s.Extract(ctx, Input{Root: private.spec.ID, Path: name, DestinationRoot: in.Root, DestinationPath: in.Path, Format: in.Format})
	}
	unlock, err := s.lock(ctx, in.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ref := Ref{in.Root, in.Path}
	if err := s.checkExpected(ctx, ref, in.ExpectedHash, in.Overwrite); err != nil {
		return nil, err
	}
	if _, err := s.snapshot(ctx, "external", ref, ""); err != nil {
		return nil, err
	}
	file.Seek(0, io.SeekStart)
	entry, diskHash, err := s.writeAtomic(ctx, ref, file, s.options.MaxTransferBytes)
	if err != nil {
		return nil, err
	}
	if _, err := s.snapshot(ctx, actorID, ref, ""); err != nil {
		return nil, err
	}
	return map[string]any{"entry": entry, "sha256": diskHash, "bytes": n}, nil
}
