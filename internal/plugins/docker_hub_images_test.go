//go:build linux

package plugins

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

func hubDigest(repo string) string { return repo + "@sha256:" + strings.Repeat("a", 64) }

func TestDockerHubOwnershipProofUsesOnlyOfficialEquivalence(t *testing.T) {
	for _, repos := range [][2]string{{"docker.io/library/alpine", "alpine"}, {"docker.io/alpine", "library/alpine"}, {"index.docker.io/library/alpine", "alpine"}, {"index.docker.io/team/app", "team/app"}, {"docker.io/team/app", "team/app"}, {"library/alpine", "alpine"}, {"docker.io/library/nested/app", "library/nested/app"}} {
		if !dockerImageDigestProofMatches(hubDigest(repos[0]), hubDigest(repos[1])) || !dockerImageDigestProofMatches(hubDigest(repos[1]), hubDigest(repos[0])) {
			t.Fatal("official Hub equivalent rejected", repos)
		}
	}
	for _, repos := range [][2]string{{"registry.example/library/alpine", "alpine"}, {"registry.example/team/app", "team/app"}, {"docker.io:443/library/alpine", "alpine"}, {"index.docker.io:443/library/alpine", "alpine"}, {"registry-1.docker.io/library/alpine", "alpine"}, {"localhost/library/alpine", "alpine"}, {"docker.io/team/app", "other/app"}, {"docker.io/library/nested/app", "nested/app"}, {"docker.io/library/al..pine", "al..pine"}, {"docker.io/library/Alpine", "Alpine"}} {
		if dockerImageDigestProofMatches(hubDigest(repos[0]), hubDigest(repos[1])) {
			t.Fatal("non-equivalent repository accepted", repos)
		}
	}
	if dockerImageDigestProofMatches(hubDigest("docker.io/library/alpine"), "alpine@sha256:"+strings.Repeat("b", 64)) {
		t.Fatal("different digest accepted")
	}
}

func dockerHubEngineFixture(t *testing.T, instance Instance, supplied string) *atomic.Int32 {
	t.Helper()
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(instance.Paths.RunDir, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1.45/images/create":
			_, _ = w.Write([]byte(`{"status":"Digest verified"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1.45/images/json":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"Id": "sha256:" + strings.Repeat("b", 64), "RepoDigests": []string{supplied}, "RepoTags": []string{"alpine:latest"}, "Size": 42}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/json"):
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:" + strings.Repeat("b", 64), "RepoDigests": []string{supplied}, "Size": 42})
		case r.Method == http.MethodDelete:
			if r.URL.Query().Get("force") != "false" || r.URL.Query().Get("noprune") != "true" {
				t.Error("unsafe image deletion options")
			}
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return calls
}

func TestDockerHubPullInspectRemoveAcceptEngineFamiliarRepoDigestOverUnixHTTP(t *testing.T) {
	for _, command := range []string{"image_pull", "image_inspect", "image_remove"} {
		t.Run(command, func(t *testing.T) {
			e, instance := dockerImageFixtureAgent(t)
			dockerHubEngineFixture(t, instance, hubDigest("alpine"))
			requested := hubDigest("docker.io/library/alpine")
			raw, _ := json.Marshal(DockerInput{ID: instance.ID, Command: command, Image: requested})
			reply, err := e.Run(context.Background(), agent.Request{Operation: "docker_action", Input: raw})
			if err != nil || reply.ExitCode != 0 {
				t.Fatal("Engine familiar digest rejected", err)
			}
			var output map[string]any
			if json.Unmarshal([]byte(reply.Stdout), &output) != nil || output["image"] != requested {
				t.Fatal("result lost caller's exact ref")
			}
		})
	}
}

func TestDockerHubProofDoesNotMergeAliasACLAndListPreservesEngineRef(t *testing.T) {
	s, a, _ := registryTestService(t)
	e, instance := dockerImageFixtureAgent(t)
	engineRef := hubDigest("alpine")
	requested := hubDigest("docker.io/library/alpine")
	calls := dockerHubEngineFixture(t, instance, engineRef)
	m, err := New(s.Runtime("plugins"), Options{Executor: e})
	if err != nil {
		t.Fatal(err)
	}
	bindings := append(m.dockerImageBindings(), m.dockerImagesListBinding())
	if err = s.RegisterProvider(core.Bindings(bindings)); err != nil {
		t.Fatal(err)
	}
	canonicalOnly := dockerImageToken(t, s, a, "hub-canonical-only", []string{"containers:read", "containers:manage"}, []string{InstanceResource(instance.ID), DockerImageResource(instance.ID, requested)})
	familiarOnly := dockerImageToken(t, s, a, "hub-familiar-only", []string{"containers:read", "containers:manage"}, []string{InstanceResource(instance.ID), DockerImageResource(instance.ID, engineRef)})
	for _, action := range []string{"docker_image_inspect", "docker_image_remove"} {
		for _, in := range []struct {
			actor core.Actor
			ref   string
		}{{canonicalOnly, engineRef}, {familiarOnly, requested}} {
			raw, _ := json.Marshal(map[string]any{"id": instance.ID, "image": in.ref})
			if _, err = s.Execute(context.Background(), in.actor, action, raw); core.ErrorCode(err) != "forbidden" {
				t.Fatal("Hub alias grant crossed exact resource boundary", action, err)
			}
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized alias action reached Engine")
	}
	raw, _ := json.Marshal(map[string]any{"id": instance.ID, "image": requested})
	if _, err = s.Execute(context.Background(), canonicalOnly, "docker_image_inspect", raw); err != nil {
		t.Fatal("exact authorized canonical ref could not use familiar Engine proof", err)
	}
	listRaw, _ := json.Marshal(IDInput{ID: instance.ID})
	out, err := s.Execute(context.Background(), familiarOnly, "docker_images_list", listRaw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(out)
	var view struct {
		Items []dockerImageView `json:"items"`
	}
	_ = json.Unmarshal(encoded, &view)
	if len(view.Items) != 1 || view.Items[0].Image != engineRef {
		t.Fatal("list rewrote Engine's exact RepoDigest")
	}
	out, err = s.Execute(context.Background(), canonicalOnly, "docker_images_list", listRaw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(out)
	_ = json.Unmarshal(encoded, &view)
	if len(view.Items) != 0 {
		t.Fatal("list merged canonical-only grant with familiar Engine ref")
	}
}

func TestDockerPrivateImageCannotUseHubOwnershipProofOverUnixHTTP(t *testing.T) {
	e, instance := dockerImageFixtureAgent(t)
	dockerHubEngineFixture(t, instance, hubDigest("alpine"))
	raw, _ := json.Marshal(DockerInput{ID: instance.ID, Command: "image_inspect", Image: hubDigest("registry.example/library/alpine")})
	if _, err := e.Run(context.Background(), agent.Request{Operation: "docker_action", Input: raw}); core.ErrorCode(err) != "ownership_invalid" {
		t.Fatal("private registry accepted Hub digest proof", err)
	}
}
