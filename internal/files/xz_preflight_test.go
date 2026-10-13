package files

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXZDictionaryBombRejectedBeforeDecodeAndGzipStreamsAboveTextBudget(t *testing.T) {
	service, root := fixture(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(root, "note"), []byte("xz-valid-fixture"), 0600)
	if _, err := service.Archive(ctx, Input{Root: "workspace", Path: "note", DestinationRoot: "workspace", DestinationPath: "note.tar.xz", Format: "tar.xz"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "note.tar.xz"))
	if err != nil {
		t.Fatal(err)
	}
	// Mutate a real container's dictionary property, retaining a valid header
	// CRC. The compressed payload never needs to be decoded to reject it.
	headerLength := (int(data[12]) + 1) * 4
	header := data[12 : 12+headerLength]
	position := 2
	for _, flag := range []byte{0x40, 0x80} {
		if header[1]&flag != 0 {
			if _, err := xzVLI(header, &position); err != nil {
				t.Fatal(err)
			}
		}
	}
	filter, err := xzVLI(header, &position)
	if err != nil || filter != 0x21 {
		t.Fatal("fixture filter not LZMA2", err)
	}
	length, err := xzVLI(header, &position)
	if err != nil || length != 1 {
		t.Fatal("fixture properties unexpected", err)
	}
	header[position] = 40 // Four GiB dictionary declaration.
	binary.LittleEndian.PutUint32(header[headerLength-4:], crc32.ChecksumIEEE(header[:headerLength-4]))
	os.WriteFile(filepath.Join(root, "bomb.tar.xz"), data, 0600)
	if _, err := service.Extract(ctx, Input{Root: "workspace", Path: "bomb.tar.xz", DestinationRoot: "workspace", DestinationPath: "bomb-out", Format: "tar.xz"}); !IsCode(err, "too_large") {
		t.Fatal("XZ dictionary bomb not rejected before decode", err)
	}
	if _, err := os.Stat(filepath.Join(root, "bomb-out")); !os.IsNotExist(err) {
		t.Fatal("dictionary bomb published output")
	}
	file, err := os.Create(filepath.Join(root, "large.gz"))
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	writer.Name = "large.txt"
	content := strings.Repeat("gz-streaming-content", 180000)
	writer.Write([]byte(content))
	writer.Close()
	file.Close()
	if _, err := service.Extract(ctx, Input{Root: "workspace", Path: "large.gz", DestinationRoot: "workspace", DestinationPath: "gzip-out", Format: "gz"}); err != nil {
		t.Fatal("gzip was limited to editor size", err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "gzip-out", "large.txt"))
	if err != nil || string(actual) != content {
		t.Fatal("large gzip stream changed bytes", err)
	}
}
