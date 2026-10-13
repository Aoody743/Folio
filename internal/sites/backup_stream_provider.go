package sites

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/url"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type backupDownloadInput struct {
	ID       string `json:"id"`
	BackupID string `json:"backup_id"`
	Offset   int64  `json:"offset,omitempty"`
	Length   int64  `json:"length,omitempty"`
}
type backupDownloadKey struct{}
type backupPreparedWriter interface {
	io.WriteCloser
	Prepare(Backup) (int64, int64, error)
}

func backupDownloadSchema(chunk bool) map[string]any {
	fields := map[string]any{"id": idField(), "backup_id": idField()}
	if chunk {
		fields["offset"] = number(0, int(maxWebsiteBackupBytes))
		fields["length"] = number(1, int(maxWebsiteBackupChunk))
	}
	return object(fields, "id", "backup_id")
}

func (p *Provider) backupDownloadResources(_ context.Context, actor core.Actor, raw json.RawMessage) ([]string, error) {
	var in backupDownloadInput
	if err := strict(raw, &in); err != nil {
		return nil, err
	}
	if !safeID.MatchString(in.ID) || len(in.ID) > 75 || !safeID.MatchString(in.BackupID) || in.Offset < 0 || in.Offset > maxWebsiteBackupBytes || in.Length < 0 || in.Length > maxWebsiteBackupChunk {
		return nil, invalid("Invalid website archive descriptor")
	}
	if !p.deps.Authorize(actor, "sites:backup", Resource(in.ID)) || !p.deps.Authorize(actor, "files:read", "fs:site-"+in.ID) {
		return nil, denied()
	}
	return []string{Resource(in.ID), "fs:site-" + in.ID}, nil
}

func (p *Provider) BackupDownloadBindings() []core.Binding {
	bindings := []core.Binding{}
	for _, chunk := range []bool{false, true} {
		name, description := "sites_backup_download", "Discover an authenticated download for a checksum-verified owned website content archive"
		if chunk {
			name, description = "sites_backup_read_chunk", "Read at most 32 KiB from an owned website archive; reauthorize each native MCP chunk"
		}
		chunk := chunk
		bindings = append(bindings, core.Binding{
			Action:    core.Action{Name: name, Description: description, Scope: "sites:backup", ReadOnly: true, Audit: true, InputSchema: backupDownloadSchema(chunk)},
			Resources: p.backupDownloadResources,
			Execute: func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
				return p.executeBackupDownload(ctx, actor, raw, chunk)
			},
		})
	}
	return bindings
}

func (p *Provider) backupLease(ctx context.Context, actor core.Actor, id string) (context.Context, context.CancelFunc, error) {
	check := func(ctx context.Context) error {
		current, err := p.deps.ResolveActor(ctx, actor)
		if err != nil {
			return err
		}
		if !p.deps.Authorize(current, "sites:backup", Resource(id)) || !p.deps.Authorize(current, "files:read", "fs:site-"+id) {
			return denied()
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return nil, nil, err
	}
	lease, cancel := context.WithCancelCause(ctx)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-lease.Done():
				return
			case <-ticker.C:
				if err := check(lease); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return lease, func() { cancel(context.Canceled) }, nil
}

func (p *Provider) executeBackupDownload(ctx context.Context, actor core.Actor, raw json.RawMessage, chunk bool) (result core.ActionResult, err error) {
	resources, err := p.backupDownloadResources(ctx, actor, raw)
	if err != nil {
		return result, err
	}
	var in backupDownloadInput
	if err := strict(raw, &in); err != nil {
		return result, err
	}
	lease, cancel, err := p.backupLease(ctx, actor, in.ID)
	if err != nil {
		return result, err
	}
	defer cancel()
	defer func() {
		if err != nil && context.Cause(lease) != nil {
			err = context.Cause(lease)
		}
	}()
	var backup Backup
	if err = p.call(lease, actor, "site_backup_stat", request{ID: in.ID, BackupID: in.BackupID}, &backup); err != nil {
		return result, err
	}
	if backup.ID != in.BackupID || backup.SiteID != in.ID || backup.IncludesTLS || backup.Size < 1 || backup.Size > maxWebsiteBackupBytes {
		return result, failure("artifact_invalid", "Verified website archive does not match its resource", 412)
	}
	result = core.ActionResult{Resources: resources, AuditSummary: map[string]any{"backup_id": backup.ID, "bytes": backup.Size, "sha256": backup.SHA256}}
	streamInput := request{Actor: actor.Public(), ID: in.ID, BackupID: in.BackupID}
	if chunk {
		if in.Length == 0 {
			in.Length = 16 << 10
		}
		if in.Offset > backup.Size {
			return result, invalid("Chunk offset exceeds the owned archive")
		}
		streamInput.ArtifactOffset, streamInput.ArtifactLength = in.Offset, in.Length
		buffer := &boundedBackupBuffer{}
		if err = p.streamBackup(lease, actor, streamInput, buffer); err != nil {
			return result, err
		}
		result.Data = map[string]any{"backup": backup, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(buffer.Bytes()),
			"offset": in.Offset, "next_offset": in.Offset + int64(buffer.Len()), "eof": in.Offset+int64(buffer.Len()) >= backup.Size}
		return result, nil
	}
	query := url.Values{"id": {in.ID}, "backup_id": {in.BackupID}}
	result.Data = map[string]any{"backup": backup, "download_url": "/api/v1/sites/backups/download?" + query.Encode()}
	if writer, ok := ctx.Value(backupDownloadKey{}).(backupPreparedWriter); ok {
		streamInput.ArtifactOffset, streamInput.ArtifactLength, err = writer.Prepare(backup)
		if err != nil {
			return result, err
		}
		if err = p.streamBackup(lease, actor, streamInput, writer); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (p *Provider) streamBackup(ctx context.Context, actor core.Actor, in request, writer io.WriteCloser) error {
	streamer, ok := p.options.Agent.(agent.StreamExecutor)
	if !ok {
		return failure("stream_unavailable", "Website archive export requires controlled Agent streaming", 503)
	}
	result, err := streamer.Stream(ctx, agent.Request{ActorID: actor.ID, Operation: "site_backup_artifact", Input: encode(in)}, agent.IO{Stdout: writer})
	if err != nil {
		return providerError(err)
	}
	if result.ExitCode != 0 || result.Stdout != "" || result.Stderr != "" {
		return failure("artifact_transfer_failed", "Website archive stream did not complete safely", 502)
	}
	return nil
}

type boundedBackupBuffer struct{ bytes.Buffer }

func (b *boundedBackupBuffer) Write(data []byte) (int, error) {
	if int64(b.Len()+len(data)) > maxWebsiteBackupChunk {
		return 0, failure("result_limit", "Website archive chunk exceeded its 32-KiB bound", 413)
	}
	return b.Buffer.Write(data)
}
func (*boundedBackupBuffer) Close() error { return nil }
