package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigNoneListenerSecurity(t *testing.T) {
	const childKey = "POWERLINUX_CONFIG_NONE_SECURITY_CASE"
	if item := os.Getenv(childKey); item != "" {
		dir := t.TempDir()
		args := []string{"serve", "--server-config", "", "--data-dir", filepath.Join(dir, "data"), "--listen", "0.0.0.0:0", "--allow-root"}
		want := "plaintext HTTP must bind a loopback"
		if item == "invalid-tls" {
			certificate, key := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
			for _, path := range []string{certificate, key} {
				if err := os.WriteFile(path, []byte("invalid PEM fixture"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args = append(args, "--tls-cert", certificate, "--tls-key", key)
			want = "PEM"
		}
		if err := run(args); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("disabled server config bypassed listener/TLS security: %v", err)
		}
		return
	}
	for _, item := range []string{"remote-plaintext", "invalid-tls"} {
		t.Run(item, func(t *testing.T) {
			// A broken guard must fail within a bound, rather than leave a
			// successfully started server blocking the whole test process.
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestConfigNoneListenerSecurity$", "-test.timeout=10s")
			command.Env = append(os.Environ(), childKey+"="+item)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("config-none security subprocess failed: %v\n%s", err, output)
			}
		})
	}
}
