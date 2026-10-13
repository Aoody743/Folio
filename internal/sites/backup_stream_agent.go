package sites

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
)

const maxWebsiteBackupBytes int64 = 3 << 30 // gzip overhead above the 2-GiB input bound
const maxWebsiteBackupChunk int64 = 32 << 10

type backupFileStamp struct {
	Device, Inode uint64
	UID, GID      uint32
	Mode          uint32
	Size          int64
	Modified      syscall.Timespec
	Changed       syscall.Timespec
}
type verifiedBackupFile struct {
	Stamp  backupFileStamp
	SHA256 string
}
type backupStreamState struct {
	mu       sync.Mutex
	slots    chan struct{}
	verified map[string]verifiedBackupFile
}

func (s *backupStreamState) acquire(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.slots == nil {
		s.slots = make(chan struct{}, 8)
	}
	slots := s.slots
	s.mu.Unlock()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, nil
	default:
		return nil, failure("rate_limited", "Website backup stream slots are busy", 429)
	}
}

func backupStamp(file *os.File) (backupFileStamp, error) {
	info, err := file.Stat()
	if err != nil {
		return backupFileStamp{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Uid != 0 || info.Mode().Perm() != 0600 {
		return backupFileStamp{}, failure("artifact_invalid", "Website archive must be a private root-owned regular file", 412)
	}
	return backupFileStamp{Device: uint64(stat.Dev), Inode: stat.Ino, UID: stat.Uid, GID: stat.Gid,
		Mode: uint32(info.Mode().Perm()), Size: info.Size(), Modified: stat.Mtim, Changed: stat.Ctim}, nil
}

func validateBackupArtifact(in request) error {
	if !safeID.MatchString(in.ID) || len(in.ID) > 75 || !safeID.MatchString(in.BackupID) || len(in.BackupID) > 96 ||
		in.ArtifactOffset < 0 || in.ArtifactOffset > maxWebsiteBackupBytes || in.ArtifactLength < 0 || in.ArtifactLength > maxWebsiteBackupBytes ||
		in.ArtifactOffset != 0 && in.ArtifactLength == 0 {
		return invalid("Invalid bounded website archive descriptor")
	}
	return nil
}

// The archive name comes exclusively from the server-issued receipt. The
// private root manifest/settings and TLS keys are separate and never streamed.
func (e *AgentExecutor) openVerifiedBackup(ctx context.Context, in request) (*os.File, Backup, backupFileStamp, error) {
	if err := validateBackupArtifact(in); err != nil {
		return nil, Backup{}, backupFileStamp{}, err
	}
	metadata, err := e.backupMetadata(in)
	if err != nil {
		return nil, Backup{}, backupFileStamp{}, err
	}
	backup := metadata.Backup
	checksum, err := hex.DecodeString(backup.SHA256)
	if backup.ID != in.BackupID || backup.SiteID != in.ID || backup.Size < 1 || backup.Size > maxWebsiteBackupBytes ||
		backup.IncludesTLS || err != nil || len(checksum) != sha256.Size {
		return nil, backup, backupFileStamp{}, failure("artifact_invalid", "Website archive receipt is invalid or includes unreviewed private material", 412)
	}
	name := filepath.Join("backups", in.ID, backup.ID+".tar.gz")
	file, err := e.state.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, backup, backupFileStamp{}, failure("artifact_invalid", "Owned website archive cannot be safely opened", 412)
	}
	fail := func(err error) (*os.File, Backup, backupFileStamp, error) {
		file.Close()
		return nil, backup, backupFileStamp{}, err
	}
	before, err := backupStamp(file)
	if err != nil {
		return fail(err)
	}
	if before.Size != backup.Size {
		return fail(failure("artifact_invalid", "Website archive size differs from its owned receipt", 412))
	}
	e.backupStreams.mu.Lock()
	cached, exists := e.backupStreams.verified[name]
	e.backupStreams.mu.Unlock()
	if !exists || cached.Stamp != before || cached.SHA256 != backup.SHA256 {
		stop := context.AfterFunc(ctx, func() { file.Close() })
		hash := sha256.New()
		count, hashErr := io.CopyBuffer(hash, cancelRead{ctx, file}, make([]byte, 32<<10))
		stop()
		if hashErr != nil {
			return fail(hashErr)
		}
		after, statErr := backupStamp(file)
		if statErr != nil || before != after || count != backup.Size || hex.EncodeToString(hash.Sum(nil)) != backup.SHA256 {
			return fail(failure("checksum_mismatch", "Website archive integrity verification failed; no bytes were exported", 412))
		}
		e.backupStreams.mu.Lock()
		if e.backupStreams.verified == nil {
			e.backupStreams.verified = map[string]verifiedBackupFile{}
		}
		if len(e.backupStreams.verified) >= 128 {
			for key := range e.backupStreams.verified {
				delete(e.backupStreams.verified, key)
				break
			}
		}
		e.backupStreams.verified[name] = verifiedBackupFile{Stamp: before, SHA256: backup.SHA256}
		e.backupStreams.mu.Unlock()
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return file, backup, before, nil
}

func (e *AgentExecutor) runBackupStat(ctx context.Context, in request) (any, error) {
	release, err := e.backupStreams.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	file, backup, _, err := e.openVerifiedBackup(ctx, in)
	if err != nil {
		return nil, err
	}
	file.Close()
	return backup, nil
}

func (e *AgentExecutor) Stream(ctx context.Context, job agent.Request, streams agent.IO) (result agent.Result, err error) {
	result = agent.Result{Operation: job.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		if err == nil {
			result.ExitCode = 0
		}
	}()
	if job.Operation != "site_backup_artifact" {
		return result, failure("stream_unavailable", "This website operation does not export a binary stream", 400)
	}
	if err := e.Validate(job); err != nil {
		return result, err
	}
	if streams.Stdout == nil {
		return result, failure("stream_required", "Website archive export requires an output stream", 400)
	}
	var in request
	if err := strict(job.Input, &in); err != nil {
		return result, err
	}
	release, err := e.backupStreams.acquire(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	file, backup, stamp, err := e.openVerifiedBackup(ctx, in)
	if err != nil {
		return result, err
	}
	defer file.Close()
	if in.ArtifactOffset > backup.Size {
		return result, invalid("Chunk offset exceeds the owned archive")
	}
	if _, err := file.Seek(in.ArtifactOffset, io.SeekStart); err != nil {
		return result, err
	}
	stop := context.AfterFunc(ctx, func() { file.Close(); streams.Stdout.Close() })
	defer stop()
	expected := backup.Size - in.ArtifactOffset
	if in.ArtifactLength > 0 {
		expected = min(expected, in.ArtifactLength)
	}
	count, err := io.CopyBuffer(streams.Stdout, cancelRead{ctx, io.LimitReader(file, expected)}, make([]byte, 32<<10))
	result.StdoutBytes = count
	if err != nil {
		return result, err
	}
	after, err := backupStamp(file)
	if err != nil || stamp != after || count != expected {
		return result, failure("artifact_transfer_failed", "Website archive changed or its export was incomplete", 502)
	}
	result.Data = encode(map[string]any{"backup_id": backup.ID, "site_id": backup.SiteID,
		"bytes": count, "offset": in.ArtifactOffset, "sha256": backup.SHA256})
	return result, nil
}
