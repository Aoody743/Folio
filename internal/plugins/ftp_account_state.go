package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
	"golang.org/x/sys/unix"
)

const (
	pureFTPPauseJournalVersion = 1
	pureFTPPasswdName          = "pureftpd.passwd"
	pureFTPPasswdMaxBytes      = 4 << 20
	pureFTPPasswdMaxLineBytes  = 64 << 10
)

type FTPAccountStateInput struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type FTPAccountStateView struct {
	Name                    string   `json:"name"`
	ReadOnly                bool     `json:"read_only"`
	Enabled                 *bool    `json:"enabled,omitempty"`
	RecoveryRequired        bool     `json:"recovery_required"`
	Phase                   string   `json:"phase"`
	AllowedRecoveryCommands []string `json:"allowed_recovery_commands"`
}

type FTPAccountStateInspection struct {
	ID                      string   `json:"id"`
	Name                    string   `json:"name"`
	Revision                int64    `json:"revision"`
	LastConfirmedEnabled    *bool    `json:"last_confirmed_enabled,omitempty"`
	RecoveryRequired        bool     `json:"recovery_required"`
	Phase                   string   `json:"phase"`
	AllowedRecoveryCommands []string `json:"allowed_recovery_commands"`
}

type pureFTPPasswdStamp struct {
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	Size      int64  `json:"size"`
	MtimeSec  int64  `json:"mtime_sec"`
	MtimeNsec int64  `json:"mtime_nsec"`
	Mode      uint32 `json:"mode"`
	UID       uint32 `json:"uid"`
	GID       uint32 `json:"gid"`
}

type pureFTPPasswdSnapshot struct {
	Data         []byte
	SHA256       string
	Stamp        pureFTPPasswdStamp
	PasswordHash string
	FieldsSHA256 string
}

// The journal is private-encrypted by the Agent record store. It deliberately
// records only password verifiers, never the temporary password used to make
// the paused verifier.
type pureFTPAccountStateJournal struct {
	Version      int    `json:"version"`
	InstanceID   string `json:"instance_id"`
	Name         string `json:"name"`
	Phase        string `json:"phase"`
	OriginalHash string `json:"original_hash"`
	PausedHash   string `json:"paused_hash"`
	FieldsSHA256 string `json:"fields_sha256"`
}

func ftpPauseJournalID(instanceID, name string) string {
	digest := sha256.Sum256([]byte(instanceID + "\x00" + name))
	return "ftp_pause_" + hex.EncodeToString(digest[:16])
}

func ftpAccountEnabled(account utilityAccount) bool {
	// Records written before pause/resume existed have no ftp_enabled field.
	// Preserve their historical active behavior.
	return account.FTPEnabled == nil || *account.FTPEnabled
}

func ftpAccountEnabledValue(enabled bool) *bool { return &enabled }

func pureFTPFieldsFingerprint(fields []string) string {
	withoutPassword := make([]string, 0, len(fields)-1)
	withoutPassword = append(withoutPassword, fields[:1]...)
	withoutPassword = append(withoutPassword, fields[2:]...)
	digest := sha256.Sum256([]byte(strings.Join(withoutPassword, "\x00")))
	return hex.EncodeToString(digest[:])
}

func pureFTPAccountFields(data []byte, name string) (start, end int, passwordHash, fieldsSHA256 string, err error) {
	if !utilityName.MatchString(name) || len(data) > pureFTPPasswdMaxBytes {
		return 0, 0, "", "", errors.New("invalid PureFTP account file or name")
	}
	found := false
	for offset := 0; offset < len(data); {
		lineEnd := bytes.IndexByte(data[offset:], '\n')
		currentEnd := len(data)
		if lineEnd >= 0 {
			currentEnd = offset + lineEnd
		}
		line := data[offset:currentEnd]
		if len(line) > pureFTPPasswdMaxLineBytes {
			return 0, 0, "", "", errors.New("PureFTP account line exceeds its limit")
		}
		if len(line) > 0 && line[0] != '#' {
			fields := strings.Split(string(line), ":")
			if len(fields) < 18 || len(fields) > 64 {
				return 0, 0, "", "", errors.New("PureFTP account line has an unsupported format")
			}
			if fields[0] == name {
				if found || fields[1] == "" || strings.ContainsAny(fields[1], ":\x00\r\n") {
					return 0, 0, "", "", errors.New("PureFTP account entry is ambiguous")
				}
				found = true
				start, end = offset, currentEnd
				passwordHash = fields[1]
				fieldsSHA256 = pureFTPFieldsFingerprint(fields)
			}
		}
		if currentEnd == len(data) {
			break
		}
		offset = currentEnd + 1
	}
	if !found {
		return 0, 0, "", "", os.ErrNotExist
	}
	return start, end, passwordHash, fieldsSHA256, nil
}

func pureFTPReplaceHash(data []byte, name, expectedHash, newHash string) ([]byte, string, error) {
	if newHash == "" || strings.ContainsAny(newHash, ":\x00\r\n") || len(newHash) > 512 {
		return nil, "", errors.New("invalid PureFTP password verifier")
	}
	start, end, currentHash, fieldsHash, err := pureFTPAccountFields(data, name)
	if err != nil {
		return nil, "", err
	}
	if currentHash != expectedHash {
		return nil, "", errors.New("PureFTP account password verifier changed")
	}
	line := strings.Split(string(data[start:end]), ":")
	line[1] = newHash
	replacement := strings.Join(line, ":")
	updated := make([]byte, 0, len(data)-len(data[start:end])+len(replacement))
	updated = append(updated, data[:start]...)
	updated = append(updated, replacement...)
	updated = append(updated, data[end:]...)
	return updated, fieldsHash, nil
}

func pureFTPStamp(stat *unix.Stat_t) pureFTPPasswdStamp {
	return pureFTPPasswdStamp{
		Device: uint64(stat.Dev), Inode: stat.Ino, Size: stat.Size,
		MtimeSec: stat.Mtim.Sec, MtimeNsec: stat.Mtim.Nsec,
		Mode: stat.Mode, UID: stat.Uid, GID: stat.Gid,
	}
}

func pureFTPDirectoryIdentity(fd int) (uint64, uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return 0, 0, err
	}
	return uint64(stat.Dev), stat.Ino, nil
}

func openPureFTPConfigDir(path string) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return -1, errors.New("PureFTP configuration directory is invalid")
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, path, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		return -1, errors.New("PureFTP configuration directory cannot be opened without following links")
	}
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil || stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0022 != 0 {
		_ = unix.Close(fd)
		return -1, errors.New("PureFTP configuration directory is not protected root-owned storage")
	}
	return fd, nil
}

func readPureFTPPasswd(instance Instance) (pureFTPPasswdSnapshot, error) {
	if os.Geteuid() != 0 {
		return pureFTPPasswdSnapshot{}, failure("privilege_required", "PureFTP account state requires the root Agent", 412)
	}
	dirfd, err := openPureFTPConfigDir(instance.Paths.ConfigDir)
	if err != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_invalid", "PureFTP account storage is not protected", 412)
	}
	defer unix.Close(dirfd)
	return readPureFTPPasswdAt(dirfd)
}

func readPureFTPPasswdAt(dirfd int) (pureFTPPasswdSnapshot, error) {
	fd, err := unix.Openat(dirfd, pureFTPPasswdName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_invalid", "PureFTP account file is unavailable", 412)
	}
	file := os.NewFile(uintptr(fd), pureFTPPasswdName)
	defer file.Close()
	var before unix.Stat_t
	if err = unix.Fstat(fd, &before); err != nil || before.Mode&unix.S_IFMT != unix.S_IFREG || before.Uid != 0 || before.Mode&0077 != 0 || before.Size < 0 || before.Size > pureFTPPasswdMaxBytes {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_invalid", "PureFTP account file permissions or size are invalid", 412)
	}
	data, err := io.ReadAll(io.LimitReader(file, pureFTPPasswdMaxBytes+1))
	if err != nil || len(data) > pureFTPPasswdMaxBytes {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_invalid", "PureFTP account file could not be read safely", 412)
	}
	var after unix.Stat_t
	if unix.Fstat(fd, &after) != nil || pureFTPStamp(&before) != pureFTPStamp(&after) {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account file changed during inspection", 409)
	}
	digest := sha256.Sum256(data)
	return pureFTPPasswdSnapshot{Data: data, SHA256: hex.EncodeToString(digest[:]), Stamp: pureFTPStamp(&before)}, nil
}

func snapshotFTPAccount(instance Instance, name string) (pureFTPPasswdSnapshot, error) {
	snapshot, err := readPureFTPPasswd(instance)
	if err != nil {
		return pureFTPPasswdSnapshot{}, err
	}
	_, _, passhash, fieldsHash, err := pureFTPAccountFields(snapshot.Data, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pureFTPPasswdSnapshot{}, failure("not_found", "PureFTP account does not exist in its protected account file", 404)
		}
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_invalid", "PureFTP account entry has an unsupported format", 412)
	}
	snapshot.PasswordHash = passhash
	snapshot.FieldsSHA256 = fieldsHash
	return snapshot, nil
}

func snapshotFTPAccountAt(dirfd int, name string) (pureFTPPasswdSnapshot, error) {
	snapshot, err := readPureFTPPasswdAt(dirfd)
	if err != nil {
		return pureFTPPasswdSnapshot{}, err
	}
	_, _, passhash, fieldsHash, err := pureFTPAccountFields(snapshot.Data, name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pureFTPPasswdSnapshot{}, failure("not_found", "PureFTP account does not exist in its protected account file", 404)
		}
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_invalid", "PureFTP account entry has an unsupported format", 412)
	}
	snapshot.PasswordHash = passhash
	snapshot.FieldsSHA256 = fieldsHash
	return snapshot, nil
}

func atomicReplacePureFTPHash(instance Instance, expected pureFTPPasswdSnapshot, name, oldHash, newHash string) (pureFTPPasswdSnapshot, error) {
	dirfd, err := openPureFTPConfigDir(instance.Paths.ConfigDir)
	if err != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_invalid", "PureFTP account storage is not protected", 412)
	}
	defer unix.Close(dirfd)
	current, err := snapshotFTPAccountAt(dirfd, name)
	if err != nil {
		return pureFTPPasswdSnapshot{}, err
	}
	if current.Stamp != expected.Stamp || current.SHA256 != expected.SHA256 || current.PasswordHash != oldHash || current.FieldsSHA256 != expected.FieldsSHA256 {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account changed after the pause preview; no update was applied", 409)
	}
	updated, fieldsHash, err := pureFTPReplaceHash(current.Data, name, oldHash, newHash)
	if err != nil || fieldsHash != expected.FieldsSHA256 {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account no longer matches its protected journal", 409)
	}
	// pure-pw writers reserve this deterministic sidecar with O_EXCL before
	// replacing pureftpd.passwd. Use the same protocol so a concurrent
	// pure-pw update cannot be silently overwritten.
	temp := pureFTPPasswdName + ".tmp"
	fd, err := unix.Openat(dirfd, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(current.Stamp.Mode&0777))
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account writer sidecar is busy; it was preserved", 409)
		}
		return pureFTPPasswdSnapshot{}, err
	}
	file := os.NewFile(uintptr(fd), temp)
	tempReserved := true
	defer func() {
		if tempReserved {
			_ = unix.Unlinkat(dirfd, temp, 0)
		}
	}()
	if _, err = file.Write(updated); err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Chown(int(current.Stamp.UID), int(current.Stamp.GID))
	}
	if err == nil {
		err = file.Chmod(os.FileMode(current.Stamp.Mode & 0777))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return pureFTPPasswdSnapshot{}, err
	}
	// Recheck both inode metadata and a content digest immediately before the
	// atomic rename. The account lock serializes Agent writers; this CAS also
	// detects administrators or other processes that edited the source file.
	current, err = snapshotFTPAccountAt(dirfd, name)
	if err != nil || current.Stamp != expected.Stamp || current.SHA256 != expected.SHA256 || current.PasswordHash != oldHash {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account changed before the atomic update; no update was applied", 409)
	}
	pathfd, pathErr := openPureFTPConfigDir(instance.Paths.ConfigDir)
	if pathErr != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP configuration directory changed before the atomic update", 409)
	}
	pathDevice, pathInode, identityErr := pureFTPDirectoryIdentity(pathfd)
	_ = unix.Close(pathfd)
	dirDevice, dirInode, dirErr := pureFTPDirectoryIdentity(dirfd)
	if identityErr != nil || dirErr != nil || pathDevice != dirDevice || pathInode != dirInode {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP configuration directory changed before the atomic update", 409)
	}
	if err = unix.Renameat(dirfd, temp, dirfd, pureFTPPasswdName); err != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_failed", "PureFTP account file could not be atomically replaced", 502)
	}
	tempReserved = false
	if err = unix.Fsync(dirfd); err != nil {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_failed", "PureFTP account file replacement could not be committed", 502)
	}
	result, err := snapshotFTPAccountAt(dirfd, name)
	if err != nil || result.PasswordHash != newHash || result.FieldsSHA256 != expected.FieldsSHA256 {
		return pureFTPPasswdSnapshot{}, failure("ftp_account_state_conflict", "PureFTP account file changed during the atomic update", 409)
	}
	return result, nil
}

func (e *PrivilegedExecutor) generatePureFTPPasswordHash(ctx context.Context, instance Instance) (string, error) {
	tempDir, err := os.MkdirTemp("", ".powerlinux-pureftp-pause-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tempDir)
	if err = os.Chmod(tempDir, 0700); err != nil {
		return "", err
	}
	passwd := filepath.Join(tempDir, pureFTPPasswdName)
	file, err := os.OpenFile(passwd, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", err
	}
	if err = file.Sync(); err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		return "", err
	}
	var nonce [32]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", err
	}
	password := []byte(base64.RawURLEncoding.EncodeToString(nonce[:]))
	clear(nonce[:])
	defer clear(password)
	tempAccount, err := pureFTPTemporaryAccountName()
	if err != nil {
		return "", err
	}
	args := []string{"useradd", tempAccount, "-f", passwd, "-u", strconv.FormatUint(uint64(e.uid), 10), "-g", strconv.FormatUint(uint64(e.gid), 10), "-d", instance.Paths.DataDir}
	operation := agent.Operation{Name: "pureftp_pause_hash", Program: "/usr/bin/pure-pw", Timeout: 15 * time.Second, BuildArgs: func(json.RawMessage) ([]string, error) { return args, nil }}
	runner, err := agent.NewRunner(agent.Options{Operations: []agent.Operation{operation}, MaxConcurrent: 1, MaxOutputBytes: 4096})
	if err != nil {
		return "", err
	}
	stdin := append(append([]byte(nil), password...), '\n')
	stdin = append(stdin, password...)
	stdin = append(stdin, '\n')
	result, runErr := runner.Stream(ctx, agent.Request{Operation: operation.Name, Input: json.RawMessage(`{}`)}, agent.IO{Stdin: io.NopCloser(bytes.NewReader(stdin))})
	clear(stdin)
	if runErr != nil || result.ExitCode != 0 {
		return "", failure("ftp_account_failed", "PureFTP could not create a protected replacement verifier", 502)
	}
	data, err := os.ReadFile(passwd)
	if err != nil || len(data) > pureFTPPasswdMaxBytes {
		return "", failure("ftp_account_failed", "PureFTP replacement verifier could not be inspected", 502)
	}
	_, _, hash, _, err := pureFTPAccountFields(data, tempAccount)
	clear(data)
	if err != nil || hash == "" {
		return "", failure("ftp_account_failed", "PureFTP replacement verifier has an unsupported format", 502)
	}
	return hash, nil
}

func pureFTPTemporaryAccountName() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	name := "pause_" + hex.EncodeToString(random[:])
	if !utilityName.MatchString(name) {
		return "", errors.New("generated PureFTP temporary account name is invalid")
	}
	return name, nil
}

func pureFTPJournalValid(journal pureFTPAccountStateJournal, instance Instance, name string) bool {
	if journal.Version != pureFTPPauseJournalVersion || journal.InstanceID != instance.ID || journal.Name != name || !utilityName.MatchString(name) ||
		journal.OriginalHash == "" || journal.PausedHash == "" || strings.ContainsAny(journal.OriginalHash+journal.PausedHash, ":\x00\r\n") ||
		len(journal.OriginalHash) > 512 || len(journal.PausedHash) > 512 || len(journal.FieldsSHA256) != 64 {
		return false
	}
	switch journal.Phase {
	case "pause_prepared", "pause_replaced", "paused", "resume_prepared", "resume_replaced", "resumed":
	default:
		return false
	}
	return true
}

func (e *PrivilegedExecutor) loadPureFTPStateJournal(instance Instance, name string) (*pureFTPAccountStateJournal, error) {
	var journal pureFTPAccountStateJournal
	err := e.load("private", ftpPauseJournalID(instance.ID, name), &journal)
	if core.ErrorCode(err) == "not_found" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !pureFTPJournalValid(journal, instance, name) {
		return nil, failure("ftp_account_state_invalid", "Protected FTP pause journal is incomplete or invalid", 412)
	}
	return &journal, nil
}

func (e *PrivilegedExecutor) savePureFTPStateJournal(instance Instance, journal pureFTPAccountStateJournal) error {
	if !pureFTPJournalValid(journal, instance, journal.Name) {
		return failure("ftp_account_state_invalid", "Protected FTP pause journal failed validation", 412)
	}
	return e.save("private", ftpPauseJournalID(instance.ID, journal.Name), journal)
}

func (e *PrivilegedExecutor) removePureFTPStateJournal(instance Instance, name string) error {
	dir := filepath.Join(e.config.StateDir, "private")
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	key := ftpPauseJournalID(instance.ID, name) + ".json"
	file, err := root.OpenFile(key, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	file.Close()
	if statErr != nil || info == nil {
		return failure("ftp_account_state_invalid", "Protected FTP pause journal could not be inspected", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return failure("ftp_account_state_invalid", "Protected FTP pause journal permissions are invalid", 412)
	}
	if err = root.Remove(key); err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

func (e *PrivilegedExecutor) rebuildPureFTPPasswordDB(ctx context.Context, instance Instance) error {
	_, err := runFixed(ctx, "/usr/bin/pure-pw", []string{"mkdb", filepath.Join(instance.Paths.ConfigDir, "pureftpd.pdb"), "-f", filepath.Join(instance.Paths.ConfigDir, pureFTPPasswdName)}, 15*time.Second, nil)
	if err != nil {
		return failure("ftp_account_failed", "PureFTP could not rebuild its virtual account database", 502)
	}
	return nil
}

func (e *PrivilegedExecutor) setPureFTPAccountEnabled(ctx context.Context, instance Instance, name string, enabled bool, rebuild bool) error {
	return e.setPureFTPAccountEnabledUsing(ctx, instance, name, enabled, rebuild, e.generatePureFTPPasswordHash, e.rebuildPureFTPPasswordDB)
}

func (e *PrivilegedExecutor) setPureFTPAccountEnabledUsing(ctx context.Context, instance Instance, name string, enabled, rebuild bool, makeHash func(context.Context, Instance) (string, error), buildDB func(context.Context, Instance) error) error {
	if instance.PluginID != "pureftp" || !utilityName.MatchString(name) {
		return failure("runtime_type_mismatch", "FTP account state requires a managed virtual account", 412)
	}
	legacyJournal, err := e.loadFTPLegacyRecoveryJournal(instance, name)
	if err != nil {
		return err
	}
	if legacyJournal != nil {
		return failure("ftp_account_state_pending", "Complete the pending FTP legacy recovery before pausing or resuming this account", 409)
	}
	journal, err := e.loadPureFTPStateJournal(instance, name)
	if err != nil {
		return err
	}
	if enabled {
		if journal == nil {
			return failure("ftp_account_state_unrecoverable", "This FTP account has no protected pause journal to restore", 409)
		}
		return e.finishPureFTPResume(ctx, instance, *journal, buildDB)
	}
	if journal == nil {
		snapshot, err := snapshotFTPAccount(instance, name)
		if err != nil {
			return err
		}
		pausedHash, err := makeHash(ctx, instance)
		if err != nil {
			return err
		}
		journal = &pureFTPAccountStateJournal{
			Version: pureFTPPauseJournalVersion, InstanceID: instance.ID, Name: name, Phase: "pause_prepared",
			OriginalHash: snapshot.PasswordHash, PausedHash: pausedHash, FieldsSHA256: snapshot.FieldsSHA256,
		}
		if err = e.savePureFTPStateJournal(instance, *journal); err != nil {
			return err
		}
	}
	if journal.Phase == "resume_prepared" || journal.Phase == "resume_replaced" || journal.Phase == "resumed" {
		return failure("ftp_account_state_pending", "Complete the pending FTP resume before pausing this account", 409)
	}
	return e.finishPureFTPPause(ctx, instance, *journal, rebuild, buildDB)
}

func (e *PrivilegedExecutor) finishPureFTPPause(ctx context.Context, instance Instance, journal pureFTPAccountStateJournal, rebuild bool, buildDB func(context.Context, Instance) error) error {
	if journal.Phase == "pause_prepared" {
		current, err := snapshotFTPAccount(instance, journal.Name)
		if err != nil {
			return err
		}
		if current.PasswordHash == journal.OriginalHash && current.FieldsSHA256 == journal.FieldsSHA256 {
			current, err = atomicReplacePureFTPHash(instance, current, journal.Name, journal.OriginalHash, journal.PausedHash)
			if err != nil {
				return err
			}
		} else if current.PasswordHash != journal.PausedHash || current.FieldsSHA256 != journal.FieldsSHA256 {
			return failure("ftp_account_state_conflict", "PureFTP account changed during pause; manual review is required", 409)
		}
		journal.Phase = "pause_replaced"
		if err = e.savePureFTPStateJournal(instance, journal); err != nil {
			return err
		}
	}
	if journal.Phase == "pause_replaced" {
		current, err := snapshotFTPAccount(instance, journal.Name)
		if err != nil || current.PasswordHash != journal.PausedHash || current.FieldsSHA256 != journal.FieldsSHA256 {
			return failure("ftp_account_state_conflict", "PureFTP account changed before pause recovery; manual review is required", 409)
		}
		if rebuild {
			if err = buildDB(ctx, instance); err != nil {
				return err
			}
			journal.Phase = "paused"
			if err = e.savePureFTPStateJournal(instance, journal); err != nil {
				return err
			}
		}
	}
	if journal.Phase == "paused" {
		current, err := snapshotFTPAccount(instance, journal.Name)
		if err != nil || current.PasswordHash != journal.PausedHash || current.FieldsSHA256 != journal.FieldsSHA256 {
			return failure("ftp_account_state_conflict", "PureFTP account changed after pause; manual review is required", 409)
		}
	}
	return nil
}

func (e *PrivilegedExecutor) finishPureFTPResume(ctx context.Context, instance Instance, journal pureFTPAccountStateJournal, buildDB func(context.Context, Instance) error) error {
	if journal.Phase == "pause_prepared" || journal.Phase == "pause_replaced" {
		if err := e.finishPureFTPPause(ctx, instance, journal, true, buildDB); err != nil {
			return err
		}
		loaded, loadErr := e.loadPureFTPStateJournal(instance, journal.Name)
		if loadErr != nil || loaded == nil {
			return failure("ftp_account_state_invalid", "PureFTP pause journal disappeared during recovery", 412)
		}
		journal = *loaded
	}
	if journal.Phase == "paused" {
		current, err := snapshotFTPAccount(instance, journal.Name)
		if err != nil || current.PasswordHash != journal.PausedHash || current.FieldsSHA256 != journal.FieldsSHA256 {
			return failure("ftp_account_state_conflict", "PureFTP account changed after pause; its original verifier was not restored", 409)
		}
		journal.Phase = "resume_prepared"
		if err = e.savePureFTPStateJournal(instance, journal); err != nil {
			return err
		}
	}
	if journal.Phase == "resume_prepared" {
		current, err := snapshotFTPAccount(instance, journal.Name)
		if err != nil {
			return err
		}
		if current.PasswordHash == journal.PausedHash && current.FieldsSHA256 == journal.FieldsSHA256 {
			current, err = atomicReplacePureFTPHash(instance, current, journal.Name, journal.PausedHash, journal.OriginalHash)
			if err != nil {
				return err
			}
		} else if current.PasswordHash != journal.OriginalHash || current.FieldsSHA256 != journal.FieldsSHA256 {
			return failure("ftp_account_state_conflict", "PureFTP account changed during resume; manual review is required", 409)
		}
		journal.Phase = "resume_replaced"
		if err = e.savePureFTPStateJournal(instance, journal); err != nil {
			return err
		}
	}
	if journal.Phase == "resume_replaced" {
		current, err := snapshotFTPAccount(instance, journal.Name)
		if err != nil || current.PasswordHash != journal.OriginalHash || current.FieldsSHA256 != journal.FieldsSHA256 {
			return failure("ftp_account_state_conflict", "PureFTP account changed before resume recovery; manual review is required", 409)
		}
		if err = buildDB(ctx, instance); err != nil {
			return err
		}
		journal.Phase = "resumed"
		if err = e.savePureFTPStateJournal(instance, journal); err != nil {
			return err
		}
	}
	if journal.Phase != "resume_replaced" && journal.Phase != "resumed" {
		return failure("ftp_account_state_invalid", "PureFTP resume journal has an unsupported recovery phase", 412)
	}
	current, err := snapshotFTPAccount(instance, journal.Name)
	if err != nil || current.PasswordHash != journal.OriginalHash || current.FieldsSHA256 != journal.FieldsSHA256 {
		return failure("ftp_account_state_conflict", "PureFTP account changed after resume; manual review is required", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) inspectFTPAccountState(instance Instance, record utilityRecord, name string) (FTPAccountStateInspection, error) {
	account, ok := record.Items[name]
	if !ok {
		return FTPAccountStateInspection{}, failure("not_found", "FTP account does not exist", 404)
	}
	view := FTPAccountStateInspection{ID: instance.ID, Name: name, Revision: record.Revision, Phase: "unknown", RecoveryRequired: true}
	journal, err := e.loadPureFTPStateJournal(instance, name)
	if err != nil {
		return FTPAccountStateInspection{}, err
	}
	if journal == nil {
		phase, commands, pinnedRevision, legacyErr := e.inspectLegacyPureFTPAccount(instance, account)
		if legacyErr != nil {
			return FTPAccountStateInspection{}, legacyErr
		}
		if phase != "" {
			if pinnedRevision > 0 {
				view.Revision = pinnedRevision
			}
			view.Phase = phase
			view.AllowedRecoveryCommands = commands
			return view, nil
		}
		enabled := true
		view.LastConfirmedEnabled = &enabled
		view.RecoveryRequired = false
		view.Phase = "active"
		view.AllowedRecoveryCommands = []string{"pause"}
		return view, nil
	}
	switch journal.Phase {
	case "paused":
		current, readErr := snapshotFTPAccount(instance, name)
		if readErr != nil || current.PasswordHash != journal.PausedHash || current.FieldsSHA256 != journal.FieldsSHA256 {
			view.Phase = "conflict"
			return view, nil
		}
		if ftpAccountEnabled(account) {
			view.Phase = "pause_pending"
			view.AllowedRecoveryCommands = []string{"pause", "resume"}
			return view, nil
		}
		enabled := false
		view.LastConfirmedEnabled = &enabled
		view.RecoveryRequired = false
		view.Phase = "paused"
		view.AllowedRecoveryCommands = []string{"resume"}
	case "pause_prepared", "pause_replaced":
		view.Phase = "pause_pending"
		view.AllowedRecoveryCommands = []string{"pause", "resume"}
	case "resume_prepared", "resume_replaced", "resumed":
		view.Phase = "resume_pending"
		view.AllowedRecoveryCommands = []string{"resume"}
	default:
		return FTPAccountStateInspection{}, failure("ftp_account_state_invalid", "Protected FTP pause journal has an unsupported phase", 412)
	}
	return view, nil
}

func (e *PrivilegedExecutor) ftpAccountStateViews(instance Instance, record utilityRecord) ([]FTPAccountStateView, error) {
	names := make([]string, 0, len(record.Items))
	for name := range record.Items {
		names = append(names, name)
	}
	sort.Strings(names)
	items := make([]FTPAccountStateView, 0, len(names))
	for _, name := range names {
		state, err := e.inspectFTPAccountState(instance, record, name)
		if err != nil {
			return nil, err
		}
		account := record.Items[name]
		item := FTPAccountStateView{
			Name: name, ReadOnly: account.ReadOnly, RecoveryRequired: state.RecoveryRequired,
			Phase: state.Phase, AllowedRecoveryCommands: state.AllowedRecoveryCommands,
		}
		if state.LastConfirmedEnabled != nil {
			enabled := *state.LastConfirmedEnabled
			item.Enabled = &enabled
		}
		items = append(items, item)
	}
	return items, nil
}

func (m *Manager) ftpAccountStateBindings() []core.Binding {
	resources := idResource
	bindings := make([]core.Binding, 0, 2)
	for _, action := range []struct {
		name    string
		command string
		enabled bool
	}{
		{name: "ftp_pause", command: "pause", enabled: false},
		{name: "ftp_resume", command: "resume", enabled: true},
	} {
		current := action
		binding := m.binding(current.name, "Queue a durable per-account FTP pause or resume while preserving its original password verifier", "ftp:manage", false, obj(map[string]any{
			"id": idSchema(), "name": map[string]any{"type": "string", "pattern": utilityName.String()}, "expected_revision": revisionSchema(),
		}, "id", "name", "expected_revision"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			var input FTPAccountStateInput
			if err := parse(raw, &input); err != nil {
				return core.ActionResult{}, err
			}
			if !validInstanceID(input.ID) || !utilityName.MatchString(input.Name) || input.ExpectedRevision < 1 {
				return core.ActionResult{}, failure("invalid_input", "A valid FTP account and expected revision are required", 400)
			}
			var instance Instance
			idRaw, _ := json.Marshal(IDInput{ID: input.ID})
			if err := m.agentJSON(ctx, "plugin_get", idRaw, &instance); err != nil {
				return core.ActionResult{}, err
			}
			if instance.PluginID != "pureftp" {
				return core.ActionResult{}, failure("runtime_type_mismatch", "FTP account state requires a managed PureFTP instance", 412)
			}
			jobInput, _ := json.Marshal(UtilityInput{ID: input.ID, Name: input.Name, Command: current.command, ExpectedRevision: input.ExpectedRevision})
			resource := InstanceResource(input.ID)
			job, err := m.deps.Tasks.Submit(ctx, actor, "utility_action", jobInput, []string{resource})
			if err != nil {
				return core.ActionResult{}, err
			}
			taskID, _ := job["id"].(string)
			if taskID == "" {
				taskID, _ = job["task_id"].(string)
			}
			return core.ActionResult{Data: job, TaskID: taskID, Resources: []string{resource}, AuditSummary: map[string]any{"instance_id": input.ID, "account": input.Name, "command": current.command, "enabled": current.enabled}}, nil
		})
		binding.Action.Task = true
		binding.Action.Audit = true
		bindings = append(bindings, binding)
	}
	inspect := m.binding("ftp_account_state_inspect", "Inspect one PureFTP account's last confirmed pause state and safe recovery choices", "ftp:read", true, obj(map[string]any{
		"id": idSchema(), "name": map[string]any{"type": "string", "pattern": utilityName.String()},
	}, "id", "name"), resources, func(ctx context.Context, _ core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		if !validInstanceID(input.ID) || !utilityName.MatchString(input.Name) {
			return core.ActionResult{}, failure("invalid_input", "A valid FTP account is required", 400)
		}
		var instance Instance
		idRaw, _ := json.Marshal(IDInput{ID: input.ID})
		if err := m.agentJSON(ctx, "plugin_get", idRaw, &instance); err != nil {
			return core.ActionResult{}, err
		}
		if instance.PluginID != "pureftp" {
			return core.ActionResult{}, failure("runtime_type_mismatch", "FTP account inspection requires a managed PureFTP instance", 412)
		}
		forward, _ := json.Marshal(UtilityInput{ID: input.ID, Name: input.Name, Command: "inspect"})
		var result FTPAccountStateInspection
		if err := m.agentJSON(ctx, "utility_action", forward, &result); err != nil {
			return core.ActionResult{}, err
		}
		resource := InstanceResource(input.ID)
		return core.ActionResult{Data: result, Resources: []string{resource}}, nil
	})
	bindings = append(bindings, inspect)
	return bindings
}
