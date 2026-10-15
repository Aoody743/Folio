package plugins

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/core"
)

func dockerArchiveTestFixture(t *testing.T, tags []string, extraFiles map[string][]byte) ([]byte, string, string) {
	t.Helper()
	config := []byte(`{"architecture":"amd64","config":{},"rootfs":{"type":"layers","diff_ids":[]}}`)
	configHash := sha256.Sum256(config)
	imageID := "sha256:" + hex.EncodeToString(configHash[:])
	image := "alpine@sha256:" + strings.Repeat("a", 64)
	layerID := strings.Repeat("b", 64)
	manifest, err := json.Marshal([]dockerSaveManifestEntry{{
		Config:   strings.TrimPrefix(imageID, "sha256:") + ".json",
		RepoTags: tags,
		Layers:   []string{layerID + "/layer.tar"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	repositories, err := json.Marshal(map[string]map[string]string{"alpine": {"latest": strings.TrimPrefix(imageID, "sha256:")}})
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{
		"manifest.json": manifest,
		"repositories":  repositories,
		strings.TrimPrefix(imageID, "sha256:") + ".json": config,
		layerID + "/VERSION":                             []byte("1.0"),
		layerID + "/json":                                []byte(`{"id":"` + layerID + `"}`),
		layerID + "/layer.tar":                           {},
	}
	for name, data := range extraFiles {
		entries[name] = data
	}
	var out bytes.Buffer
	writer := tar.NewWriter(&out)
	for _, name := range []string{"manifest.json", "repositories", strings.TrimPrefix(imageID, "sha256:") + ".json", layerID + "/VERSION", layerID + "/json", layerID + "/layer.tar"} {
		if data, ok := entries[name]; ok {
			if err = writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err = writer.Write(data); err != nil {
				t.Fatal(err)
			}
			delete(entries, name)
		}
	}
	for name, data := range entries {
		if err = writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), image, imageID
}

func dockerArchiveTestFile(t *testing.T, data []byte) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "docker-archive-*.tar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(data); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestDockerSaveArchivePreflightBindsOneImageAndRepository(t *testing.T) {
	tag := "docker.io/library/alpine:latest"
	data, image, imageID := dockerArchiveTestFixture(t, []string{tag}, nil)
	if err := validateDockerSaveArchiveFile(context.Background(), dockerArchiveTestFile(t, data), image, imageID); err != nil {
		t.Fatalf("valid single-image export rejected: %v", err)
	}

	badTag, _, _ := dockerArchiveTestFixture(t, []string{"attacker.example/other:latest"}, nil)
	if err := validateDockerSaveArchiveFile(context.Background(), dockerArchiveTestFile(t, badTag), image, imageID); err == nil {
		t.Fatal("archive with an unrelated repository tag was accepted")
	}

	traversal, _, _ := dockerArchiveTestFixture(t, []string{tag}, map[string][]byte{"../outside": []byte("x")})
	if err := validateDockerSaveArchiveFile(context.Background(), dockerArchiveTestFile(t, traversal), image, imageID); err == nil {
		t.Fatal("archive with a traversal path was accepted")
	}

	if err := validateDockerSaveArchiveFile(context.Background(), dockerArchiveTestFile(t, data), image, "sha256:"+strings.Repeat("c", 64)); err == nil {
		t.Fatal("archive config was accepted for a different Engine image ID")
	}
}

func TestDockerArchiveLoadStreamRemovesMutableTags(t *testing.T) {
	data, image, imageID := dockerArchiveTestFixture(t, []string{"docker.io/library/alpine:latest"}, nil)
	var sanitized bytes.Buffer
	if err := streamDockerArchiveForLoad(context.Background(), bytes.NewReader(data), &sanitized); err != nil {
		t.Fatalf("sanitize image archive: %v", err)
	}

	reader := tar.NewReader(bytes.NewReader(sanitized.Bytes()))
	foundManifest, foundRepositories := false, false
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch header.Name {
		case "manifest.json":
			foundManifest = true
			var manifest []dockerSaveManifestEntry
			if err = json.NewDecoder(reader).Decode(&manifest); err != nil {
				t.Fatal(err)
			}
			if len(manifest) != 1 || len(manifest[0].RepoTags) != 0 {
				t.Fatalf("mutable RepoTags remain in sanitized manifest: %#v", manifest)
			}
		case "repositories":
			foundRepositories = true
		}
	}
	if !foundManifest || foundRepositories {
		t.Fatalf("sanitized archive metadata: manifest=%t repositories=%t", foundManifest, foundRepositories)
	}
	if err := validateDockerSaveArchiveFile(context.Background(), dockerArchiveTestFile(t, sanitized.Bytes()), image, imageID); err != nil {
		t.Fatalf("sanitized archive no longer proves the same image: %v", err)
	}
}

func TestDockerArchiveProofIsOpaqueAndAuthenticated(t *testing.T) {
	block, err := aes.NewCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	proofCipher, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	executor := &PrivilegedExecutor{privateCipher: proofCipher}
	proof := dockerArchiveProof{Version: 1, SourceInstanceID: "docker-source", Image: "alpine@sha256:" + strings.Repeat("a", 64), ImageID: "sha256:" + strings.Repeat("b", 64), Bytes: 123, SHA256: strings.Repeat("c", 64), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	token, err := executor.sealDockerArchiveProof(proof)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(token, proof.Image) || len(token) > 4096 {
		t.Fatalf("proof token is not opaque or exceeds its bound: %d bytes", len(token))
	}
	opened, err := executor.openDockerArchiveProof(token)
	if err != nil || opened != proof {
		t.Fatalf("proof did not round trip: %#v, %v", opened, err)
	}
	tamperedBytes := []byte(token)
	tamperedBytes[len(tamperedBytes)/2] ^= 1
	tampered := string(tamperedBytes)
	if _, err = executor.openDockerArchiveProof(tampered); err == nil {
		t.Fatal("tampered proof was accepted")
	}
}

func TestDockerArchiveUploadFinalChunkRetryIsIdempotentAndActorPinned(t *testing.T) {
	data, image, imageID := dockerArchiveTestFixture(t, []string{"docker.io/library/alpine:latest"}, nil)
	state := t.TempDir()
	rootDir := filepath.Join(t.TempDir(), "software")
	if err := os.MkdirAll(filepath.Join(state, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	proofCipher, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	executor := &PrivilegedExecutor{config: PrivilegedConfig{RootDir: rootDir, StateDir: state}, privateCipher: proofCipher}
	instance := Instance{ID: "docker-archive-test", PluginID: "docker", InstanceID: "archive-test"}
	instance.Paths = executor.instancePaths(instance.PluginID, instance.InstanceID)
	if err = os.MkdirAll(instance.Paths.BackupDir, 0700); err != nil {
		t.Fatal(err)
	}
	archiveID := "dockerarchive_" + strings.Repeat("d", 32)
	archiveFile, err := os.OpenFile(filepath.Join(instance.Paths.BackupDir, dockerArchiveFilename(archiveID)), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = archiveFile.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	chunkHash := hex.EncodeToString(digest[:])
	record := dockerImageArchive{ID: archiveID, InstanceID: instance.ID, SourceInstanceID: "docker-source", Image: image, ImageID: imageID, Bytes: int64(len(data)), SHA256: chunkHash, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), State: "uploading", ActorID: "actor-1"}
	input := dockerArchiveArtifactInput{ID: instance.ID, Image: image, ArchiveID: archiveID, Offset: 0, Length: int64(len(data)), SHA256: chunkHash}
	if _, err = executor.writeDockerImageArchiveChunk(context.Background(), instance, record, input, "actor-1", io.NopCloser(bytes.NewReader(data))); err != nil {
		t.Fatalf("commit final archive chunk: %v", err)
	}
	if err = executor.load("private", archiveID, &record); err != nil {
		t.Fatal(err)
	}
	if record.State != "ready" || record.ActorID != "actor-1" {
		t.Fatalf("completed upload lost its state or actor pin: %#v", record)
	}
	result, err := executor.writeDockerImageArchiveChunk(context.Background(), instance, record, input, "actor-1", io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatalf("retry of acknowledged final chunk failed: %v", err)
	}
	var retry struct {
		Next     int64 `json:"next_offset"`
		Complete bool  `json:"complete"`
	}
	if json.Unmarshal(result.Data, &retry) != nil || retry.Next != int64(len(data)) || !retry.Complete {
		t.Fatalf("retry did not return committed completion: %s", result.Data)
	}
	if _, err = executor.writeDockerImageArchiveChunk(context.Background(), instance, record, input, "actor-2", io.NopCloser(bytes.NewReader(data))); core.ErrorCode(err) != "forbidden" {
		t.Fatalf("another actor retried an owned upload: %v", err)
	}
}

func dockerArchiveTestAlias(t *testing.T, executor *PrivilegedExecutor, instance Instance) (dockerImageArchive, string) {
	t.Helper()
	image := "registry.example/team/app@sha256:" + strings.Repeat("a", 64)
	imageID := "sha256:" + strings.Repeat("b", 64)
	proof := dockerArchiveProof{Version: 1, SourceInstanceID: "docker-source", Image: image, ImageID: imageID, Bytes: 1, SHA256: strings.Repeat("c", 64), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	token, err := executor.sealDockerArchiveProof(proof)
	if err != nil {
		t.Fatal(err)
	}
	record := dockerImageArchive{ID: "dockerarchive_" + strings.Repeat("e", 32), InstanceID: instance.ID, SourceInstanceID: proof.SourceInstanceID, Image: image, ImageID: imageID, Bytes: proof.Bytes, SHA256: proof.SHA256, CreatedAt: proof.CreatedAt, State: "loaded", Provenance: "managed_archive", Proof: token}
	if err = executor.save("private", record.ID, record); err != nil {
		t.Fatal(err)
	}
	if err = executor.saveDockerArchiveImageAlias(record); err != nil {
		t.Fatal(err)
	}
	return record, image
}

func dockerArchiveAliasEngineFixture(t *testing.T, instance Instance, tags, digests []string) {
	t.Helper()
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.EscapedPath()
		if r.Method != http.MethodGet || !strings.HasSuffix(path, "/json") {
			http.NotFound(w, r)
			return
		}
		if strings.Contains(path, "registry.example") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(dockerImageInspection{ID: "sha256:" + strings.Repeat("b", 64), RepoTags: tags, RepoDigests: digests, Size: 1234})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
}

func TestDockerArchiveAliasRequiresCurrentIDOnlyEngineImage(t *testing.T) {
	for _, test := range []struct {
		name    string
		tags    []string
		digests []string
		want    bool
	}{
		{name: "id-only image resolves", want: true},
		{name: "mutable tag blocks proof alias", tags: []string{"other:latest"}},
		{name: "malformed empty tag blocks proof alias", tags: []string{""}},
		{name: "repo digest blocks proof alias", digests: []string{"registry.example/team/app@sha256:" + strings.Repeat("d", 64)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, instance := dockerImageFixtureAgent(t)
			_, image := dockerArchiveTestAlias(t, executor, instance)
			dockerArchiveAliasEngineFixture(t, instance, test.tags, test.digests)
			resolved, restored, err := executor.resolveDockerImageArchive(context.Background(), instance, image)
			if test.want {
				if err != nil || !restored || resolved.ID != "sha256:"+strings.Repeat("b", 64) {
					t.Fatalf("proof-bound ID-only image was not resolved: %#v, restored=%t, err=%v", resolved, restored, err)
				}
				return
			}
			if err == nil || restored {
				t.Fatalf("archive proof authorized an image with Engine aliases: restored=%t, err=%v", restored, err)
			}
		})
	}
}

func TestDockerArchiveAliasListIsSeparateAndImageScoped(t *testing.T) {
	executor, instance := dockerImageFixtureAgent(t)
	record, image := dockerArchiveTestAlias(t, executor, instance)
	views, err := executor.dockerArchiveImageAliases(instance, []dockerImageInspection{{ID: record.ImageID, Size: 1234}})
	if err != nil || len(views) != 1 || views[0].Image != image || !views[0].RestoredFromArchive {
		t.Fatalf("restored alias missing from typed image list: %#v, %v", views, err)
	}
	for _, items := range [][]dockerImageInspection{
		{{ID: record.ImageID, RepoTags: []string{"other:latest"}}},
		{{ID: record.ImageID, RepoDigests: []string{"registry.example/team/app@sha256:" + strings.Repeat("d", 64)}}},
		{{ID: "sha256:" + strings.Repeat("f", 64)}},
	} {
		blocked, listErr := executor.dockerArchiveImageAliases(instance, items)
		if listErr != nil || len(blocked) != 0 {
			t.Fatalf("archive alias appeared without its exact ID-only Engine image: %#v, %v", blocked, listErr)
		}
	}
	if !dockerArchiveAliasID.MatchString(dockerArchiveImageAliasID(instance.ID, image)) || !dockerArchiveIndexID.MatchString(dockerArchiveImageAliasIndexID(instance.ID)) {
		t.Fatal(fmt.Sprintf("invalid protected alias/index identifiers for %s", image))
	}
}
