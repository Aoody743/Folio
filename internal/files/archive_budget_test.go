package files

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootTarPayloadCannotBypassExtractionBudget(t *testing.T) {
	service, root := fixture(t)
	service.options.MaxTransferBytes = 32 << 10
	var archive bytes.Buffer
	compressed := gzip.NewWriter(&archive)
	writer := tar.NewWriter(compressed)
	payload := strings.Repeat("unbudgeted compressed root entry", 10000)
	if err := writer.WriteHeader(&tar.Header{Name: ".", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	writer.Write([]byte(payload))
	writer.Close()
	compressed.Close()
	if err := os.WriteFile(filepath.Join(root, "root-payload.tar.gz"), archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Extract(context.Background(), Input{Root: "workspace", Path: "root-payload.tar.gz", DestinationRoot: "workspace", DestinationPath: "root-out", Format: "tar.gz"}); !IsCode(err, "unsafe_archive") {
		t.Fatal("a root regular entry bypassed the decompression budget", err)
	}
	if _, err := os.Stat(filepath.Join(root, "root-out")); !os.IsNotExist(err) {
		t.Fatal("invalid root entry published a destination", err)
	}
}

func TestPrivateCleanupRemovesZeroPermissionDirectories(t *testing.T) {
	service, _ := fixture(t)
	name := "staging/zero-permissions"
	if err := service.state.MkdirAll(name+"/nested", 0700); err != nil {
		t.Fatal(err)
	}
	file, err := service.state.Create(name + "/nested/value")
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("private artifact")
	file.Close()
	if err := service.state.Chmod(name+"/nested", 0000); err != nil {
		t.Fatal(err)
	}
	if err := service.state.Chmod(name, 0000); err != nil {
		t.Fatal(err)
	}
	if err := removePrivateTree(service.state, name); err != nil {
		service.state.Chmod(name, 0700)
		service.state.Chmod(name+"/nested", 0700)
		t.Fatal(err)
	}
	if _, err := service.state.Lstat(name); !os.IsNotExist(err) {
		t.Fatal("private zero-permission artifact was retained", err)
	}
}
