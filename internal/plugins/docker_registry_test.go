package plugins

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestDockerRegistryAuthHeaderIsBoundToDigestImageRegistry(t *testing.T) {
	const password = "registry-password-canary"
	for _, test := range []struct {
		image  string
		server string
	}{
		{image: "ghcr.io/team/app@sha256:" + strings.Repeat("a", 64), server: "ghcr.io"},
		{image: "registry.example:5000/team/app@sha256:" + strings.Repeat("b", 64), server: "registry.example:5000"},
		{image: "ubuntu@sha256:" + strings.Repeat("c", 64), server: "https://index.docker.io/v1/"},
		{image: "library/ubuntu@sha256:" + strings.Repeat("d", 64), server: "https://index.docker.io/v1/"},
		{image: "docker.io/team/app@sha256:" + strings.Repeat("f", 64), server: "https://index.docker.io/v1/"},
		{image: "index.docker.io/team/app@sha256:" + strings.Repeat("0", 64), server: "https://index.docker.io/v1/"},
	} {
		header, err := dockerRegistryAuthHeader(DockerInput{Image: test.image, RegistryUsername: "private-user", RegistryPassword: password})
		if err != nil {
			t.Fatalf("could not prepare registry auth for %s: %v", test.image, err)
		}
		decoded, err := base64.URLEncoding.DecodeString(header)
		if err != nil {
			t.Fatalf("registry auth header is not padded base64url: %v", err)
		}
		var auth struct {
			Username      string `json:"username"`
			Password      string `json:"password"`
			ServerAddress string `json:"serveraddress"`
		}
		if err = json.Unmarshal(decoded, &auth); err != nil {
			t.Fatalf("registry auth header is not Docker AuthConfig JSON: %v", err)
		}
		if auth.Username != "private-user" || auth.Password != password || auth.ServerAddress != test.server {
			t.Fatalf("auth header was not bound to the image registry: %+v", auth)
		}
	}
	// Tildes force standard base64 to use '+', so this guards the URL-safe
	// alphabet instead of testing an input that happens to decode either way.
	header, err := dockerRegistryAuthHeader(DockerInput{
		Image:            "registry.example/team/app@sha256:" + strings.Repeat("e", 64),
		RegistryUsername: strings.Repeat("~", 20),
		RegistryPassword: strings.Repeat("~", 20),
	})
	if err != nil || strings.ContainsAny(header, "+/") {
		t.Fatal("Docker Engine auth header did not use the URL-safe base64 alphabet", err, header)
	}
	if header, err := dockerRegistryAuthHeader(DockerInput{Image: "ubuntu@sha256:" + strings.Repeat("a", 64)}); err != nil || header != "" {
		t.Fatal("public digest pull unexpectedly acquired a registry credential header", header, err)
	}
}

func TestDockerImagePullCredentialFieldsAreWriteOnlySecrets(t *testing.T) {
	binding := (&Manager{}).dockerImagePullBinding()
	if !binding.Action.Task {
		t.Fatal("digest image pull must retain shared task semantics")
	}
	properties, ok := binding.Action.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("image pull schema omitted its properties: %#v", binding.Action.InputSchema)
	}
	for _, name := range []string{"registry_username", "registry_password"} {
		field, ok := properties[name].(map[string]any)
		if !ok || field["format"] != "password" || field["writeOnly"] != true {
			t.Fatalf("registry field %q is not explicitly write-only and secret-formatted: %#v", name, properties[name])
		}
	}
}

func TestDockerRegistryAuthRejectsIncompleteOrMalformedCredentials(t *testing.T) {
	image := "registry.example:65536/team/app@sha256:" + strings.Repeat("a", 64)
	if _, err := dockerRegistryAuthHeader(DockerInput{Image: image, RegistryUsername: "user", RegistryPassword: "pass"}); err == nil {
		t.Fatal("out-of-range registry port was accepted")
	}
	if _, err := dockerRegistryAuthHeader(DockerInput{Image: "registry.example/team/app@sha256:" + strings.Repeat("a", 64), RegistryUsername: "user"}); err == nil {
		t.Fatal("username without password was accepted")
	}
	for _, invalid := range []string{
		"registry.example:abc/team/app@sha256:" + strings.Repeat("a", 64),
		"registry.example:0/team/app@sha256:" + strings.Repeat("a", 64),
	} {
		if err := validateDockerImageReference(invalid); err == nil {
			t.Fatalf("malformed registry reference passed digest and port validation: %s", invalid)
		}
	}
}
