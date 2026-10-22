package plugins

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ulikunitz/xz"
)

const maxArtifactBytes = int64(1 << 30)
const maxExtractedBytes = int64(4 << 30)
const maxArtifactFiles = 100000

func secureRuntimeTree(root string, gid uint32) error {
	return secureRuntimeTreeWithAccess(root, gid, false)
}

// Node.js archives are immutable shared executables used by isolated
// per-application users. Make their code readable and executable without
// giving those users access to the Agent's service group.
func secureApplicationRuntimeTree(root string, gid uint32) error {
	return secureRuntimeTreeWithAccess(root, gid, true)
}

func secureRuntimeTreeWithAccess(root string, gid uint32, publicRead bool) error {
	return filepath.WalkDir(root, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, resolveErr := filepath.EvalSymlinks(name)
			if resolveErr != nil {
				return failure("artifact_unsafe_entry", "Runtime archive contains a dangling or cyclic symbolic link", 412)
			}
			relative, relErr := filepath.Rel(root, resolved)
			if relErr != nil || !filepath.IsLocal(relative) {
				return failure("artifact_unsafe_entry", "Runtime archive symbolic link resolves outside its managed root", 412)
			}
			target, statErr := os.Stat(resolved)
			if statErr != nil || !target.Mode().IsRegular() && !target.IsDir() {
				return failure("artifact_unsafe_entry", "Runtime archive symbolic link target is not a regular file or directory", 412)
			}
			return nil
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return failure("artifact_unsafe_entry", "Runtime only permits regular files and directories", 412)
		}
		mode := os.FileMode(0640)
		if publicRead {
			mode = 0644
		}
		if info.IsDir() || info.Mode().Perm()&0111 != 0 {
			mode = 0750
			if publicRead {
				mode = 0755
			}
		}
		if err = os.Chmod(name, mode); err != nil {
			return err
		}
		if os.Geteuid() == 0 {
			return os.Chown(name, 0, int(gid))
		}
		return nil
	})
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

type artifactStore struct {
	dir    string
	client *http.Client
}

func newArtifactStore(dir string, client *http.Client) (*artifactStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := realDirectory(dir); err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	copyClient := *client
	if transport, ok := client.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		return nil, errors.New("artifact transport must verify TLS certificates")
	}
	copyClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !trustedHTTPS(request.URL.String()) {
			return errors.New("artifact redirect must remain verified HTTPS and bounded")
		}
		return nil
	}
	return &artifactStore{dir: dir, client: &copyClient}, nil
}

func (s *artifactStore) fetch(ctx context.Context, source Source) (string, error) {
	if source.Kind != "verified_archive" && source.Kind != "verified_source" || source.Trust != "pinned_sha256" || !artifactHash.MatchString(source.SHA256) || !trustedHTTPS(source.URL) {
		return "", failure("artifact_untrusted", "A verified official artifact hash and HTTPS source are required", 412)
	}
	name := filepath.Join(s.dir, source.SHA256)
	if exists, err := verifyFile(name, source.SHA256); err == nil && exists {
		return name, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source.URL, nil)
	if err != nil {
		return "", err
	}
	response, err := s.client.Do(request)
	if err != nil {
		return "", failure("artifact_download_failed", "Cannot download the trusted artifact with TLS verification", 502)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.ContentLength > maxArtifactBytes {
		return "", failure("artifact_download_failed", "Artifact response is unsuccessful or exceeds the size limit", 502)
	}
	file, err := os.CreateTemp(s.dir, ".download-*")
	if err != nil {
		return "", err
	}
	temp := file.Name()
	defer os.Remove(temp)
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxArtifactBytes+1))
	if err != nil || n > maxArtifactBytes {
		return "", failure("artifact_download_failed", "Artifact transfer failed or exceeded its size limit", 502)
	}
	if hex.EncodeToString(hash.Sum(nil)) != source.SHA256 {
		return "", failure("artifact_checksum_failed", "Artifact SHA-256 does not match the trusted catalog", 412)
	}
	if err = file.Sync(); err != nil {
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	if err = os.Chmod(temp, 0600); err != nil {
		return "", err
	}
	if err = os.Rename(temp, name); err != nil {
		return "", err
	}
	return name, nil
}

func verifyFile(name, digest string) (bool, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxArtifactBytes {
		return false, errors.New("artifact cache entry is invalid")
	}
	hash := sha256.New()
	if _, err = io.Copy(hash, io.LimitReader(file, maxArtifactBytes+1)); err != nil {
		return false, err
	}
	return hex.EncodeToString(hash.Sum(nil)) == digest, nil
}

func extractArtifact(ctx context.Context, fileName, target string, source Source) error {
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	if err := realDirectory(target); err != nil {
		return err
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer root.Close()
	var total int64
	count := 0
	type safeLink struct{ name, target string }
	links := []safeLink{}
	write := func(name, linkTarget string, mode os.FileMode, size int64, reader io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > maxArtifactFiles || size < 0 || total+size > maxExtractedBytes {
			return failure("artifact_extract_limit", "Artifact extraction exceeds its file or byte limits", 413)
		}
		total += size
		clean, err := archivePath(name, source.StripComponents)
		if err != nil {
			return err
		}
		if clean == "" {
			return nil
		}
		if mode&os.ModeDevice != 0 || mode&os.ModeNamedPipe != 0 || mode&os.ModeSocket != 0 || mode&os.ModeSetuid != 0 || mode&os.ModeSetgid != 0 {
			return failure("artifact_unsafe_entry", "Artifact contains a device or privileged-mode entry", 412)
		}
		if mode&os.ModeSymlink != 0 {
			if size != 0 || len(links) >= 1024 || !safeRelativeLinkTarget(clean, linkTarget) {
				return failure("artifact_unsafe_entry", "Artifact contains an unsafe or excessive symbolic link", 412)
			}
			links = append(links, safeLink{name: clean, target: linkTarget})
			return nil
		}
		if mode.IsDir() {
			return root.MkdirAll(clean, 0755)
		}
		if err = root.MkdirAll(path.Dir(clean), 0755); err != nil {
			return err
		}
		permission := os.FileMode(0644)
		if mode&0111 != 0 {
			permission = 0755
		}
		file, err := root.OpenFile(clean, os.O_WRONLY|os.O_CREATE|os.O_EXCL, permission)
		if err != nil {
			return err
		}
		n, err := io.CopyN(file, &contextReader{ctx: ctx, reader: reader}, size)
		if err == nil && n != size {
			err = io.ErrUnexpectedEOF
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		return err
	}
	finishLinks := func() error {
		// Create links only after regular entries, so an archive cannot use one
		// to redirect a later extraction write.
		for _, link := range links {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := root.Symlink(link.target, link.name); err != nil {
				return failure("artifact_unsafe_entry", "Artifact contains a duplicate or invalid symbolic link", 412)
			}
			resolved, err := filepath.EvalSymlinks(filepath.Join(target, link.name))
			if err != nil {
				return failure("artifact_unsafe_entry", "Artifact contains a dangling or cyclic symbolic link", 412)
			}
			relative, err := filepath.Rel(target, resolved)
			if err != nil || !filepath.IsLocal(relative) {
				return failure("artifact_unsafe_entry", "Artifact symbolic link resolves outside its staging root", 412)
			}
			info, err := os.Stat(resolved)
			if err != nil || !info.Mode().IsRegular() && !info.IsDir() {
				return failure("artifact_unsafe_entry", "Artifact symbolic link target is not a regular file or directory", 412)
			}
		}
		return nil
	}
	if source.ArchiveFormat == "zip" {
		archive, err := zip.OpenReader(fileName)
		if err != nil {
			return err
		}
		defer archive.Close()
		for _, entry := range archive.File {
			reader, err := entry.Open()
			if err != nil {
				return err
			}
			err = write(entry.Name, "", entry.Mode(), int64(entry.UncompressedSize64), reader)
			reader.Close()
			if err != nil {
				return err
			}
		}
		return finishLinks()
	}
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader
	switch source.ArchiveFormat {
	case "tar.gz":
		gz, err := gzip.NewReader(file)
		if err != nil {
			return err
		}
		defer gz.Close()
		reader = gz
	case "tar.xz":
		reader, err = xz.NewReader(file)
		if err != nil {
			return err
		}
	default:
		return failure("artifact_format_unsupported", "Unsupported verified archive format", 400)
	}
	archive := tar.NewReader(reader)
	for {
		entry, err := archive.Next()
		if err == io.EOF {
			return finishLinks()
		}
		if err != nil {
			return err
		}
		if entry.Typeflag != tar.TypeReg && entry.Typeflag != tar.TypeRegA && entry.Typeflag != tar.TypeDir && entry.Typeflag != tar.TypeSymlink {
			return failure("artifact_unsafe_entry", "Artifact links and device entries are not accepted", 412)
		}
		if err = write(entry.Name, entry.Linkname, entry.FileInfo().Mode(), entry.Size, archive); err != nil {
			return err
		}
	}
}

func safeRelativeLinkTarget(linkPath, rawTarget string) bool {
	if rawTarget == "" || len(rawTarget) > 4096 || strings.ContainsAny(rawTarget, "\\\x00:") || strings.HasPrefix(rawTarget, "/") || filepath.IsAbs(rawTarget) {
		return false
	}
	clean := path.Clean(path.Join(path.Dir(linkPath), rawTarget))
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, "../")
}

func archivePath(raw string, strip int) (string, error) {
	if raw == "" || len(raw) > 4096 || strings.ContainsAny(raw, "\\\x00") || strings.HasPrefix(raw, "/") || strings.Contains(raw, ":") {
		return "", failure("artifact_unsafe_path", "Artifact path is invalid or absolute", 412)
	}
	parts := strings.Split(strings.TrimSuffix(raw, "/"), "/")
	for _, part := range parts {
		if part == ".." {
			return "", failure("artifact_unsafe_path", "Artifact attempts directory traversal", 412)
		}
	}
	filtered := []string{}
	for _, part := range parts {
		if part != "" && part != "." {
			filtered = append(filtered, part)
		}
	}
	if len(filtered) <= strip {
		return "", nil
	}
	clean := path.Join(filtered[strip:]...)
	if !filepath.IsLocal(clean) {
		return "", failure("artifact_unsafe_path", "Artifact path escapes its root", 412)
	}
	return clean, nil
}

func realDirectory(name string) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("managed directory must not be a symlink: %s", name)
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	if resolved != absolute {
		return errors.New("managed directory ancestors must not contain symlinks")
	}
	return nil
}
