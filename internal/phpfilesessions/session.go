// Package phpfilesessions provides bounded, race-resistant primitives for the
// managed PHP files session handler. It deliberately knows nothing about
// website, plugin, actor, or runtime ownership receipts; callers supply those
// checks and keep preview state in their own protected Agent roots.
package phpfilesessions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	ErrUnsafe    = errors.New("unsafe PHP session entry")
	ErrScanLimit = errors.New("PHP session directory scan limit exceeded")
	ErrStale     = errors.New("PHP session identity changed")
	ErrBusy      = errors.New("PHP session is locked by a request")
	ErrRemove    = errors.New("PHP session could not be removed")
)

var sessionFilename = regexp.MustCompile(`^sess_[A-Za-z0-9,-]{1,256}$`)

// FileIdentity is an immutable compare-and-swap record for one session file.
type FileIdentity struct {
	Name      string `json:"name"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	Size      int64  `json:"size"`
	MTimeSec  int64  `json:"mtime_sec"`
	MTimeNsec int64  `json:"mtime_nsec"`
	CTimeSec  int64  `json:"ctime_sec"`
	CTimeNsec int64  `json:"ctime_nsec"`
	UID       uint32 `json:"uid"`
	GID       uint32 `json:"gid"`
	Mode      uint32 `json:"mode"`
	Links     uint64 `json:"links"`
}

type DirectoryIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    uint32 `json:"uid"`
	GID    uint32 `json:"gid"`
	Mode   uint32 `json:"mode"`
}

type ScanOptions struct {
	UID                   uint32
	GID                   uint32
	MaxEntries            int
	IgnoreNonSessionFiles bool
}

type LockedFile struct {
	File     *os.File
	Identity FileIdentity
}

func IdentifyDirectory(info os.FileInfo, uid, gid uint32, private bool) (DirectoryIdentity, error) {
	var identity DirectoryIdentity
	if info == nil || !info.IsDir() {
		return identity, fmt.Errorf("%w: session storage is not a directory", ErrUnsafe)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid || info.Mode().Perm()&0022 != 0 || (private && info.Mode().Perm()&0077 != 0) {
		return identity, fmt.Errorf("%w: session directory ownership or mode is unsafe", ErrUnsafe)
	}
	return DirectoryIdentity{Device: uint64(stat.Dev), Inode: stat.Ino, UID: stat.Uid, GID: stat.Gid, Mode: uint32(info.Mode().Perm())}, nil
}

func Identify(name string, info os.FileInfo, uid, gid uint32) (FileIdentity, error) {
	var identity FileIdentity
	if !sessionFilename.MatchString(name) || info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return identity, fmt.Errorf("%w: expected a private regular sess_ file", ErrUnsafe)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || stat.Gid != gid || stat.Nlink != 1 || stat.Size < 0 {
		return identity, fmt.Errorf("%w: session file owner or link count is unsafe", ErrUnsafe)
	}
	return FileIdentity{Name: name, Device: uint64(stat.Dev), Inode: stat.Ino, Size: stat.Size, MTimeSec: stat.Mtim.Sec, MTimeNsec: stat.Mtim.Nsec, CTimeSec: stat.Ctim.Sec, CTimeNsec: stat.Ctim.Nsec, UID: stat.Uid, GID: stat.Gid, Mode: uint32(info.Mode().Perm()), Links: uint64(stat.Nlink)}, nil
}

// Scan bounds all directory entries. Strict mode is used for a dedicated
// session directory; shared PHP tmp directories ignore unrelated artifacts.
// Any entry beginning with sess_ is treated as a session candidate and fails
// closed if its name or file identity is unsafe.
func Scan(ctx context.Context, root *os.Root, options ScanOptions) ([]FileIdentity, error) {
	if root == nil || options.MaxEntries < 1 {
		return nil, fmt.Errorf("%w: invalid scan root or bound", ErrUnsafe)
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("%w: session directory could not be scanned", ErrUnsafe)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(options.MaxEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: session directory could not be scanned", ErrUnsafe)
	}
	if len(entries) > options.MaxEntries {
		return nil, ErrScanLimit
	}
	files := make([]FileIdentity, 0, len(entries))
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "sess_") {
			if options.IgnoreNonSessionFiles {
				continue
			}
			return nil, fmt.Errorf("%w: unexpected entry in dedicated session directory", ErrUnsafe)
		}
		info, statErr := root.Lstat(name)
		if statErr != nil {
			return nil, fmt.Errorf("%w: session entry changed during scan", ErrUnsafe)
		}
		identity, identityErr := Identify(name, info, options.UID, options.GID)
		if identityErr != nil {
			return nil, identityErr
		}
		files = append(files, identity)
	}
	slices.SortFunc(files, func(a, b FileIdentity) int { return strings.Compare(a.Name, b.Name) })
	return files, nil
}

func Lock(root *os.Root, expected FileIdentity, uid, gid uint32) (*LockedFile, error) {
	if root == nil {
		return nil, fmt.Errorf("%w: session root is unavailable", ErrStale)
	}
	// A captured session can be replaced by a FIFO before open. O_NONBLOCK
	// prevents an attacker-controlled special file from hanging the Agent.
	file, err := root.OpenFile(expected.Name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	identity, err := Identify(expected.Name, info, uid, gid)
	if err != nil || identity != expected {
		_ = file.Close()
		return nil, ErrStale
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrBusy
		}
		return nil, err
	}
	pathInfo, pathErr := root.Lstat(expected.Name)
	openedInfo, openedErr := file.Stat()
	if pathErr != nil || openedErr != nil || pathInfo == nil || !os.SameFile(openedInfo, pathInfo) {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, ErrStale
	}
	current, currentErr := Identify(expected.Name, pathInfo, uid, gid)
	if currentErr != nil || current != expected {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		return nil, ErrStale
	}
	return &LockedFile{File: file, Identity: expected}, nil
}

func (item *LockedFile) Close() {
	if item == nil || item.File == nil {
		return
	}
	_ = syscall.Flock(int(item.File.Fd()), syscall.LOCK_UN)
	_ = item.File.Close()
	item.File = nil
}

func SameExceptCTime(a, b FileIdentity) bool {
	a.CTimeSec, a.CTimeNsec = 0, 0
	b.CTimeSec, b.CTimeNsec = 0, 0
	return a == b
}

// Remove renames only the already locked and captured file into a private,
// unique quarantine name, verifies the same inode there, and then unlinks it.
// The caller must pass an Agent-generated basename and retain the directory
// lock while this runs.
func Remove(root, quarantineRoot *os.Root, item *LockedFile, quarantine string) error {
	return remove(root, quarantineRoot, item, quarantine, nil)
}

// remove accepts a package-private hook so tests can simulate a name
// substitution between the post-rename identity check and unlink. Production
// callers always use Remove without a hook.
func remove(root, quarantineRoot *os.Root, item *LockedFile, quarantine string, afterRename func(*os.Root, string) error) error {
	if root == nil || quarantineRoot == nil || item == nil || item.File == nil || !safeQuarantineName(quarantine) {
		return fmt.Errorf("%w: invalid quarantine target", ErrStale)
	}
	sourceDir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer sourceDir.Close()
	quarantineDir, err := quarantineRoot.Open(".")
	if err != nil {
		return err
	}
	defer quarantineDir.Close()
	var sourceStat, quarantineStat unix.Stat_t
	if err = unix.Fstat(int(sourceDir.Fd()), &sourceStat); err != nil {
		return err
	}
	if err = unix.Fstat(int(quarantineDir.Fd()), &quarantineStat); err != nil {
		return err
	}
	if sourceStat.Dev != quarantineStat.Dev {
		return fmt.Errorf("%w: session quarantine is on another filesystem", ErrUnsafe)
	}
	quarantineInfo, err := quarantineRoot.Stat(".")
	if err != nil || !safePrivateDirectory(quarantineInfo) {
		return fmt.Errorf("%w: session quarantine is not a private Agent directory", ErrUnsafe)
	}
	sourceInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	if _, err = IdentifyDirectory(sourceInfo, item.Identity.UID, item.Identity.GID, true); err != nil {
		return err
	}
	openedInfo, err := item.File.Stat()
	if err != nil {
		return err
	}
	openedIdentity, err := Identify(item.Identity.Name, openedInfo, item.Identity.UID, item.Identity.GID)
	if err != nil || openedIdentity != item.Identity {
		return ErrStale
	}
	pathInfo, err := root.Lstat(item.Identity.Name)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, pathInfo) {
		return ErrStale
	}
	if err = unix.Renameat2(int(sourceDir.Fd()), item.Identity.Name, int(quarantineDir.Fd()), quarantine, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("%w: session could not be isolated", ErrStale)
	}
	if afterRename != nil {
		if err = afterRename(quarantineRoot, quarantine); err != nil {
			return err
		}
	}
	quarantined, statErr := quarantineRoot.Lstat(quarantine)
	if statErr != nil || quarantined.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, quarantined) {
		return ErrStale
	}
	identity, identifyErr := Identify(item.Identity.Name, quarantined, item.Identity.UID, item.Identity.GID)
	if identifyErr != nil || !SameExceptCTime(identity, item.Identity) {
		return ErrStale
	}
	if err = unix.Unlinkat(int(quarantineDir.Fd()), quarantine, 0); err != nil {
		return fmt.Errorf("%w: session could not be unlinked", ErrRemove)
	}
	return nil
}

func safeQuarantineName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func safePrivateDirectory(info os.FileInfo) bool {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

// EnsureQuarantine opens a root-owned, private child directory through an
// already verified parent. Keeping it on the session filesystem lets callers
// use descriptor-relative rename without falling back to a worker-writable
// quarantine name. The returned Root pins the child even if the worker can
// rename entries in the parent directory.
func EnsureQuarantine(parent *os.Root, name string) (*os.Root, error) {
	if parent == nil || !safeQuarantineName(name) {
		return nil, fmt.Errorf("%w: invalid quarantine directory", ErrUnsafe)
	}
	err := parent.Mkdir(name, 0700)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	pathInfo, err := parent.Lstat(name)
	if err != nil || !safePrivateDirectory(pathInfo) {
		return nil, fmt.Errorf("%w: quarantine directory identity is unsafe", ErrUnsafe)
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	openedInfo, err := root.Stat(".")
	if err != nil || !safePrivateDirectory(openedInfo) || !os.SameFile(pathInfo, openedInfo) {
		_ = root.Close()
		return nil, fmt.Errorf("%w: quarantine directory changed while opening", ErrUnsafe)
	}
	return root, nil
}
