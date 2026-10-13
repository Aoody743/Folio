package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func (s *Service) UploadBegin(ctx context.Context, actorID string, in Input) (Upload, error) {
	if actorID == "" {
		return Upload{}, failure("unauthorized", "Authenticated actor required", 401)
	}
	if in.Size < 0 || in.Size > s.options.MaxTransferBytes {
		return Upload{}, failure("too_large", "Invalid upload size", 413)
	}
	if in.SHA256 != "" && !digestPattern.MatchString(in.SHA256) {
		return Upload{}, failure("invalid_input", "Invalid SHA-256", 400)
	}
	if in.Mode != nil && *in.Mode > 0777 {
		return Upload{}, failure("invalid_input", "Special copy mode bits are forbidden", 400)
	}
	root, path, err := s.resolve(Ref{in.Root, in.Path}, true)
	if err != nil {
		return Upload{}, err
	}
	if err := s.checkExpected(ctx, Ref{in.Root, path}, in.ExpectedHash, in.Overwrite); err != nil {
		return Upload{}, err
	}
	if in.Overwrite && in.ExpectedHash == "" {
		in.ExpectedHash, err = s.hash(ctx, Ref{in.Root, path})
		if err != nil {
			return Upload{}, err
		}
	}
	session := Upload{ID: newID(), ActorID: actorID, Root: in.Root, Path: path, Size: in.Size, ExpectedHash: in.ExpectedHash, SHA256: in.SHA256, ExpiresAt: time.Now().UTC().Add(time.Hour), State: "uploading"}
	if in.ExpectedHash == "" && !in.Overwrite {
		session.CopyMode = in.Mode
	}
	identity, err := strictLstat(root.root, ".")
	if err != nil {
		return Upload{}, err
	}
	stat := identity.Sys().(*syscall.Stat_t)
	session.RootDevice, session.RootInode = uint64(stat.Dev), stat.Ino
	file, err := s.state.OpenFile(filepath.Join("uploads", session.ID+".part"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return Upload{}, err
	}
	file.Close()
	if err := s.store.Save(ctx, "file_uploads", session.ID, encode(session)); err != nil {
		s.state.Remove(filepath.Join("uploads", session.ID+".part"))
		return Upload{}, err
	}
	return session, nil
}
func (s *Service) UploadStatus(ctx context.Context, actorID, id string) (Upload, error) {
	if !safeID.MatchString(id) {
		return Upload{}, failure("invalid_input", "Invalid upload ID", 400)
	}
	raw, err := s.store.Get(ctx, "file_uploads", id)
	if err != nil {
		return Upload{}, err
	}
	var session Upload
	if err := json.Unmarshal(raw, &session); err != nil {
		return Upload{}, err
	}
	if session.ActorID != actorID {
		return Upload{}, failure("forbidden", "Upload belongs to another actor", 403)
	}
	return session, nil
}

// UploadChunk appends at a verified offset. It consumes at most four MiB plus
// one byte, never allocates the full upload, and rolls back a rejected chunk.
func (s *Service) UploadChunk(ctx context.Context, actorID, id string, offset int64, reader io.Reader, checksum string) (Upload, error) {
	session, err := s.UploadStatus(ctx, actorID, id)
	if err != nil {
		return Upload{}, err
	}
	unlock, err := s.lock(ctx, session.Root)
	if err != nil {
		return Upload{}, err
	}
	defer unlock()
	session, err = s.UploadStatus(ctx, actorID, id)
	if err != nil {
		return Upload{}, err
	}
	if session.State != "uploading" || time.Now().After(session.ExpiresAt) {
		return Upload{}, failure("conflict", "Upload is not active", 409)
	}
	if err := s.checkUploadRoot(session); err != nil {
		return Upload{}, err
	}
	if offset != session.Offset {
		return Upload{}, &Error{Code: "conflict", Message: "Upload offset changed", Status: 409, Details: map[string]any{"offset": session.Offset}}
	}
	if checksum != "" && !digestPattern.MatchString(checksum) {
		return Upload{}, failure("invalid_input", "Invalid chunk SHA-256", 400)
	}
	path := filepath.Join("uploads", id+".part")
	file, info, err := openRegular(s.state, path, os.O_RDWR)
	if err != nil {
		return Upload{}, err
	}
	defer file.Close()
	if info.Size() != offset {
		return Upload{}, failure("conflict", "Upload was interrupted; restart it before continuing", 409)
	}
	if _, err := file.Seek(offset, 0); err != nil {
		return Upload{}, err
	}
	hash := sha256.New()
	limit := min(int64(4<<20), session.Size-offset)
	n, err := io.CopyBuffer(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, reader}, limit+1), make([]byte, 32<<10))
	if err == nil && n > limit {
		err = failure("too_large", "Chunk exceeds remaining size or four MiB budget", 413)
	}
	if err == nil && checksum != "" && checksum != hex.EncodeToString(hash.Sum(nil)) {
		err = failure("checksum_mismatch", "Upload chunk checksum mismatch", 400)
	}
	if err != nil {
		file.Truncate(offset)
		file.Sync()
		return Upload{}, err
	}
	if err := file.Sync(); err != nil {
		return Upload{}, err
	}
	session.Offset += n
	if err := s.store.Save(ctx, "file_uploads", id, encode(session)); err != nil {
		file.Truncate(offset)
		file.Sync()
		return Upload{}, err
	}
	return session, nil
}
func (s *Service) UploadCommit(ctx context.Context, actorID string, in Input) (any, error) {
	session, err := s.UploadStatus(ctx, actorID, in.ID)
	if err != nil {
		return nil, err
	}
	if in.Root != "" && in.Root != session.Root {
		return nil, failure("forbidden", "Upload root mismatch", 403)
	}
	unlock, err := s.lock(ctx, session.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	session, err = s.UploadStatus(ctx, actorID, in.ID)
	if err != nil {
		return nil, err
	}
	if session.State == "completed" {
		return session, nil
	}
	if err := s.checkUploadRoot(session); err != nil {
		return nil, err
	}
	if session.Offset != session.Size {
		return nil, failure("conflict", "Upload is incomplete", 409)
	}
	if session.State != "uploading" && session.State != "committing" {
		return nil, failure("conflict", "Upload is not active", 409)
	}
	file, _, err := openRegular(s.state, filepath.Join("uploads", session.ID+".part"), os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash, _, err := hashReader(ctx, file)
	if err != nil {
		return nil, err
	}
	if session.SHA256 != "" && session.SHA256 != hash {
		return nil, failure("checksum_mismatch", "Upload checksum mismatch", 400)
	}
	if in.SHA256 != "" && (!digestPattern.MatchString(in.SHA256) || in.SHA256 != hash) {
		return nil, failure("checksum_mismatch", "Commit digest does not match the streamed artifact", 400)
	}
	ref := Ref{session.Root, session.Path}
	if session.State == "committing" {
		actual, err := s.hash(ctx, ref)
		if err != nil {
			return nil, err
		}
		if actual == hash {
			session.State = "completed"
			if err := s.store.Save(ctx, "file_uploads", session.ID, encode(session)); err != nil {
				return nil, err
			}
			s.state.Remove(filepath.Join("uploads", session.ID+".part"))
			return session, nil
		}
		return nil, failure("conflict", "Interrupted upload needs inspection before retry", 409)
	}
	if err := s.checkExpected(ctx, ref, session.ExpectedHash, false); err != nil {
		return nil, err
	}
	if _, err := s.snapshot(ctx, "external", ref, "utf-8"); err != nil {
		return nil, err
	}
	session.State, session.SHA256 = "committing", hash
	if err := s.store.Save(ctx, "file_uploads", session.ID, encode(session)); err != nil {
		return nil, err
	}
	file.Seek(0, 0)
	entry, _, err := s.writeAtomic(ctx, ref, file, s.options.MaxTransferBytes)
	if err != nil {
		return nil, err
	}
	if session.CopyMode != nil {
		root, path, err := s.resolve(ref, true)
		if err != nil {
			return nil, err
		}
		destination, _, err := openRegular(root.root, path, os.O_RDONLY)
		if err != nil {
			return nil, err
		}
		err = destination.Chmod(fs.FileMode(*session.CopyMode))
		destination.Close()
		if err != nil {
			return nil, err
		}
		entry.Mode = *session.CopyMode
	}
	if _, err := s.snapshot(ctx, actorID, ref, "utf-8"); err != nil {
		return nil, err
	}
	session.State = "completed"
	if err := s.store.Save(ctx, "file_uploads", session.ID, encode(session)); err != nil {
		return nil, err
	}
	s.state.Remove(filepath.Join("uploads", session.ID+".part"))
	return map[string]any{"upload": session, "entry": entry, "sha256": hash}, nil
}

func (s *Service) checkUploadRoot(session Upload) error {
	root, _, err := s.resolve(Ref{session.Root, session.Path}, true)
	if err != nil {
		return err
	}
	info, err := strictLstat(root.root, ".")
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if session.RootInode == 0 || session.RootInode != stat.Ino || session.RootDevice != uint64(stat.Dev) {
		return failure("conflict", "Filesystem root changed since upload creation; restart the upload", 409)
	}
	return nil
}
func (s *Service) UploadCancel(ctx context.Context, actorID string, in Input) (any, error) {
	session, err := s.UploadStatus(ctx, actorID, in.ID)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock(ctx, session.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	session, err = s.UploadStatus(ctx, actorID, in.ID)
	if err != nil {
		return nil, err
	}
	if session.State != "uploading" {
		return nil, failure("conflict", "Committed uploads cannot be canceled", 409)
	}
	if err := s.state.Remove(filepath.Join("uploads", session.ID+".part")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	session.State = "canceled"
	if err := s.store.Save(ctx, "file_uploads", session.ID, encode(session)); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *Service) validateDownloadURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return nil, failure("invalid_url", "A valid URL without embedded credentials is required", 400)
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && s.options.AllowHTTPDownloads) {
		return nil, failure("invalid_url", "Remote downloads require HTTPS", 400)
	}
	if len(s.options.DownloadHosts) > 0 {
		allowed := false
		for _, host := range s.options.DownloadHosts {
			if strings.EqualFold(host, parsed.Hostname()) {
				allowed = true
			}
		}
		if !allowed {
			return nil, failure("forbidden", "Download host is not allowed", 403)
		}
	}
	return parsed, nil
}

// A global-unicast address can still refer to cloud metadata or a special-use
// network. These ranges are never valid destinations for an untrusted URL.
var blockedDownloadNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func publicIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return false
	}
	for _, network := range blockedDownloadNetworks {
		if network.Contains(address) {
			return false
		}
	}
	return true
}
func (s *Service) RemoteDownload(ctx context.Context, actorID string, in Input) (any, error) {
	parsed, err := s.validateDownloadURL(in.URL)
	if err != nil {
		return nil, err
	}
	if in.SHA256 != "" && !digestPattern.MatchString(in.SHA256) {
		return nil, failure("invalid_input", "Invalid expected download digest", 400)
	}
	unlock, err := s.lock(ctx, in.Root)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ref := Ref{in.Root, in.Path}
	if _, _, err := s.resolve(ref, true); err != nil {
		return nil, err
	}
	if err := s.checkExpected(ctx, ref, in.ExpectedHash, in.Overwrite); err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, DisableCompression: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !s.options.AllowPrivateDownloads && !publicIP(ip) {
				return nil, failure("forbidden", "Private/link-local network targets are blocked", 403)
			}
		}
		var last error
		for _, ip := range ips {
			conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			last = err
		}
		return nil, last
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 30 * time.Minute}
	if s.options.DownloadClient != nil {
		client = *s.options.DownloadClient
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("too many redirects")
		}
		_, err := s.validateDownloadURL(req.URL.String())
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, failure("download_failed", "Remote download failed or network destination was blocked", 502)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, failure("download_failed", "Remote server did not return HTTP 200", 502)
	}
	if response.ContentLength > s.options.MaxTransferBytes {
		return nil, failure("too_large", "Remote file exceeds transfer budget", 413)
	}
	name := filepath.Join("staging", newID()+".download")
	file, err := s.state.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	defer s.state.Remove(name)
	hash := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(file, hash), io.LimitReader(contextReader{ctx, response.Body}, s.options.MaxTransferBytes+1), make([]byte, 32<<10))
	if err != nil {
		return nil, err
	}
	if n > s.options.MaxTransferBytes {
		return nil, failure("too_large", "Remote file exceeds transfer budget", 413)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if in.SHA256 != "" && digest != in.SHA256 {
		return nil, failure("checksum_mismatch", "Remote download checksum mismatch", 400)
	}
	if _, err := s.snapshot(ctx, "external", ref, "utf-8"); err != nil {
		return nil, err
	}
	file.Seek(0, 0)
	entry, _, err := s.writeAtomic(ctx, ref, file, s.options.MaxTransferBytes)
	if err != nil {
		return nil, err
	}
	if _, err := s.snapshot(ctx, actorID, ref, "utf-8"); err != nil {
		return nil, err
	}
	return map[string]any{"entry": entry, "sha256": digest}, nil
}
