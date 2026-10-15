package plugins

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

const (
	dockerArchiveMaxBytes   = int64(4 << 30)
	dockerArchiveChunk      = 32 << 10
	dockerArchiveMaxAge     = 24 * time.Hour
	dockerArchiveMaxAliases = 4096
)

var (
	dockerArchiveID      = regexp.MustCompile("^dockerarchive_[a-f0-9]{32}$")
	dockerArchiveAliasID = regexp.MustCompile("^dockerarchivealias_[a-f0-9]{32}$")
	dockerArchiveIndexID = regexp.MustCompile("^dockerarchiveindex_[a-f0-9]{32}$")
	dockerArchiveLayerID = regexp.MustCompile("^[a-f0-9]{64}$")
	dockerArchiveTag     = regexp.MustCompile("^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$")
)

// DockerArchiveAction carries only typed operations and managed identifiers.
// Actor identity is supplied by the authenticated Agent request envelope.
type DockerArchiveAction struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	Command   string `json:"command"`
	ArchiveID string `json:"archive_id,omitempty"`
	Proof     string `json:"proof,omitempty"`
}

type dockerArchiveArtifactInput struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	ArchiveID string `json:"archive_id"`
	Direction string `json:"direction"`
	Offset    int64  `json:"offset"`
	Length    int64  `json:"length"`
	SHA256    string `json:"sha256,omitempty"`
}

type dockerImageArchive struct {
	ID               string `json:"id"`
	InstanceID       string `json:"instance_id"`
	SourceInstanceID string `json:"source_instance_id"`
	Image            string `json:"image"`
	ImageID          string `json:"image_id"`
	Bytes            int64  `json:"bytes"`
	SHA256           string `json:"sha256"`
	CreatedAt        string `json:"created_at"`
	State            string `json:"state"`
	ActorID          string `json:"actor_id,omitempty"`
	UploadOffset     int64  `json:"upload_offset,omitempty"`
	LastChunkOffset  int64  `json:"last_chunk_offset,omitempty"`
	LastChunkBytes   int64  `json:"last_chunk_bytes,omitempty"`
	LastChunkSHA256  string `json:"last_chunk_sha256,omitempty"`
	Provenance       string `json:"provenance,omitempty"`
	Proof            string `json:"proof,omitempty"`
}

type dockerArchiveProof struct {
	Version          int    `json:"version"`
	SourceInstanceID string `json:"source_instance_id"`
	Image            string `json:"image"`
	ImageID          string `json:"image_id"`
	Bytes            int64  `json:"bytes"`
	SHA256           string `json:"sha256"`
	CreatedAt        string `json:"created_at"`
}

type dockerArchiveImageAlias struct {
	Version    int    `json:"version"`
	InstanceID string `json:"instance_id"`
	Image      string `json:"image"`
	ImageID    string `json:"image_id"`
	ArchiveID  string `json:"archive_id"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	Proof      string `json:"proof"`
	CreatedAt  string `json:"created_at"`
}

type dockerArchiveImageAliasIndex struct {
	InstanceID string   `json:"instance_id"`
	Images     []string `json:"images"`
}

type dockerArchiveView struct {
	ArchiveID string `json:"archive_id"`
	Image     string `json:"image"`
	ImageID   string `json:"image_id"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	CreatedAt string `json:"created_at"`
	State     string `json:"state,omitempty"`
	Proof     string `json:"proof,omitempty"`
}

func (e *PrivilegedExecutor) validateDockerArchiveAction(raw json.RawMessage) error {
	var in DockerArchiveAction
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.ID) || !dockerImage.MatchString(in.Image) {
		return failure("invalid_input", "A managed Docker instance and immutable image digest are required", 400)
	}
	switch in.Command {
	case "export":
		if in.ArchiveID != "" || in.Proof != "" {
			return failure("invalid_input", "Export does not accept an archive identifier or proof", 400)
		}
	case "upload_begin":
		if in.ArchiveID != "" || len(in.Proof) < 32 || len(in.Proof) > 4096 {
			return failure("invalid_input", "A bounded proof from a managed image export is required", 400)
		}
	case "upload_status", "import":
		if !dockerArchiveID.MatchString(in.ArchiveID) || in.Proof != "" {
			return failure("invalid_input", "A managed archive identifier is required", 400)
		}
	default:
		return failure("invalid_input", "Unknown typed Docker archive action", 400)
	}
	return nil
}

func validateDockerArchiveArtifact(raw json.RawMessage) error {
	var in dockerArchiveArtifactInput
	if err := parse(raw, &in); err != nil {
		return err
	}
	if !validInstanceID(in.ID) || !dockerImage.MatchString(in.Image) || !dockerArchiveID.MatchString(in.ArchiveID) || in.Offset < 0 || in.Offset > dockerArchiveMaxBytes || in.Length < 1 || in.Length > dockerArchiveChunk {
		return failure("invalid_input", "Invalid bounded Docker image archive chunk descriptor", 400)
	}
	if in.Direction == "upload" {
		if !artifactHash.MatchString(in.SHA256) || in.Offset+in.Length > dockerArchiveMaxBytes {
			return failure("invalid_input", "Upload chunks require a SHA-256 and remain within the archive limit", 400)
		}
	} else if in.Direction == "download" {
		if in.SHA256 != "" {
			return failure("invalid_input", "Download chunks do not accept a caller checksum", 400)
		}
	} else {
		return failure("invalid_input", "Archive direction must be upload or download", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) dockerArchiveAction(ctx context.Context, request agent.Request) (any, error) {
	if err := e.validateDockerArchiveAction(request.Input); err != nil {
		return nil, err
	}
	var in DockerArchiveAction
	if err := parse(request.Input, &in); err != nil {
		return nil, err
	}
	if (in.Command == "upload_begin" || in.Command == "upload_status" || in.Command == "import") && request.ActorID == "" {
		return nil, failure("unauthorized", "A trusted actor identity is required for archive transfer", 401)
	}
	unlock, err := e.lockContext(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	instance, err := e.dockerArchiveInstance(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	switch in.Command {
	case "export":
		return e.exportDockerImageArchive(ctx, instance, in.Image)
	case "upload_begin":
		return e.beginDockerImageArchiveUpload(ctx, instance, in.Image, in.Proof, request.ActorID)
	case "upload_status":
		return e.dockerImageArchiveStatus(instance, in.Image, in.ArchiveID, request.ActorID)
	case "import":
		if err = e.ensureInstallFinalized(ctx, in.ID); err != nil {
			return nil, err
		}
		return e.importDockerImageArchive(ctx, instance, in.Image, in.ArchiveID, request.ActorID)
	default:
		return nil, failure("invalid_input", "Unknown typed Docker archive action", 400)
	}
}

func (e *PrivilegedExecutor) dockerArchiveInstance(ctx context.Context, id string) (Instance, error) {
	instance, err := e.get(ctx, id, false)
	if err != nil {
		return Instance{}, err
	}
	if instance.PluginID != "docker" {
		return Instance{}, failure("runtime_type_mismatch", "A managed Docker daemon is required", 412)
	}
	if instance.Status == "uninstalled" {
		return Instance{}, failure("runtime_unavailable", "Docker instance is uninstalled", 412)
	}
	return instance, nil
}

func dockerArchiveFilename(id string) string { return id + ".tar" }

func (e *PrivilegedExecutor) dockerArchiveRoot(instance Instance) (*os.Root, error) {
	if instance.Paths != e.instancePaths(instance.PluginID, instance.InstanceID) {
		return nil, failure("ownership_invalid", "Docker archive path does not match the managed instance layout", 412)
	}
	if err := os.MkdirAll(instance.Paths.BackupDir, 0700); err != nil {
		return nil, err
	}
	if err := realDirectory(instance.Paths.BackupDir); err != nil {
		return nil, failure("ownership_invalid", "Managed Docker archive directory is unsafe", 412)
	}
	return os.OpenRoot(instance.Paths.BackupDir)
}

func (e *PrivilegedExecutor) sealDockerArchiveProof(proof dockerArchiveProof) (string, error) {
	if e.privateCipher == nil {
		return "", failure("runtime_unavailable", "Protected archive proof service is unavailable", 503)
	}
	raw, err := json.Marshal(proof)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, e.privateCipher.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := e.privateCipher.Seal(nonce, nonce, raw, []byte("docker-image-archive-proof-v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (e *PrivilegedExecutor) openDockerArchiveProof(token string) (dockerArchiveProof, error) {
	var proof dockerArchiveProof
	if e.privateCipher == nil || len(token) < 32 || len(token) > 4096 {
		return proof, failure("archive_proof_invalid", "Managed archive proof is invalid", 412)
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(sealed) < e.privateCipher.NonceSize()+e.privateCipher.Overhead() {
		return proof, failure("archive_proof_invalid", "Managed archive proof is invalid", 412)
	}
	nonce := sealed[:e.privateCipher.NonceSize()]
	raw, err := e.privateCipher.Open(nil, nonce, sealed[e.privateCipher.NonceSize():], []byte("docker-image-archive-proof-v1"))
	if err != nil {
		return proof, failure("archive_proof_invalid", "Managed archive proof is invalid", 412)
	}
	defer clear(raw)
	if err = parse(raw, &proof); err != nil || proof.Version != 1 || !validInstanceID(proof.SourceInstanceID) || !dockerImage.MatchString(proof.Image) || !dockerImageID.MatchString(proof.ImageID) || proof.Bytes < 1 || proof.Bytes > dockerArchiveMaxBytes || !artifactHash.MatchString(proof.SHA256) {
		return dockerArchiveProof{}, failure("archive_proof_invalid", "Managed archive proof is invalid", 412)
	}
	if _, err = time.Parse(time.RFC3339Nano, proof.CreatedAt); err != nil {
		return dockerArchiveProof{}, failure("archive_proof_invalid", "Managed archive proof is invalid", 412)
	}
	return proof, nil
}

func (e *PrivilegedExecutor) exportDockerImageArchive(ctx context.Context, instance Instance, image string) (dockerArchiveView, error) {
	var out dockerArchiveView
	before, err := e.inspectDockerImage(ctx, instance, image)
	if err != nil {
		return out, err
	}
	id, err := randomDockerArchiveID()
	if err != nil {
		return out, err
	}
	root, err := e.dockerArchiveRoot(instance)
	if err != nil {
		return out, err
	}
	defer root.Close()
	tempName := ".incoming-" + id
	file, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return out, failure("artifact_exists", "Docker archive could not be reserved", 409)
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = root.Remove(tempName)
		}
	}()
	response, err := e.dockerArchiveEngineRequest(ctx, instance, http.MethodGet, "/images/"+url.PathEscape(image)+"/get", nil, 0)
	if err != nil {
		return out, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return out, failure("not_found", "Digest-qualified Docker image does not exist", 404)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return out, failure("docker_operation_failed", "Docker rejected image archive export", 502)
	}
	if response.ContentLength > dockerArchiveMaxBytes {
		return out, failure("result_too_large", "Docker image archive exceeds four GiB", 413)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(&contextReader{ctx: ctx, reader: response.Body}, dockerArchiveMaxBytes+1))
	if copyErr != nil {
		return out, copyErr
	}
	if written < 1 || written > dockerArchiveMaxBytes {
		return out, failure("result_too_large", "Docker image archive is empty or exceeds four GiB", 413)
	}
	if err = file.Sync(); err != nil {
		return out, err
	}
	if err = file.Close(); err != nil {
		return out, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if err = validateDockerSaveArchive(ctx, filepath.Join(instance.Paths.BackupDir, tempName), image, before.ID); err != nil {
		return out, err
	}
	after, err := e.inspectDockerImage(ctx, instance, image)
	if err != nil || after.ID != before.ID {
		return out, failure("ownership_invalid", "Docker image changed while its archive was being saved", 412)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	proof, err := e.sealDockerArchiveProof(dockerArchiveProof{Version: 1, SourceInstanceID: instance.ID, Image: image, ImageID: before.ID, Bytes: written, SHA256: digest, CreatedAt: now})
	if err != nil {
		return out, err
	}
	if err = root.Rename(tempName, dockerArchiveFilename(id)); err != nil {
		return out, failure("artifact_commit_failed", "Docker image archive could not be committed", 500)
	}
	committed = true
	if err = syncDockerArchiveRoot(root); err != nil {
		_ = root.Remove(dockerArchiveFilename(id))
		return out, err
	}
	record := dockerImageArchive{ID: id, InstanceID: instance.ID, SourceInstanceID: instance.ID, Image: image, ImageID: before.ID, Bytes: written, SHA256: digest, CreatedAt: now, State: "ready", Proof: proof}
	if err = e.save("private", id, record); err != nil {
		_ = root.Remove(dockerArchiveFilename(id))
		return out, failure("operation_recovery_required", "Archive bytes exist but their protected ownership receipt could not be committed", 409)
	}
	return dockerArchivePublic(record, true), nil
}

func (e *PrivilegedExecutor) beginDockerImageArchiveUpload(ctx context.Context, instance Instance, image, token, actorID string) (map[string]any, error) {
	proof, err := e.openDockerArchiveProof(token)
	if err != nil {
		return nil, err
	}
	if proof.Image != image {
		return nil, failure("ownership_invalid", "Archive proof belongs to a different exact image digest", 412)
	}
	id, err := randomDockerArchiveID()
	if err != nil {
		return nil, err
	}
	root, err := e.dockerArchiveRoot(instance)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(dockerArchiveFilename(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, failure("artifact_exists", "Docker archive upload could not be reserved", 409)
	}
	if err = file.Sync(); err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		_ = root.Remove(dockerArchiveFilename(id))
		return nil, err
	}
	now := time.Now().UTC()
	record := dockerImageArchive{ID: id, InstanceID: instance.ID, SourceInstanceID: proof.SourceInstanceID, Image: image, ImageID: proof.ImageID, Bytes: proof.Bytes, SHA256: proof.SHA256, CreatedAt: now.Format(time.RFC3339Nano), State: "uploading", ActorID: actorID, Proof: token}
	if err = e.save("private", id, record); err != nil {
		_ = root.Remove(dockerArchiveFilename(id))
		return nil, failure("persistence_failed", "Docker archive upload receipt could not be persisted", 500)
	}
	if err = syncDockerArchiveRoot(root); err != nil {
		_ = root.Remove(dockerArchiveFilename(id))
		return nil, err
	}
	return map[string]any{"archive": dockerArchivePublic(record, false), "chunk_size": dockerArchiveChunk, "next_offset": int64(0), "complete": false}, nil
}

func dockerArchivePublic(record dockerImageArchive, includeProof bool) dockerArchiveView {
	view := dockerArchiveView{ArchiveID: record.ID, Image: record.Image, ImageID: record.ImageID, Bytes: record.Bytes, SHA256: record.SHA256, CreatedAt: record.CreatedAt, State: record.State}
	if includeProof {
		view.Proof = record.Proof
	}
	return view
}

func (e *PrivilegedExecutor) dockerImageArchiveStatus(instance Instance, image, archiveID, actorID string) (map[string]any, error) {
	var record dockerImageArchive
	if err := e.load("private", archiveID, &record); err != nil {
		return nil, err
	}
	if record.ID != archiveID || record.InstanceID != instance.ID || record.Image != image {
		return nil, failure("forbidden", "Archive belongs to a different managed image resource", 403)
	}
	if record.State == "uploading" {
		if record.ActorID != actorID {
			return nil, failure("forbidden", "Archive upload belongs to a different actor", 403)
		}
		if expiredDockerArchiveUpload(record) {
			return nil, failure("transfer_expired", "Docker archive upload expired; begin a fresh transfer", 410)
		}
	}
	return map[string]any{"archive": dockerArchivePublic(record, false), "next_offset": record.UploadOffset, "complete": record.State == "ready" || record.State == "loaded"}, nil
}

func expiredDockerArchiveUpload(record dockerImageArchive) bool {
	created, err := time.Parse(time.RFC3339Nano, record.CreatedAt)
	return err != nil || time.Since(created) > dockerArchiveMaxAge
}

func randomDockerArchiveID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "dockerarchive_" + hex.EncodeToString(random), nil
}

func (e *PrivilegedExecutor) dockerArchiveArtifactStream(ctx context.Context, request agent.Request, streams agent.IO) (result agent.Result, err error) {
	result = agent.Result{Operation: request.Operation, StartedAt: time.Now().UTC(), ExitCode: -1}
	defer func() { result.FinishedAt = time.Now().UTC() }()
	if err = validateDockerArchiveArtifact(request.Input); err != nil {
		return result, err
	}
	var in dockerArchiveArtifactInput
	if err = parse(request.Input, &in); err != nil {
		return result, err
	}
	if in.Direction == "upload" && (streams.Stdin == nil || streams.Stdout != nil || request.ActorID == "") {
		return result, failure("stream_required", "Archive upload requires an authenticated input stream only", 400)
	}
	if in.Direction == "download" && (streams.Stdout == nil || streams.Stdin != nil) {
		return result, failure("stream_required", "Archive download requires an output stream only", 400)
	}
	unlock, err := e.lockContext(ctx, in.ID)
	if err != nil {
		return result, err
	}
	defer unlock()
	instance, err := e.dockerArchiveInstance(ctx, in.ID)
	if err != nil {
		return result, err
	}
	var record dockerImageArchive
	if err = e.load("private", in.ArchiveID, &record); err != nil {
		return result, err
	}
	if record.ID != in.ArchiveID || record.InstanceID != in.ID || record.Image != in.Image {
		return result, failure("forbidden", "Archive belongs to a different managed image resource", 403)
	}
	if in.Direction == "upload" {
		return e.writeDockerImageArchiveChunk(ctx, instance, record, in, request.ActorID, streams.Stdin)
	}
	return e.readDockerImageArchiveChunk(ctx, instance, record, in, streams.Stdout)
}

func (e *PrivilegedExecutor) writeDockerImageArchiveChunk(ctx context.Context, instance Instance, record dockerImageArchive, in dockerArchiveArtifactInput, actorID string, source io.ReadCloser) (agent.Result, error) {
	result := agent.Result{Operation: "docker_image_archive_artifact", StartedAt: time.Now().UTC(), ExitCode: -1}
	if (record.State != "uploading" && record.State != "ready") || actorID == "" || actorID != record.ActorID || expiredDockerArchiveUpload(record) {
		return result, failure("forbidden", "Archive upload is expired or belongs to a different actor", 403)
	}
	if in.Offset+in.Length > record.Bytes {
		return result, failure("invalid_input", "Upload chunk exceeds the proof-bound archive size", 413)
	}
	stop := context.AfterFunc(ctx, func() { _ = source.Close() })
	defer stop()
	data, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: source}, in.Length+1))
	if err != nil {
		return result, err
	}
	if int64(len(data)) != in.Length {
		return result, failure("artifact_transfer_failed", "Upload chunk length does not match its descriptor", 400)
	}
	digest := sha256.Sum256(data)
	chunkHash := hex.EncodeToString(digest[:])
	if chunkHash != in.SHA256 {
		return result, failure("checksum_mismatch", "Upload chunk SHA-256 does not match", 412)
	}
	root, err := e.dockerArchiveRoot(instance)
	if err != nil {
		return result, err
	}
	defer root.Close()
	if in.Offset == record.LastChunkOffset && record.LastChunkBytes == in.Length && record.LastChunkSHA256 == chunkHash && record.UploadOffset >= in.Offset+in.Length {
		file, openErr := root.OpenFile(dockerArchiveFilename(record.ID), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if openErr != nil {
			return result, failure("artifact_invalid", "Staged Docker archive cannot be opened safely", 412)
		}
		existing := make([]byte, len(data))
		_, readErr := file.ReadAt(existing, in.Offset)
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(existing, data) {
			return result, failure("checksum_mismatch", "Repeated upload chunk differs from the committed chunk", 412)
		}
		result.StdoutBytes = int64(len(data))
		result.Data, _ = json.Marshal(map[string]any{"archive_id": record.ID, "next_offset": record.UploadOffset, "complete": record.State == "ready"})
		result.ExitCode = 0
		return result, nil
	}
	if record.State != "uploading" || in.Offset != record.UploadOffset {
		return result, failure("offset_conflict", "Upload offset does not match the committed archive length", 409)
	}
	file, err := root.OpenFile(dockerArchiveFilename(record.ID), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return result, failure("artifact_invalid", "Staged Docker archive cannot be opened safely", 412)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < record.UploadOffset {
		return result, failure("artifact_invalid", "Staged Docker archive differs from its transfer receipt", 412)
	}
	if info.Size() > record.UploadOffset {
		if err = file.Truncate(record.UploadOffset); err != nil {
			return result, err
		}
	}
	if _, err = file.Seek(record.UploadOffset, io.SeekStart); err != nil {
		return result, err
	}
	n, err := file.Write(data)
	if err != nil || n != len(data) {
		return result, failure("artifact_transfer_failed", "Upload chunk could not be committed", 502)
	}
	if err = file.Sync(); err != nil {
		return result, err
	}
	next := record.UploadOffset + int64(n)
	record.LastChunkOffset, record.LastChunkBytes, record.LastChunkSHA256 = record.UploadOffset, int64(n), chunkHash
	record.UploadOffset = next
	if next == record.Bytes {
		if err = verifyDockerArchiveFile(ctx, file, record); err != nil {
			record.State, record.ActorID = "failed", ""
			_ = e.save("private", record.ID, record)
			return result, err
		}
		if err = validateDockerSaveArchiveFile(ctx, file, record.Image, record.ImageID); err != nil {
			record.State, record.ActorID = "failed", ""
			_ = e.save("private", record.ID, record)
			return result, err
		}
		record.State = "ready"
	}
	if err = e.save("private", record.ID, record); err != nil {
		return result, failure("persistence_failed", "Upload chunk bytes were written but their offset was not committed", 409)
	}
	result.StdoutBytes = int64(n)
	result.Data, _ = json.Marshal(map[string]any{"archive_id": record.ID, "next_offset": next, "complete": record.State == "ready"})
	result.ExitCode = 0
	return result, nil
}

func (e *PrivilegedExecutor) readDockerImageArchiveChunk(ctx context.Context, instance Instance, record dockerImageArchive, in dockerArchiveArtifactInput, sink io.Writer) (agent.Result, error) {
	result := agent.Result{Operation: "docker_image_archive_artifact", StartedAt: time.Now().UTC(), ExitCode: -1}
	if record.State != "ready" && record.State != "loaded" {
		return result, failure("transfer_incomplete", "Docker image archive is not complete", 409)
	}
	if in.Offset > record.Bytes {
		return result, failure("invalid_input", "Download offset exceeds the owned archive", 400)
	}
	root, err := e.dockerArchiveRoot(instance)
	if err != nil {
		return result, err
	}
	defer root.Close()
	file, err := root.OpenFile(dockerArchiveFilename(record.ID), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return result, failure("artifact_invalid", "Owned Docker archive cannot be opened safely", 412)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != record.Bytes || info.Size() > dockerArchiveMaxBytes {
		return result, failure("artifact_invalid", "Owned Docker archive differs from its protected receipt", 412)
	}
	if _, err = file.Seek(in.Offset, io.SeekStart); err != nil {
		return result, err
	}
	length := min(in.Length, record.Bytes-in.Offset)
	chunkHash := sha256.New()
	written, err := io.CopyN(io.MultiWriter(sink, chunkHash), &contextReader{ctx: ctx, reader: file}, length)
	result.StdoutBytes = written
	if err != nil || written != length {
		return result, failure("artifact_transfer_failed", "Docker archive download chunk was incomplete", 502)
	}
	result.Data, _ = json.Marshal(map[string]any{"archive_id": record.ID, "bytes": record.Bytes, "sha256": record.SHA256, "chunk_sha256": hex.EncodeToString(chunkHash.Sum(nil)), "offset": in.Offset, "next_offset": in.Offset + written, "eof": in.Offset+written >= record.Bytes})
	result.ExitCode = 0
	return result, nil
}

func (e *PrivilegedExecutor) importDockerImageArchive(ctx context.Context, instance Instance, image, archiveID, actorID string) (map[string]any, error) {
	var record dockerImageArchive
	if err := e.load("private", archiveID, &record); err != nil {
		return nil, err
	}
	if record.ID != archiveID || record.InstanceID != instance.ID || record.Image != image {
		return nil, failure("forbidden", "Archive belongs to a different managed image resource", 403)
	}
	proof, err := e.openDockerArchiveProof(record.Proof)
	if err != nil || proof.SourceInstanceID != record.SourceInstanceID || proof.Image != image || proof.ImageID != record.ImageID || proof.Bytes != record.Bytes || proof.SHA256 != record.SHA256 {
		return nil, failure("ownership_invalid", "Archive receipt does not match its protected export proof", 412)
	}
	switch record.State {
	case "loaded":
		if record.Provenance == "engine_digest" {
			loaded, inspectErr := e.inspectDockerImage(ctx, instance, image)
			if inspectErr != nil || loaded.ID != record.ImageID {
				return nil, failure("ownership_invalid", "Previously loaded exact digest no longer resolves to its proof-bound Engine image", 412)
			}
		} else if err := e.verifyDockerImageArchiveAlias(ctx, instance, record); err != nil {
			return nil, err
		}
		provenance := record.Provenance
		if provenance == "" {
			provenance = "managed_archive"
		}
		return map[string]any{"archive_id": record.ID, "image": image, "image_id": record.ImageID, "loaded": true, "already_loaded": true, "sha256": record.SHA256, "provenance": provenance}, nil
	case "loading":
		return nil, failure("operation_recovery_required", "Docker archive load was interrupted; inspect the image before retrying", 409)
	case "failed":
		return nil, failure("archive_invalid", "Docker archive transfer failed validation and cannot be loaded", 412)
	case "uploading":
		if record.ActorID != actorID {
			return nil, failure("forbidden", "Archive upload belongs to a different actor", 403)
		}
		if expiredDockerArchiveUpload(record) {
			return nil, failure("transfer_expired", "Docker archive upload expired; begin a fresh transfer", 410)
		}
		if record.UploadOffset != record.Bytes {
			return nil, failure("transfer_incomplete", "Docker archive upload has not reached its proof-bound size", 409)
		}
	case "ready":
		if record.ActorID != actorID {
			return nil, failure("forbidden", "Archive upload belongs to a different actor", 403)
		}
	default:
		return nil, failure("invalid_state", "Docker archive receipt has an unknown state", 500)
	}
	root, err := e.dockerArchiveRoot(instance)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	file, err := root.OpenFile(dockerArchiveFilename(record.ID), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, failure("artifact_invalid", "Owned Docker archive cannot be opened safely", 412)
	}
	defer file.Close()
	if err = verifyDockerArchiveFile(ctx, file, record); err != nil {
		return nil, err
	}
	if err = validateDockerSaveArchiveFile(ctx, file, image, record.ImageID); err != nil {
		return nil, err
	}
	before, beforeErr := e.inspectDockerImage(ctx, instance, image)
	if beforeErr == nil {
		if before.ID != record.ImageID {
			return nil, failure("ownership_invalid", "Target Engine resolves the exact digest to a different image ID", 412)
		}
		record.State, record.ActorID, record.Provenance = "loaded", "", "engine_digest"
		if err = e.save("private", record.ID, record); err != nil {
			return nil, failure("operation_recovery_required", "Existing image is verified but archive completion could not be persisted", 409)
		}
		return map[string]any{"archive_id": record.ID, "image": image, "image_id": record.ImageID, "loaded": true, "already_loaded": true, "sha256": record.SHA256, "provenance": "engine_digest"}, nil
	}
	if core.ErrorCode(beforeErr) != "not_found" && core.ErrorCode(beforeErr) != "ownership_invalid" {
		return nil, beforeErr
	}
	// Reuse an existing proof-bound image ID only while the Engine has no
	// mutable tags or digest aliases that the digest ACL did not authorize.
	preexisting, preexistingErr := e.inspectDockerImageByID(ctx, instance, record.ImageID)
	if preexistingErr == nil {
		if dockerImageHasTags(preexisting.RepoTags) || len(preexisting.RepoDigests) != 0 {
			return nil, failure("ownership_invalid", "Target already has this image ID under ungranted Docker aliases", 412)
		}
		record.State, record.ActorID, record.Provenance = "loaded", "", "managed_archive"
		if err = e.save("private", record.ID, record); err != nil {
			return nil, failure("operation_recovery_required", "Existing proof-bound image is verified but archive completion could not be persisted", 409)
		}
		if err = e.saveDockerArchiveImageAlias(record); err != nil {
			return nil, failure("operation_recovery_required", "Existing proof-bound image is verified but its digest alias could not be persisted", 409)
		}
		return map[string]any{"archive_id": record.ID, "image": image, "image_id": record.ImageID, "loaded": true, "already_loaded": true, "sha256": record.SHA256, "provenance": "managed_archive"}, nil
	}
	if core.ErrorCode(preexistingErr) != "not_found" {
		return nil, preexistingErr
	}
	record.State, record.ActorID = "loading", ""
	if err = e.save("private", record.ID, record); err != nil {
		return nil, failure("persistence_failed", "Docker archive load intent could not be persisted", 500)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	loadReader, loadWriter := io.Pipe()
	loadDone := make(chan error, 1)
	go func() {
		streamErr := streamDockerArchiveForLoad(ctx, file, loadWriter)
		_ = loadWriter.CloseWithError(streamErr)
		loadDone <- streamErr
	}()
	response, requestErr := e.dockerArchiveEngineRequest(ctx, instance, http.MethodPost, "/images/load", loadReader, -1)
	if requestErr != nil {
		_ = loadReader.CloseWithError(requestErr)
		streamErr := <-loadDone
		if streamErr != nil {
			return nil, streamErr
		}
		return nil, requestErr
	}
	responseErr := validateDockerLoadResponse(response)
	_ = response.Body.Close()
	if responseErr != nil {
		_ = loadReader.CloseWithError(responseErr)
	}
	streamErr := <-loadDone
	if responseErr != nil {
		return nil, responseErr
	}
	if streamErr != nil {
		return nil, streamErr
	}
	loaded, err := e.inspectDockerImageByID(ctx, instance, record.ImageID)
	if err != nil || loaded.ID != record.ImageID {
		return nil, failure("operation_recovery_required", "Docker accepted the archive but the proof-bound image ID could not be verified", 409)
	}
	if dockerImageHasTags(loaded.RepoTags) || len(loaded.RepoDigests) != 0 {
		return nil, failure("ownership_invalid", "Docker loaded image carries mutable aliases outside the ID-only archive proof", 412)
	}
	record.State, record.Provenance = "loaded", "managed_archive"
	if err = e.save("private", record.ID, record); err != nil {
		return nil, failure("operation_recovery_required", "Docker image loaded but archive completion could not be persisted", 409)
	}
	if err = e.saveDockerArchiveImageAlias(record); err != nil {
		return nil, failure("operation_recovery_required", "Docker image loaded but its exact digest alias could not be persisted", 409)
	}
	return map[string]any{"archive_id": record.ID, "image": image, "image_id": record.ImageID, "loaded": true, "already_loaded": false, "sha256": record.SHA256, "provenance": "managed_archive"}, nil
}

func (e *PrivilegedExecutor) inspectDockerImageByID(ctx context.Context, instance Instance, imageID string) (dockerImageInspection, error) {
	var inspected dockerImageInspection
	if !dockerImageID.MatchString(imageID) {
		return inspected, failure("invalid_input", "Invalid immutable Docker image ID", 400)
	}
	if err := e.dockerRequest(ctx, instance, http.MethodGet, "/images/"+url.PathEscape(imageID)+"/json", nil, &inspected); err != nil {
		return dockerImageInspection{}, err
	}
	if inspected.ID != imageID || inspected.Size < 0 || len(inspected.RepoDigests) > 256 || len(inspected.RepoTags) > 256 {
		return dockerImageInspection{}, failure("invalid_response", "Docker returned invalid loaded image metadata", 502)
	}
	for _, ref := range inspected.RepoDigests {
		if !dockerImage.MatchString(ref) {
			return dockerImageInspection{}, failure("invalid_response", "Docker returned an invalid loaded RepoDigest", 502)
		}
	}
	for _, tag := range inspected.RepoTags {
		if tag == "<none>:<none>" {
			continue
		}
		if len(tag) > 255 || strings.ContainsAny(tag, "\x00\r\n\t ") {
			return dockerImageInspection{}, failure("invalid_response", "Docker returned an invalid loaded RepoTag", 502)
		}
	}
	return inspected, nil
}

func dockerArchiveImageAliasID(instanceID, image string) string {
	digest := sha256.Sum256([]byte(instanceID + "\x00" + image))
	return "dockerarchivealias_" + hex.EncodeToString(digest[:16])
}

func dockerArchiveImageAliasIndexID(instanceID string) string {
	digest := sha256.Sum256([]byte(instanceID + "\x00docker-image-alias-index"))
	return "dockerarchiveindex_" + hex.EncodeToString(digest[:16])
}

func dockerImageHasTags(tags []string) bool {
	for _, tag := range tags {
		if tag != "<none>:<none>" {
			return true
		}
	}
	return false
}

func (e *PrivilegedExecutor) saveDockerArchiveImageAlias(record dockerImageArchive) error {
	if record.State != "loaded" || record.Provenance != "managed_archive" || !validInstanceID(record.InstanceID) || !dockerImage.MatchString(record.Image) || !dockerImageID.MatchString(record.ImageID) || !dockerArchiveID.MatchString(record.ID) {
		return failure("ownership_invalid", "Loaded Docker archive receipt is invalid", 412)
	}
	proof, err := e.openDockerArchiveProof(record.Proof)
	if err != nil || proof.SourceInstanceID != record.SourceInstanceID || proof.Image != record.Image || proof.ImageID != record.ImageID || proof.Bytes != record.Bytes || proof.SHA256 != record.SHA256 {
		return failure("ownership_invalid", "Loaded Docker archive receipt does not match its export proof", 412)
	}
	alias := dockerArchiveImageAlias{Version: 1, InstanceID: record.InstanceID, Image: record.Image, ImageID: record.ImageID, ArchiveID: record.ID, Bytes: record.Bytes, SHA256: record.SHA256, Proof: record.Proof, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = e.save("private", dockerArchiveImageAliasID(record.InstanceID, record.Image), alias); err != nil {
		return err
	}
	indexID := dockerArchiveImageAliasIndexID(record.InstanceID)
	var index dockerArchiveImageAliasIndex
	if err = e.load("private", indexID, &index); err != nil {
		if core.ErrorCode(err) != "not_found" {
			return err
		}
		index = dockerArchiveImageAliasIndex{InstanceID: record.InstanceID}
	}
	if index.InstanceID != record.InstanceID || len(index.Images) > dockerArchiveMaxAliases {
		return failure("ownership_invalid", "Managed Docker image alias index is invalid", 412)
	}
	for _, image := range index.Images {
		if image == record.Image {
			return nil
		}
	}
	if len(index.Images) >= dockerArchiveMaxAliases {
		return failure("result_too_large", "Managed restored image aliases exceed the 4096-image bound", 413)
	}
	index.Images = append(index.Images, record.Image)
	sort.Strings(index.Images)
	return e.save("private", indexID, index)
}

func (e *PrivilegedExecutor) loadDockerArchiveImageAlias(instanceID, image string) (dockerArchiveImageAlias, error) {
	var alias dockerArchiveImageAlias
	id := dockerArchiveImageAliasID(instanceID, image)
	if err := e.load("private", id, &alias); err != nil {
		return dockerArchiveImageAlias{}, err
	}
	if !dockerArchiveAliasID.MatchString(id) || alias.Version != 1 || alias.InstanceID != instanceID || alias.Image != image || !dockerImageID.MatchString(alias.ImageID) || !dockerArchiveID.MatchString(alias.ArchiveID) || alias.Bytes < 1 || alias.Bytes > dockerArchiveMaxBytes || !artifactHash.MatchString(alias.SHA256) {
		return dockerArchiveImageAlias{}, failure("ownership_invalid", "Managed Docker image alias receipt is invalid", 412)
	}
	proof, err := e.openDockerArchiveProof(alias.Proof)
	if err != nil || proof.Image != alias.Image || proof.ImageID != alias.ImageID || proof.Bytes != alias.Bytes || proof.SHA256 != alias.SHA256 {
		return dockerArchiveImageAlias{}, failure("ownership_invalid", "Managed Docker image alias does not match its archive proof", 412)
	}
	if _, err = time.Parse(time.RFC3339Nano, alias.CreatedAt); err != nil {
		return dockerArchiveImageAlias{}, failure("ownership_invalid", "Managed Docker image alias has an invalid creation time", 412)
	}
	var archive dockerImageArchive
	if err = e.load("private", alias.ArchiveID, &archive); err != nil || archive.ID != alias.ArchiveID || archive.InstanceID != alias.InstanceID || archive.State != "loaded" || archive.Provenance != "managed_archive" || archive.Image != alias.Image || archive.ImageID != alias.ImageID || archive.Bytes != alias.Bytes || archive.SHA256 != alias.SHA256 || archive.Proof != alias.Proof {
		return dockerArchiveImageAlias{}, failure("ownership_invalid", "Managed Docker image alias no longer matches its loaded archive receipt", 412)
	}
	return alias, nil
}

func (e *PrivilegedExecutor) resolveDockerImageArchive(ctx context.Context, instance Instance, image string) (dockerImageInspection, bool, error) {
	metadata, err := e.inspectDockerImageMetadata(ctx, instance, image)
	if err == nil {
		for _, ref := range metadata.RepoDigests {
			if dockerImageDigestProofMatches(image, ref) {
				return metadata, false, nil
			}
		}
		if len(metadata.RepoDigests) != 0 || dockerImageHasTags(metadata.RepoTags) {
			return dockerImageInspection{}, false, failure("ownership_invalid", "Docker did not confirm the exact digest-qualified image reference", 412)
		}
		alias, aliasErr := e.loadDockerArchiveImageAlias(instance.ID, image)
		if aliasErr != nil || alias.ImageID != metadata.ID {
			return dockerImageInspection{}, false, failure("ownership_invalid", "Image ID has no matching managed archive proof for this exact digest", 412)
		}
		return metadata, true, nil
	}
	if core.ErrorCode(err) != "not_found" {
		return dockerImageInspection{}, false, err
	}
	alias, aliasErr := e.loadDockerArchiveImageAlias(instance.ID, image)
	if aliasErr != nil {
		return dockerImageInspection{}, false, err
	}
	metadata, err = e.inspectDockerImageByID(ctx, instance, alias.ImageID)
	if err != nil {
		return dockerImageInspection{}, false, err
	}
	if metadata.ID != alias.ImageID || dockerImageHasTags(metadata.RepoTags) || len(metadata.RepoDigests) != 0 {
		return dockerImageInspection{}, false, failure("ownership_invalid", "Restored image no longer has its proof-bound ID-only Engine state", 412)
	}
	return metadata, true, nil
}

func (e *PrivilegedExecutor) verifyDockerImageArchiveAlias(ctx context.Context, instance Instance, record dockerImageArchive) error {
	if record.Provenance == "engine_digest" {
		return failure("invalid_state", "Engine digest import does not use an archive alias", 412)
	}
	loaded, err := e.inspectDockerImageByID(ctx, instance, record.ImageID)
	if err != nil || loaded.ID != record.ImageID || dockerImageHasTags(loaded.RepoTags) || len(loaded.RepoDigests) != 0 {
		return failure("ownership_invalid", "Restored image no longer has its proof-bound ID-only Engine state", 412)
	}
	if err = e.saveDockerArchiveImageAlias(record); err != nil {
		return failure("operation_recovery_required", "Restored image is present but its digest alias could not be persisted", 409)
	}
	return nil
}

func (e *PrivilegedExecutor) dockerArchiveImageAliases(instance Instance, images []dockerImageInspection) ([]dockerImageView, error) {
	available := make(map[string]dockerImageInspection, len(images))
	for _, image := range images {
		if dockerImageID.MatchString(image.ID) && !dockerImageHasTags(image.RepoTags) && len(image.RepoDigests) == 0 {
			available[image.ID] = image
		}
	}
	if len(available) == 0 {
		return []dockerImageView{}, nil
	}
	var index dockerArchiveImageAliasIndex
	if err := e.load("private", dockerArchiveImageAliasIndexID(instance.ID), &index); err != nil {
		if core.ErrorCode(err) == "not_found" {
			return []dockerImageView{}, nil
		}
		return nil, err
	}
	if index.InstanceID != instance.ID || len(index.Images) > dockerArchiveMaxAliases {
		return nil, failure("ownership_invalid", "Managed Docker image alias index is invalid", 412)
	}
	views := []dockerImageView{}
	seen := map[string]bool{}
	for _, image := range index.Images {
		if !dockerImage.MatchString(image) || seen[image] {
			return nil, failure("ownership_invalid", "Managed Docker image alias index contains an invalid entry", 412)
		}
		seen[image] = true
		alias, err := e.loadDockerArchiveImageAlias(instance.ID, image)
		if err != nil {
			continue
		}
		metadata, ok := available[alias.ImageID]
		if !ok {
			continue
		}
		if alias.ImageID != metadata.ID {
			continue
		}
		views = append(views, dockerImageView{Image: alias.Image, ID: metadata.ID, SizeBytes: metadata.Size, RestoredFromArchive: true})
	}
	return views, nil
}

func (e *PrivilegedExecutor) dockerArchiveEngineRequest(ctx context.Context, instance Instance, method, endpoint string, body io.Reader, length int64) (*http.Response, error) {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	}}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Minute}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker/v1.45"+endpoint, body)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	if body != nil {
		request.ContentLength = length
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/x-tar")
		}
	}
	response, err := client.Do(request)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, failure("docker_unavailable", "Protected Docker socket is unavailable", 502)
	}
	response.Body = &dockerArchiveResponseBody{ReadCloser: response.Body, close: transport.CloseIdleConnections}
	return response, nil
}

type dockerArchiveResponseBody struct {
	io.ReadCloser
	close func()
}

func (b *dockerArchiveResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.close()
	return err
}

func validateDockerLoadResponse(response *http.Response) error {
	if response.StatusCode == http.StatusNotFound {
		return failure("not_found", "Docker image load endpoint is unavailable", 404)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return failure("image_load_failed", "Docker rejected the verified image archive", 502)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return failure("invalid_response", "Docker image load response exceeded its bound", 502)
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event map[string]any
		if json.Unmarshal(line, &event) != nil || event["error"] != nil || event["errorDetail"] != nil {
			return failure("image_load_failed", "Docker reported an image archive load error", 502)
		}
	}
	return nil
}

// streamDockerArchiveForLoad removes all mutable tag metadata before passing a
// verified image to Docker. The original archive remains byte-for-byte bound
// to its protected checksum receipt; this stream preserves the image config and
// layers while importing it by image ID only.
func streamDockerArchiveForLoad(ctx context.Context, source io.Reader, destination io.Writer) error {
	archive := tar.NewReader(&contextReader{ctx: ctx, reader: source})
	bounded := &dockerArchiveBoundedWriter{writer: destination, maximum: dockerArchiveMaxBytes}
	writer := tar.NewWriter(bounded)
	manifestSeen := false
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failure("archive_invalid", "Docker save archive changed during import", 412)
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if header.Name == "repositories" {
			continue
		}
		if header.Name == "manifest.json" {
			if manifestSeen || header.Size > 1<<20 {
				return failure("archive_invalid", "Docker save manifest changed during import", 412)
			}
			manifestSeen = true
			data, readErr := io.ReadAll(io.LimitReader(archive, header.Size+1))
			if readErr != nil || int64(len(data)) != header.Size {
				return failure("archive_invalid", "Docker save manifest changed during import", 412)
			}
			var entries []dockerSaveManifestEntry
			if json.Unmarshal(data, &entries) != nil || len(entries) != 1 {
				return failure("archive_invalid", "Docker save archive must contain exactly one image", 412)
			}
			entries[0].RepoTags = []string{}
			data, err = json.Marshal(entries)
			if err != nil {
				return err
			}
			copyHeader := *header
			copyHeader.Size = int64(len(data))
			if err = writer.WriteHeader(&copyHeader); err != nil {
				return err
			}
			if _, err = writer.Write(data); err != nil {
				return err
			}
			continue
		}
		if err = writer.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			if _, err = io.CopyN(writer, &contextReader{ctx: ctx, reader: archive}, header.Size); err != nil {
				return failure("archive_invalid", "Docker save archive changed during import", 412)
			}
		}
	}
	if !manifestSeen {
		return failure("archive_invalid", "Docker save archive lacks its image manifest", 412)
	}
	return writer.Close()
}

type dockerArchiveBoundedWriter struct {
	writer  io.Writer
	maximum int64
	written int64
}

func (w *dockerArchiveBoundedWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.maximum-w.written {
		return 0, failure("archive_limit", "Sanitized Docker image archive exceeds four GiB", 413)
	}
	n, err := w.writer.Write(data)
	w.written += int64(n)
	return n, err
}

func verifyDockerArchiveFile(ctx context.Context, file *os.File, record dockerImageArchive) error {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != record.Bytes || record.Bytes < 1 || record.Bytes > dockerArchiveMaxBytes {
		return failure("artifact_invalid", "Docker archive differs from its protected size receipt", 412)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.LimitReader(&contextReader{ctx: ctx, reader: file}, dockerArchiveMaxBytes+1))
	if err != nil {
		return err
	}
	if count != record.Bytes || hex.EncodeToString(hash.Sum(nil)) != record.SHA256 {
		return failure("checksum_mismatch", "Docker archive SHA-256 differs from its protected proof", 412)
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

type dockerSaveManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

func validateDockerSaveArchive(ctx context.Context, filename, image, imageID string) error {
	file, err := os.OpenFile(filename, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return failure("artifact_invalid", "Docker save archive cannot be opened safely", 412)
	}
	defer file.Close()
	return validateDockerSaveArchiveFile(ctx, file, image, imageID)
}

func validateDockerSaveArchiveFile(ctx context.Context, file *os.File, image, imageID string) error {
	if !dockerImage.MatchString(image) || !dockerImageID.MatchString(imageID) {
		return failure("invalid_input", "Invalid image archive ownership proof", 400)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	reader := tar.NewReader(&contextReader{ctx: ctx, reader: file})
	seen, fileNames := map[string]bool{}, map[string]bool{}
	var manifestData, configData, repositoriesData []byte
	count := 0
	var extracted int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failure("archive_invalid", "Docker save archive tar structure is invalid", 412)
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		count++
		if count > maxArtifactFiles || header.Size < 0 || extracted+header.Size > dockerArchiveMaxBytes {
			return failure("archive_limit", "Docker save archive exceeds its file or byte limits", 413)
		}
		extracted += header.Size
		if header.Mode&06000 != 0 || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA && header.Typeflag != tar.TypeDir {
			return failure("archive_unsafe_entry", "Docker save archive contains a link, device or privileged entry", 412)
		}
		clean, err := archivePath(header.Name, 0)
		if err != nil || clean == "" {
			return failure("archive_unsafe_entry", "Docker save archive contains an unsafe path", 412)
		}
		name := strings.TrimSuffix(clean, "/")
		if seen[name] {
			return failure("archive_invalid", "Docker save archive contains duplicate paths", 412)
		}
		seen[name] = true
		if header.Typeflag == tar.TypeDir {
			continue
		}
		fileNames[name] = true
		switch name {
		case "manifest.json":
			if header.Size > 1<<20 {
				return failure("archive_limit", "Docker save manifest exceeds one MiB", 413)
			}
			manifestData, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
		case "repositories":
			if header.Size > 1<<20 {
				return failure("archive_limit", "Docker save repository map exceeds one MiB", 413)
			}
			repositoriesData, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
		default:
			configName := strings.TrimPrefix(imageID, "sha256:") + ".json"
			if name == configName {
				if header.Size > 4<<20 {
					return failure("archive_limit", "Docker image configuration exceeds four MiB", 413)
				}
				configData, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
			}
		}
		if err != nil || (name == "manifest.json" && int64(len(manifestData)) != header.Size) || (name == "repositories" && int64(len(repositoriesData)) != header.Size) || (name == strings.TrimPrefix(imageID, "sha256:")+".json" && int64(len(configData)) != header.Size) {
			return failure("archive_invalid", "Docker save archive contains a truncated metadata file", 412)
		}
	}
	if manifestData == nil || configData == nil {
		return failure("archive_invalid", "Docker save archive lacks its image manifest or config", 412)
	}
	configHash := sha256.Sum256(configData)
	if "sha256:"+hex.EncodeToString(configHash[:]) != imageID {
		return failure("ownership_invalid", "Docker archive config does not match the proof-bound Engine image ID", 412)
	}
	var manifest []dockerSaveManifestEntry
	if json.Unmarshal(manifestData, &manifest) != nil || len(manifest) != 1 {
		return failure("archive_invalid", "Docker save archive must contain exactly one image", 412)
	}
	entry := manifest[0]
	configName := strings.TrimPrefix(imageID, "sha256:") + ".json"
	if entry.Config != configName || len(entry.Layers) > maxArtifactFiles {
		return failure("ownership_invalid", "Docker archive manifest does not identify the proof-bound image", 412)
	}
	layerFiles, layerSidecars := map[string]bool{}, map[string]bool{}
	for _, layer := range entry.Layers {
		clean, pathErr := archivePath(layer, 0)
		if pathErr != nil || clean != layer || !strings.HasSuffix(clean, "/layer.tar") {
			return failure("archive_unsafe_entry", "Docker archive manifest contains an unsafe layer path", 412)
		}
		layerID := strings.TrimSuffix(clean, "/layer.tar")
		if !dockerArchiveLayerID.MatchString(layerID) || layerFiles[clean] {
			return failure("archive_invalid", "Docker archive manifest contains an invalid or duplicate layer", 412)
		}
		layerFiles[clean] = true
		layerSidecars[layerID+"/VERSION"] = true
		layerSidecars[layerID+"/json"] = true
	}
	for _, tag := range entry.RepoTags {
		if !dockerArchiveTagMatchesImage(image, tag) {
			return failure("ownership_invalid", "Docker archive contains a tag outside the proof-bound repository", 412)
		}
	}
	if len(repositoriesData) > 0 {
		var repositories map[string]map[string]string
		if json.Unmarshal(repositoriesData, &repositories) != nil || len(repositories) > maxArtifactFiles {
			return failure("archive_invalid", "Docker archive repository map is invalid", 412)
		}
		for repository, tags := range repositories {
			if len(tags) > maxArtifactFiles {
				return failure("archive_limit", "Docker archive repository map exceeds its tag limit", 413)
			}
			for tag := range tags {
				if !dockerArchiveTagMatchesImage(image, repository+":"+tag) {
					return failure("ownership_invalid", "Docker archive repository map contains an unrelated tag", 412)
				}
			}
		}
	}
	expected := map[string]bool{"manifest.json": true, configName: true}
	if len(repositoriesData) > 0 {
		expected["repositories"] = true
	}
	for name := range layerFiles {
		expected[name] = true
	}
	for name := range layerSidecars {
		expected[name] = true
	}
	for name := range fileNames {
		if !expected[name] {
			return failure("ownership_invalid", "Docker archive contains files outside its single proof-bound image", 412)
		}
	}
	for name := range layerFiles {
		if !fileNames[name] {
			return failure("archive_invalid", "Docker archive is missing a manifest layer", 412)
		}
	}
	for name := range layerSidecars {
		if !fileNames[name] {
			return failure("archive_invalid", "Docker archive is missing a layer ownership sidecar", 412)
		}
	}
	_, err := file.Seek(0, io.SeekStart)
	return err
}

func dockerArchiveTagMatchesImage(image, tag string) bool {
	if !dockerImage.MatchString(image) || strings.ContainsAny(tag, "\x00\r\n\\") {
		return false
	}
	slash, colon := strings.LastIndex(tag, "/"), strings.LastIndex(tag, ":")
	if colon <= slash || colon == len(tag)-1 || !dockerArchiveTag.MatchString(tag[colon+1:]) {
		return false
	}
	repository := tag[:colon]
	_, digest, ok := strings.Cut(image, "@")
	return ok && dockerImageDigestProofMatches(image, repository+"@"+digest)
}

func syncDockerArchiveRoot(root *os.Root) error {
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

func archiveResourceInput(raw json.RawMessage) ([]string, error) {
	var input struct {
		ID    string `json:"id"`
		Image string `json:"image"`
	}
	if json.Unmarshal(raw, &input) != nil || !validInstanceID(input.ID) || !dockerImage.MatchString(input.Image) {
		return nil, failure("invalid_input", "A managed Docker instance and immutable image digest are required", 400)
	}
	return []string{InstanceResource(input.ID), DockerImageResource(input.ID, input.Image)}, nil
}

func dockerArchiveResources(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
	return archiveResourceInput(raw)
}

func dockerArchiveActor(ctx context.Context, m *Manager, actor core.Actor, scope string, resources []string) (core.Actor, error) {
	if m.deps.ResolveActor != nil {
		current, err := m.deps.ResolveActor(ctx, actor)
		if err != nil {
			return core.Actor{}, err
		}
		actor = current
	}
	for _, resource := range resources {
		if !m.deps.Authorize(actor, scope, resource) {
			return core.Actor{}, failure("forbidden", "Docker archive grant is no longer valid", 403)
		}
	}
	return actor, nil
}

func (m *Manager) dockerArchiveAgentJSON(ctx context.Context, actor core.Actor, in DockerArchiveAction, out any) error {
	if m.executor == nil {
		return failure("agent_unavailable", "A protected Docker Agent is required for image archives", 503)
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	request := agent.Request{ActorID: actor.ID, Operation: "docker_image_archive_action", Input: raw}
	if err = m.executor.Validate(request); err != nil {
		return failure("invalid_input", "Agent rejected the typed Docker archive request", 400)
	}
	result, err := m.executor.Run(ctx, request)
	if err != nil || result.ExitCode != 0 || result.Truncated || len(result.Stdout) > 1<<20 {
		return dockerArchiveAgentError(result, err)
	}
	if err = json.Unmarshal([]byte(result.Stdout), out); err != nil {
		return failure("agent_protocol_error", "Docker Agent returned invalid bounded archive metadata", 502)
	}
	return nil
}

func dockerArchiveAgentError(result agent.Result, err error) error {
	var problem struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Status  int    `json:"status"`
		Details any    `json:"details"`
	}
	if json.Unmarshal([]byte(result.Stderr), &problem) == nil && problem.Code != "" && problem.Status >= 400 {
		return &core.Error{Code: problem.Code, Message: problem.Message, Status: problem.Status, Details: problem.Details}
	}
	if err != nil {
		var remote *agent.RemoteError
		if errors.As(err, &remote) && remote.Code != "" && remote.Status >= 400 {
			return &core.Error{Code: remote.Code, Message: remote.Message, Status: remote.Status}
		}
	}
	return failure("agent_operation_failed", "Docker archive operation failed; inspect its task and prerequisites", 502)
}

func (m *Manager) dockerArchiveAgentStream(ctx context.Context, actor core.Actor, input any, streams agent.IO) (agent.Result, error) {
	if m.executor == nil {
		return agent.Result{}, failure("agent_unavailable", "A protected Docker Agent is required for image archives", 503)
	}
	streamer, ok := m.executor.(interface {
		Stream(context.Context, agent.Request, agent.IO) (agent.Result, error)
	})
	if !ok {
		return agent.Result{}, failure("stream_unavailable", "Protected Agent streaming is required for Docker image archives", 412)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return agent.Result{}, err
	}
	request := agent.Request{ActorID: actor.ID, Operation: "docker_image_archive_artifact", Input: raw}
	if err = m.executor.Validate(request); err != nil {
		return agent.Result{}, failure("invalid_input", "Agent rejected the typed Docker archive stream descriptor", 400)
	}
	result, err := streamer.Stream(ctx, request, streams)
	if err != nil || result.ExitCode != 0 {
		return result, dockerArchiveAgentError(result, err)
	}
	return result, nil
}

func (m *Manager) dockerArchiveBindings() []core.Binding {
	export := m.binding("docker_image_archive_export", "Save one exact digest-owned Docker image into a managed archive", "containers:read", false,
		obj(map[string]any{"id": idSchema(), "image": map[string]any{"type": "string", "pattern": dockerImage.String()}}, "id", "image"), dockerArchiveResources,
		func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			resources, err := archiveResourceInput(raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			actor, err = dockerArchiveActor(ctx, m, actor, "containers:read", resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			var in struct {
				ID    string `json:"id"`
				Image string `json:"image"`
			}
			if err = parse(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			forward, _ := json.Marshal(DockerArchiveAction{ID: in.ID, Image: in.Image, Command: "export"})
			job, err := m.deps.Tasks.Submit(ctx, actor, "docker_image_archive_action", forward, resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			taskID, _ := job["id"].(string)
			return core.ActionResult{Data: job, TaskID: taskID, Resources: resources, AuditSummary: map[string]any{"image_resource": resources[1]}}, nil
		})
	export.Action.Task, export.Action.Audit = true, true

	begin := m.binding("docker_image_archive_upload_begin", "Stage an archive only with Agent proof from an exact digest-owned export", "containers:manage", false,
		obj(map[string]any{"id": idSchema(), "image": map[string]any{"type": "string", "pattern": dockerImage.String()}, "proof": map[string]any{"type": "string", "minLength": 32, "maxLength": 4096}}, "id", "image", "proof"), dockerArchiveResources,
		func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			resources, err := archiveResourceInput(raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			actor, err = dockerArchiveActor(ctx, m, actor, "containers:manage", resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			var in struct {
				ID    string `json:"id"`
				Image string `json:"image"`
				Proof string `json:"proof"`
			}
			if err = parse(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			var output map[string]any
			err = m.dockerArchiveAgentJSON(ctx, actor, DockerArchiveAction{ID: in.ID, Image: in.Image, Command: "upload_begin", Proof: in.Proof}, &output)
			return core.ActionResult{Data: output, Resources: resources, AuditSummary: map[string]any{"image_resource": resources[1]}}, err
		})
	begin.Action.Audit = true

	write := m.binding("docker_image_archive_upload_chunk", "Upload one bounded SHA-256 checked chunk to an owned Docker archive", "containers:manage", false,
		obj(map[string]any{"id": idSchema(), "image": map[string]any{"type": "string", "pattern": dockerImage.String()}, "archive_id": map[string]any{"type": "string", "pattern": dockerArchiveID.String()}, "offset": map[string]any{"type": "integer", "minimum": 0, "maximum": dockerArchiveMaxBytes}, "sha256": map[string]any{"type": "string", "pattern": artifactHash.String()}, "data": map[string]any{"type": "string", "minLength": 4, "maxLength": base64.StdEncoding.EncodedLen(dockerArchiveChunk), "pattern": "^[A-Za-z0-9+/]*={0,2}$"}}, "id", "image", "archive_id", "offset", "sha256", "data"), dockerArchiveResources,
		func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			resources, err := archiveResourceInput(raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			actor, err = dockerArchiveActor(ctx, m, actor, "containers:manage", resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			var in struct {
				ID        string `json:"id"`
				Image     string `json:"image"`
				ArchiveID string `json:"archive_id"`
				Offset    int64  `json:"offset"`
				SHA256    string `json:"sha256"`
				Data      string `json:"data"`
			}
			if err = parse(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			data, err := base64.StdEncoding.Strict().DecodeString(in.Data)
			if err != nil || len(data) < 1 || len(data) > dockerArchiveChunk {
				return core.ActionResult{}, failure("invalid_input", "Archive upload data must contain one base64 chunk of at most 32 KiB", 400)
			}
			chunkHash := sha256.Sum256(data)
			if hex.EncodeToString(chunkHash[:]) != in.SHA256 {
				return core.ActionResult{}, failure("checksum_mismatch", "Upload chunk SHA-256 does not match its base64 data", 412)
			}
			streamIn := dockerArchiveArtifactInput{ID: in.ID, Image: in.Image, ArchiveID: in.ArchiveID, Direction: "upload", Offset: in.Offset, Length: int64(len(data)), SHA256: in.SHA256}
			result, streamErr := m.dockerArchiveAgentStream(ctx, actor, streamIn, agent.IO{Stdin: io.NopCloser(bytes.NewReader(data))})
			if streamErr != nil {
				return core.ActionResult{}, streamErr
			}
			var output struct {
				ArchiveID string `json:"archive_id"`
				Next      int64  `json:"next_offset"`
				Complete  bool   `json:"complete"`
			}
			if json.Unmarshal(result.Data, &output) != nil || output.ArchiveID != in.ArchiveID || output.Next < in.Offset {
				return core.ActionResult{}, failure("agent_protocol_error", "Docker Agent returned an invalid upload receipt", 502)
			}
			return core.ActionResult{Data: map[string]any{"archive_id": output.ArchiveID, "next_offset": output.Next, "complete": output.Complete}, Resources: resources, AuditSummary: map[string]any{"image_resource": resources[1], "chunk_bytes": len(data)}}, nil
		})
	write.Action.Audit = true

	status := m.binding("docker_image_archive_upload_status", "Read the committed offset of one owned Docker archive upload", "containers:manage", true,
		obj(map[string]any{"id": idSchema(), "image": map[string]any{"type": "string", "pattern": dockerImage.String()}, "archive_id": map[string]any{"type": "string", "pattern": dockerArchiveID.String()}}, "id", "image", "archive_id"), dockerArchiveResources,
		func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			resources, err := archiveResourceInput(raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			actor, err = dockerArchiveActor(ctx, m, actor, "containers:manage", resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			var in struct {
				ID        string `json:"id"`
				Image     string `json:"image"`
				ArchiveID string `json:"archive_id"`
			}
			if err = parse(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			var output map[string]any
			err = m.dockerArchiveAgentJSON(ctx, actor, DockerArchiveAction{ID: in.ID, Image: in.Image, Command: "upload_status", ArchiveID: in.ArchiveID}, &output)
			return core.ActionResult{Data: output, Resources: resources}, err
		})
	status.Action.Audit = true

	importAction := m.binding("docker_image_archive_import", "Load a verified managed Docker archive into its authorized daemon", "containers:manage", false,
		obj(map[string]any{"id": idSchema(), "image": map[string]any{"type": "string", "pattern": dockerImage.String()}, "archive_id": map[string]any{"type": "string", "pattern": dockerArchiveID.String()}}, "id", "image", "archive_id"), dockerArchiveResources,
		func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			resources, err := archiveResourceInput(raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			actor, err = dockerArchiveActor(ctx, m, actor, "containers:manage", resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			var in struct {
				ID        string `json:"id"`
				Image     string `json:"image"`
				ArchiveID string `json:"archive_id"`
			}
			if err = parse(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			forward, _ := json.Marshal(DockerArchiveAction{ID: in.ID, Image: in.Image, Command: "import", ArchiveID: in.ArchiveID})
			job, err := m.deps.Tasks.Submit(ctx, actor, "docker_image_archive_action", forward, resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			taskID, _ := job["id"].(string)
			return core.ActionResult{Data: job, TaskID: taskID, Resources: resources, AuditSummary: map[string]any{"image_resource": resources[1], "archive_id": in.ArchiveID}}, nil
		})
	importAction.Action.Task, importAction.Action.Audit = true, true

	read := m.binding("docker_image_archive_read", "Read one bounded chunk from an owned Docker image archive", "containers:read", true,
		obj(map[string]any{"id": idSchema(), "image": map[string]any{"type": "string", "pattern": dockerImage.String()}, "archive_id": map[string]any{"type": "string", "pattern": dockerArchiveID.String()}, "offset": map[string]any{"type": "integer", "minimum": 0, "maximum": dockerArchiveMaxBytes}, "length": map[string]any{"type": "integer", "minimum": 1, "maximum": dockerArchiveChunk}}, "id", "image", "archive_id", "offset", "length"), dockerArchiveResources,
		func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
			resources, err := archiveResourceInput(raw)
			if err != nil {
				return core.ActionResult{}, err
			}
			actor, err = dockerArchiveActor(ctx, m, actor, "containers:read", resources)
			if err != nil {
				return core.ActionResult{}, err
			}
			var in dockerArchiveArtifactInput
			if err = parse(raw, &in); err != nil {
				return core.ActionResult{}, err
			}
			in.Direction = "download"
			buffer := &boundedDatabaseBuffer{maximum: dockerArchiveChunk}
			result, err := m.dockerArchiveAgentStream(ctx, actor, in, agent.IO{Stdout: buffer})
			if err != nil {
				return core.ActionResult{}, err
			}
			var details map[string]any
			if json.Unmarshal(result.Data, &details) != nil {
				return core.ActionResult{}, failure("agent_protocol_error", "Docker Agent returned invalid download metadata", 502)
			}
			details["encoding"] = "base64"
			details["data"] = base64.StdEncoding.EncodeToString(buffer.Bytes())
			return core.ActionResult{Data: details, Resources: resources, AuditSummary: map[string]any{"image_resource": resources[1], "chunk_bytes": buffer.Len()}}, nil
		})
	read.Action.Audit = true
	return []core.Binding{export, begin, write, status, importAction, read}
}

var _ agent.StreamExecutor = (*PrivilegedExecutor)(nil)
