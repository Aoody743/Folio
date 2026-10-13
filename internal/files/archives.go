package files

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ulikunitz/xz"
)

type limitedLog struct{ data []byte }

func (b *limitedLog) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data) < 4096 {
		b.data = append(b.data, p[:min(len(p), 4096-len(b.data))]...)
	}
	return n, nil
}
func (s *Service) archiveCommand(ctx context.Context, tool string, args ...string) (*exec.Cmd, error) {
	path := s.options.ArchiveTools[tool]
	if path == "" {
		defaults := map[string][]string{"xz": {"/usr/bin/xz"}, "bzip2": {"/usr/bin/bzip2"}, "7z": {"/usr/bin/7zz", "/usr/bin/7z"}, "rar": {"/usr/bin/rar"}}
		for _, candidate := range defaults[tool] {
			if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
				path = candidate
				break
			}
		}
	}
	if path == "" || !filepath.IsAbs(path) {
		return nil, failure("unsupported_format", "Trusted archive tool is unavailable: "+tool, 422)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0002 != 0 {
		return nil, failure("unsupported_format", "Archive tool is not a trusted executable", 422)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C.UTF-8", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 500 * time.Millisecond
	cmd.Stderr = &limitedLog{}
	return cmd, nil
}
func normalizeFormat(format string) string {
	switch strings.ToLower(format) {
	case "tgz", "tar_gz":
		return "tar.gz"
	case "xz":
		return "tar.xz"
	case "bz2":
		return "tar.bz2"
	}
	return strings.ToLower(format)
}
func (s *Service) ArchiveFormats() []map[string]any {
	out := []map[string]any{}
	for _, format := range []string{"zip", "tar", "tar.gz", "gz", "tar.bz2", "tar.xz", "7z", "rar"} {
		read, write := true, true
		switch format {
		case "tar.bz2":
			_, err := s.archiveCommand(context.Background(), "bzip2")
			write = err == nil
		case "7z":
			_, err := s.archiveCommand(context.Background(), "7z")
			read, write = err == nil, err == nil
		case "rar":
			_, readErr := s.archiveCommand(context.Background(), "7z")
			_, writeErr := s.archiveCommand(context.Background(), "rar")
			read, write = readErr == nil, writeErr == nil
		}
		out = append(out, map[string]any{"format": format, "extract": read, "create": write})
	}
	return out
}
func archivePath(name string) (string, error) {
	name = strings.TrimPrefix(name, "./")
	if name == "" || name == "." {
		return ".", nil
	}
	path, err := clean(name)
	if err != nil {
		return "", err
	}
	if strings.Contains(name, ":") || strings.ContainsAny(name, "\r\n") {
		return "", failure("unsafe_archive", "Archive entry has an unsafe name", 403)
	}
	return path, nil
}

type archiveBudget struct {
	bytes       int64
	entries     int
	directories map[string]fs.FileMode
}

func (b *archiveBudget) directory(name string, mode fs.FileMode) {
	if b.directories == nil {
		b.directories = make(map[string]fs.FileMode)
	}
	b.directories[name] = mode.Perm()
}

// Populate private extraction directories while they are writable. Apply the
// archive's exact permission bits after all children and ownership are ready,
// deepest first, so umask and read-only parent modes cannot alter the result.
func (b *archiveBudget) applyDirectoryModes(ctx context.Context, root *os.Root) error {
	paths := make([]string, 0, len(b.directories))
	for path := range b.directories {
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool {
		if a, z := strings.Count(paths[i], "/"), strings.Count(paths[j], "/"); a != z {
			return a > z
		}
		return paths[i] > paths[j]
	})
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		directory, err := strictOpen(root, path, os.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return err
		}
		err = directory.Chmod(b.directories[path])
		directory.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) extractFile(ctx context.Context, root *os.Root, name string, reader io.Reader, size int64, mode fs.FileMode, budget *archiveBudget) error {
	path, err := archivePath(name)
	if err != nil {
		return err
	}
	if path == "." {
		return nil
	}
	budget.entries++
	if budget.entries > s.options.MaxArchiveEntries || size < 0 || size > s.options.MaxTransferBytes-budget.bytes {
		return failure("too_large", "Archive exceeds entry or extracted-size budget", 413)
	}
	if err := root.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	n, err := io.CopyBuffer(file, io.LimitReader(contextReader{ctx, reader}, size+1), make([]byte, 32<<10))
	if err == nil && n != size {
		err = failure("invalid_archive", "Archive entry size mismatch", 400)
	}
	if err == nil {
		err = file.Chmod(mode.Perm() & 0777)
	}
	err = errors.Join(err, file.Close())
	budget.bytes += n
	return err
}

func (s *Service) extractUnknownFile(ctx context.Context, root *os.Root, name string, reader io.Reader, budget *archiveBudget) error {
	path, err := archivePath(name)
	if err != nil {
		return err
	}
	if path == "." {
		return failure("unsafe_archive", "gzip output needs a regular filename", 403)
	}
	budget.entries++
	if budget.entries > s.options.MaxArchiveEntries {
		return failure("too_large", "Archive exceeds entry budget", 413)
	}
	if err := root.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	limit := s.options.MaxTransferBytes - budget.bytes
	n, err := io.CopyBuffer(file, io.LimitReader(contextReader{ctx, reader}, limit+1), make([]byte, 32<<10))
	if err != nil {
		return err
	}
	if n > limit {
		return failure("too_large", "Decompressed gzip exceeds transfer budget", 413)
	}
	budget.bytes += n
	if err := file.Chmod(0644); err != nil {
		return err
	}
	return file.Sync()
}

func (s *Service) finishCompressedStream(ctx context.Context, reader io.Reader) error {
	// Force the compression trailer/checksum to be read after tar's end marker.
	// A malformed stream cannot hide corruption behind valid tar headers.
	n, err := io.CopyBuffer(io.Discard, io.LimitReader(contextReader{ctx, reader}, s.options.MaxTransferBytes+1), make([]byte, 32<<10))
	if err == nil && n > s.options.MaxTransferBytes {
		return failure("too_large", "Compression trailing data exceeds transfer budget", 413)
	}
	return err
}
func (s *Service) extractTar(ctx context.Context, reader io.Reader, root *os.Root, budget *archiveBudget) error {
	tr := tar.NewReader(contextReader{ctx, reader})
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		path, err := archivePath(header.Name)
		if err != nil {
			return err
		}
		if path == "." {
			// Only a root-directory marker may be ignored. Skipping a regular
			// entry here lets tar.Next drain arbitrary decompressed payloads
			// without charging the extraction budget.
			if header.Typeflag != tar.TypeDir || header.Size != 0 {
				return failure("unsafe_archive", "Archive root markers must be empty directories", 403)
			}
			budget.entries++
			if budget.entries > s.options.MaxArchiveEntries {
				return failure("too_large", "Archive exceeds entry budget", 413)
			}
			continue
		}
		switch header.Typeflag {
		case tar.TypeDir:
			budget.entries++
			if budget.entries > s.options.MaxArchiveEntries {
				return failure("too_large", "Archive exceeds entry budget", 413)
			}
			if err := root.MkdirAll(path, 0700); err != nil {
				return err
			}
			budget.directory(path, fs.FileMode(header.Mode))
		case tar.TypeReg, tar.TypeRegA:
			if err := s.extractFile(ctx, root, path, tr, header.Size, fs.FileMode(header.Mode), budget); err != nil {
				return err
			}
		default:
			return failure("unsafe_archive", "Links, devices and special archive entries are not extracted", 403)
		}
	}
}
func (s *Service) Extract(ctx context.Context, in Input) (any, error) {
	source, sourcePath, err := s.resolve(Ref{in.Root, in.Path}, false)
	if err != nil {
		return nil, err
	}
	dest, destPath, err := s.resolve(Ref{in.DestinationRoot, in.DestinationPath}, false)
	if err != nil {
		return nil, err
	}
	if dest.spec.ReadOnly {
		return nil, failure("read_only", "Destination root is read-only", 403)
	}
	if err := s.guardRootPath(dest, destPath, true); err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx, in.Root, in.DestinationRoot)
	if err != nil {
		return nil, err
	}
	defer unlock()
	input, info, err := openRegular(source.root, sourcePath, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	if info.Size() > s.options.MaxTransferBytes {
		return nil, failure("too_large", "Archive exceeds compressed-size budget", 413)
	}
	stageName := filepath.Join("staging", newID())
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
	format := normalizeFormat(in.Format)
	if format == "" {
		for _, candidate := range []string{"tar.gz", "tar.xz", "tar.bz2", "zip", "7z", "rar", "tar", "gz"} {
			if strings.HasSuffix(strings.ToLower(sourcePath), "."+candidate) {
				format = candidate
				break
			}
		}
	}
	switch format {
	case "zip":
		frozen, size, cleanup, err := s.freezeArchive(ctx, input, ".zip-input")
		if err != nil {
			return nil, err
		}
		defer cleanup()
		if err := s.preflightZIP(ctx, frozen, size); err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(frozen, size)
		if err != nil {
			return nil, err
		}
		if len(zr.File) > s.options.MaxArchiveEntries {
			return nil, failure("too_large", "Archive exceeds entry budget", 413)
		}
		for _, entry := range zr.File {
			path, err := archivePath(entry.Name)
			if err != nil {
				return nil, err
			}
			if path == "." {
				continue
			}
			if entry.Mode()&fs.ModeSymlink != 0 {
				return nil, failure("unsafe_archive", "Archive links are forbidden", 403)
			}
			if entry.FileInfo().IsDir() {
				budget.entries++
				if err := stage.MkdirAll(path, 0700); err != nil {
					return nil, err
				}
				budget.directory(path, entry.Mode())
				continue
			}
			if entry.UncompressedSize64 > uint64(s.options.MaxTransferBytes) {
				return nil, failure("too_large", "Archive entry exceeds extraction budget", 413)
			}
			reader, err := entry.Open()
			if err != nil {
				return nil, err
			}
			err = s.extractFile(ctx, stage, path, reader, int64(entry.UncompressedSize64), entry.Mode(), budget)
			reader.Close()
			if err != nil {
				return nil, err
			}
		}
	case "tar":
		err = s.extractTar(ctx, input, stage, budget)
	case "tar.gz", "gz":
		var reader *gzip.Reader
		reader, err = gzip.NewReader(input)
		if err == nil {
			if format == "tar.gz" {
				err = s.extractTar(ctx, reader, stage, budget)
				if err == nil {
					err = s.finishCompressedStream(ctx, reader)
				}
			} else {
				name := reader.Name
				if name == "" {
					name = strings.TrimSuffix(filepath.Base(sourcePath), ".gz")
				}
				err = s.extractUnknownFile(ctx, stage, name, reader, budget)
			}
			reader.Close()
		}
	case "tar.bz2":
		reader := bzip2.NewReader(input)
		err = s.extractTar(ctx, reader, stage, budget)
		if err == nil {
			err = s.finishCompressedStream(ctx, reader)
		}
	case "tar.xz":
		// A public archive can be rewritten by its owner while it is decoded.
		// Freeze the bytes in private state before metadata preflight so the
		// checked dictionary cannot change between validation and allocation.
		snapshotName := filepath.Join("staging", newID()+".xz-input")
		snapshot, copyErr := s.state.OpenFile(snapshotName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if copyErr != nil {
			return nil, copyErr
		}
		defer snapshot.Close()
		defer s.state.Remove(snapshotName)
		n, copyErr := io.CopyBuffer(snapshot, io.LimitReader(contextReader{ctx, input}, s.options.MaxTransferBytes+1), make([]byte, 32<<10))
		if copyErr != nil {
			return nil, copyErr
		}
		if n > s.options.MaxTransferBytes {
			return nil, failure("too_large", "Compressed XZ exceeds transfer budget", 413)
		}
		snapshot.Seek(0, io.SeekStart)
		input = snapshot
		if err := s.preflightXZ(ctx, input, n); err != nil {
			return nil, err
		}
		var reader *xz.Reader
		reader, err = (xz.ReaderConfig{DictCap: 8 << 20}).NewReader(contextReader{ctx, input})
		if err == nil {
			err = s.extractTar(ctx, reader, stage, budget)
		}
		if err == nil {
			err = s.finishCompressedStream(ctx, reader)
		}
	case "7z", "rar":
		err = s.extractExternal(ctx, format, input, stage, budget)
	default:
		return nil, failure("unsupported_format", "Unsupported archive format", 422)
	}
	if err != nil {
		return nil, err
	}
	// Validate all entries and collision checks before touching the destination.
	destinationInfo, statError := strictLstat(dest.root, destPath)
	newDirectory := errors.Is(statError, fs.ErrNotExist)
	if statError != nil && !newDirectory {
		return nil, statError
	}
	if !newDirectory && !destinationInfo.IsDir() {
		return nil, failure("invalid_input", "Extraction destination must be a directory", 400)
	}
	ownerInfo := destinationInfo
	if newDirectory {
		ownerInfo, err = strictLstat(dest.root, filepath.Dir(destPath))
		if err != nil {
			return nil, err
		}
	}
	if err := reownTree(ctx, stage, ".", ownerInfo.Sys().(*syscall.Stat_t)); err != nil {
		return nil, err
	}
	if err := budget.applyDirectoryModes(ctx, stage); err != nil {
		return nil, err
	}
	if newDirectory {
		directory, err := stage.Open(".")
		if err != nil {
			return nil, err
		}
		err = directory.Chmod(dest.spec.DefaultDirMode)
		directory.Close()
		if err != nil {
			return nil, err
		}
		err = renameBetweenNoReplace(s.state, stageName, dest.root, destPath)
		if errors.Is(err, syscall.EXDEV) {
			_, err = s.copyAcrossDevices(ctx, s.privateManagedRoot(), stageName, dest, destPath, false)
		}
		if err != nil {
			return nil, err
		}
	} else {
		entries, err := stage.Open(".")
		if err != nil {
			return nil, err
		}
		list, err := entries.ReadDir(-1)
		entries.Close()
		if err != nil {
			return nil, err
		}
		for _, entry := range list {
			target := filepath.Join(destPath, entry.Name())
			if err := s.guardRootPath(dest, target, true); err != nil {
				return nil, err
			}
			if _, err := strictLstat(dest.root, target); err == nil {
				return nil, failure("conflict", "Extraction destination contains an archive entry", 409)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
		for _, entry := range list {
			target := filepath.Join(destPath, entry.Name())
			err := renameBetweenNoReplace(stage, entry.Name(), dest.root, target)
			if errors.Is(err, syscall.EXDEV) {
				source := &managedRoot{root: stage, spec: RootSpec{Path: filepath.Join(s.options.StateDir, stageName)}}
				_, err = s.copyAcrossDevices(ctx, source, entry.Name(), dest, target, false)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return map[string]any{"path": destPath, "entries": budget.entries, "bytes": budget.bytes}, nil
}
func (s *Service) extractExternal(ctx context.Context, format string, input *os.File, stage *os.Root, budget *archiveBudget) error {
	cmd, err := s.archiveCommand(ctx, "7z", "l", "-slt", "-sccUTF-8", "-t"+format, "--", "/proc/self/fd/3")
	if err != nil {
		return err
	}
	input.Seek(0, 0)
	cmd.ExtraFiles = []*os.File{input}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	listing, err := io.ReadAll(io.LimitReader(contextReader{ctx, output}, 4<<20+1))
	output.Close()
	if err != nil || len(listing) > 4<<20 {
		cmd.Cancel()
		cmd.Wait()
		return failure("too_large", "Archive index exceeds budget", 413)
	}
	if err := cmd.Wait(); err != nil {
		return err
	}
	text := string(listing)
	separator := strings.Index(text, "----------")
	if separator < 0 {
		return failure("invalid_archive", "Archive tool did not return a structured index", 400)
	}
	for _, block := range strings.Split(text[separator+10:], "\n\n") {
		fields := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, ok := strings.Cut(strings.TrimSuffix(line, "\r"), " = ")
			if ok {
				fields[key] = value
			}
		}
		name := fields["Path"]
		if name == "" {
			continue
		}
		path, err := archivePath(name)
		if err != nil {
			return err
		}
		if fields["Symbolic Link"] != "" || fields["Hard Link"] != "" || fields["Encrypted"] == "+" {
			return failure("unsafe_archive", "Linked or encrypted entries are forbidden", 403)
		}
		if fields["Folder"] == "+" || strings.HasPrefix(fields["Attributes"], "D") {
			budget.entries++
			if budget.entries > s.options.MaxArchiveEntries {
				return failure("too_large", "Archive has too many entries", 413)
			}
			if err := stage.MkdirAll(path, 0750); err != nil {
				return err
			}
			continue
		}
		size, err := strconv.ParseInt(fields["Size"], 10, 64)
		if err != nil {
			return err
		}
		input.Seek(0, 0)
		decoder, err := s.archiveCommand(ctx, "7z", "x", "-so", "-spd", "-bd", "-t"+format, "--", "/proc/self/fd/3", name)
		if err != nil {
			return err
		}
		decoder.ExtraFiles = []*os.File{input}
		reader, err := decoder.StdoutPipe()
		if err != nil {
			return err
		}
		if err := decoder.Start(); err != nil {
			return err
		}
		err = s.extractFile(ctx, stage, path, reader, size, 0644, budget)
		reader.Close()
		if err != nil {
			decoder.Cancel()
		}
		waitErr := decoder.Wait()
		if err != nil {
			return err
		}
		if waitErr != nil {
			return waitErr
		}
	}
	return nil
}
func (s *Service) Archive(ctx context.Context, in Input) (any, error) {
	source, base, err := s.resolve(Ref{in.Root, in.Path}, false)
	if err != nil {
		return nil, err
	}
	dest := Ref{in.DestinationRoot, in.DestinationPath}
	if _, _, err := s.resolve(dest, true); err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx, in.Root, in.DestinationRoot)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.checkExpected(ctx, dest, in.ExpectedHash, in.Overwrite); err != nil {
		return nil, err
	}
	format := normalizeFormat(in.Format)
	if format == "7z" || format == "rar" {
		return s.archiveExternal(ctx, in, format, source, base, dest)
	}
	name := filepath.Join("staging", newID()+".archive")
	file, err := s.state.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	defer s.state.Remove(name)
	var writer io.Writer = file
	var closeCompression func() error
	var process *exec.Cmd
	switch format {
	case "zip", "tar":
	case "tar.gz", "gz":
		gz := gzip.NewWriter(file)
		writer = gz
		closeCompression = gz.Close
	case "tar.xz":
		xzWriter, createErr := (xz.WriterConfig{DictCap: 1 << 20, BufSize: 64 << 10}).NewWriter(file)
		if createErr != nil {
			return nil, createErr
		}
		writer = xzWriter
		closeCompression = xzWriter.Close
	case "tar.bz2":
		process, err = s.archiveCommand(ctx, "bzip2", "-c")
		if err != nil {
			return nil, err
		}
		process.Stdout = file
		pipe, err := process.StdinPipe()
		if err != nil {
			return nil, err
		}
		if err := process.Start(); err != nil {
			return nil, err
		}
		writer = pipe
		closeCompression = func() error { err := pipe.Close(); return errors.Join(err, process.Wait()) }
	default:
		return nil, failure("unsupported_format", "Unsupported archive creation format", 422)
	}
	budget := &archiveBudget{}
	var zw *zip.Writer
	var tw *tar.Writer
	if format == "zip" {
		zw = zip.NewWriter(writer)
	} else if format != "gz" {
		tw = tar.NewWriter(writer)
	}
	items := append([]string(nil), in.Items...)
	if len(items) == 0 {
		items = []string{base}
	} else {
		for i, item := range items {
			path, err := clean(item)
			if err != nil {
				return nil, err
			}
			items[i] = filepath.Join(base, path)
		}
	}
	var add func(string, string) error
	add = func(path, name string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.guardRootPath(source, path, false); err != nil {
			return err
		}
		if in.Root == in.DestinationRoot && path == in.DestinationPath {
			return nil
		}
		info, err := strictLstat(source.root, path)
		if err != nil {
			return err
		}
		budget.entries++
		if budget.entries > s.options.MaxArchiveEntries {
			return failure("too_large", "Archive exceeds entry budget", 413)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return failure("unsafe_archive", "Archive creation cannot follow symbolic links", 403)
		}
		if info.IsDir() {
			if format == "gz" {
				return failure("invalid_input", "gzip requires one regular file", 400)
			}
			if name != "." {
				if zw != nil {
					header, err := zip.FileInfoHeader(info)
					if err != nil {
						return err
					}
					header.Name = filepath.ToSlash(name) + "/"
					if _, err := zw.CreateHeader(header); err != nil {
						return err
					}
				} else {
					header, err := tar.FileInfoHeader(info, "")
					if err != nil {
						return err
					}
					header.Name = filepath.ToSlash(name) + "/"
					if err := tw.WriteHeader(header); err != nil {
						return err
					}
				}
			}
			dir, err := strictOpen(source.root, path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			defer dir.Close()
			entries, err := dir.ReadDir(-1)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := add(filepath.Join(path, e.Name()), filepath.Join(name, e.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		reader, info, err := openRegular(source.root, path, os.O_RDONLY)
		if err != nil {
			return err
		}
		defer reader.Close()
		budget.bytes += info.Size()
		if budget.bytes > s.options.MaxTransferBytes {
			return failure("too_large", "Archive source exceeds transfer budget", 413)
		}
		var sink io.Writer = writer
		if zw != nil {
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			header.Name = filepath.ToSlash(name)
			header.Method = zip.Deflate
			sink, err = zw.CreateHeader(header)
			if err != nil {
				return err
			}
		} else if tw != nil {
			header, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			header.Name = filepath.ToSlash(name)
			if err := tw.WriteHeader(header); err != nil {
				return err
			}
			sink = tw
		}
		n, err := io.CopyBuffer(sink, contextReader{ctx, io.LimitReader(reader, info.Size()+1)}, make([]byte, 32<<10))
		if err == nil && n != info.Size() {
			err = failure("conflict", "Source changed during archive creation", 409)
		}
		return err
	}
	for _, path := range items {
		if err := add(path, filepath.Base(path)); err != nil {
			if process != nil {
				process.Cancel()
				process.Wait()
			}
			return nil, err
		}
	}
	if zw != nil {
		err = zw.Close()
	}
	if tw != nil {
		err = tw.Close()
	}
	if closeCompression != nil {
		err = errors.Join(err, closeCompression())
	}
	if err != nil {
		return nil, err
	}
	file.Seek(0, 0)
	entry, hash, err := s.writeAtomic(ctx, dest, file, s.options.MaxTransferBytes)
	if err != nil {
		return nil, err
	}
	return map[string]any{"entry": entry, "sha256": hash, "entries": budget.entries, "bytes": budget.bytes}, nil
}
func (s *Service) archiveExternal(ctx context.Context, in Input, format string, source *managedRoot, base string, dest Ref) (any, error) {
	tool := "7z"
	if format == "rar" {
		tool = "rar"
	}
	if _, err := s.archiveCommand(ctx, tool); err != nil {
		return nil, err
	}
	stageName := filepath.Join("staging", newID())
	if err := s.state.Mkdir(stageName, 0700); err != nil {
		return nil, err
	}
	defer s.state.RemoveAll(stageName)
	stage, err := s.state.OpenRoot(stageName)
	if err != nil {
		return nil, err
	}
	defer stage.Close()
	budget := &archiveBudget{}
	var copyPath func(string, string) error
	copyPath = func(from, to string) error {
		if err := s.guardRootPath(source, from, false); err != nil {
			return err
		}
		info, err := strictLstat(source.root, from)
		if err != nil {
			return err
		}
		budget.entries++
		if budget.entries > s.options.MaxArchiveEntries {
			return failure("too_large", "Archive has too many entries", 413)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return failure("unsafe_archive", "Archive cannot follow links", 403)
		}
		if info.IsDir() {
			if err := stage.MkdirAll(to, 0700); err != nil {
				return err
			}
			dir, err := strictOpen(source.root, from, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
			if err != nil {
				return err
			}
			defer dir.Close()
			entries, err := dir.ReadDir(-1)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := copyPath(filepath.Join(from, e.Name()), filepath.Join(to, e.Name())); err != nil {
					return err
				}
			}
			return nil
		}
		file, info, err := openRegular(source.root, from, os.O_RDONLY)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := s.extractFile(ctx, stage, to, file, info.Size(), info.Mode(), budget); err != nil {
			return err
		}
		return nil
	}
	items := append([]string(nil), in.Items...)
	if len(items) == 0 {
		items = []string{base}
	} else {
		for i, item := range items {
			path, err := clean(item)
			if err != nil {
				return nil, err
			}
			items[i] = filepath.Join(base, path)
		}
	}
	names := []string{}
	for _, path := range items {
		name := filepath.Base(path)
		if name == "." {
			name = "contents"
		}
		if err := copyPath(path, name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	archiveName := filepath.Join("staging", newID()+"."+format)
	defer s.state.Remove(archiveName)
	archiveAbs := filepath.Join(s.options.StateDir, archiveName)
	args := []string{"a", "-t7z", "-mx=3", "-bd", "-y", "--", archiveAbs}
	if format == "rar" {
		args = []string{"a", "-r", "-idq", "--", archiveAbs}
	}
	args = append(args, names...)
	cmd, err := s.archiveCommand(ctx, tool, args...)
	if err != nil {
		return nil, err
	}
	cmd.Dir = filepath.Join(s.options.StateDir, stageName)
	cmd.Stdout = &limitedLog{}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	file, _, err := openRegular(s.state, archiveName, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entry, hash, err := s.writeAtomic(ctx, dest, file, s.options.MaxTransferBytes)
	if err != nil {
		return nil, err
	}
	return map[string]any{"entry": entry, "sha256": hash, "entries": budget.entries, "bytes": budget.bytes}, nil
}
