package files

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func zipFixture(t *testing.T, count int) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for i := 0; i < count; i++ {
		file, err := writer.Create(fmt.Sprintf("entry-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("actual archive bytes")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return append([]byte(nil), data.Bytes()...)
}

func TestZIPDirectoryAllocationIsBoundedBeforeParsing(t *testing.T) {
	service, root := fixture(t)
	service.options.MaxArchiveEntries = 10
	ctx := context.Background()
	for name, data := range map[string][]byte{"declared": zipFixture(t, 1), "actual": zipFixture(t, 11)} {
		t.Run(name, func(t *testing.T) {
			end := data[len(data)-22:]
			count := uint16(0xffff)
			if name == "actual" {
				// Lie about the count; actual scanning must still stop before
				// archive/zip allocates every directory record.
				count = 1
			}
			binary.LittleEndian.PutUint16(end[8:10], count)
			binary.LittleEndian.PutUint16(end[10:12], count)
			if err := os.WriteFile(filepath.Join(root, name+".zip"), data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Extract(ctx, Input{Root: "workspace", Path: name + ".zip", DestinationRoot: "workspace", DestinationPath: name + "-out", Format: "zip"}); !IsCode(err, "too_large") {
				t.Fatal("ZIP directory budget did not reject before standard parsing", err)
			}
			if _, err := os.Stat(filepath.Join(root, name+"-out")); !os.IsNotExist(err) {
				t.Fatal("oversized directory published output", err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "valid.zip"), zipFixture(t, 2), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Extract(ctx, Input{Root: "workspace", Path: "valid.zip", DestinationRoot: "workspace", DestinationPath: "valid-out", Format: "zip"}); err != nil {
		t.Fatal("valid bounded ZIP was rejected", err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "valid-out", "entry-1"))
	if err != nil || string(actual) != "actual archive bytes" {
		t.Fatal("valid bounded ZIP lost data", err)
	}
}

func TestZIP64AndPrefixedZIPPreflightCompatibility(t *testing.T) {
	service, _ := fixture(t)
	ctx := context.Background()
	original := zipFixture(t, 2)
	end := append([]byte(nil), original[len(original)-22:]...)
	endOffset := uint64(len(original) - 22)
	var record [56]byte
	binary.LittleEndian.PutUint32(record[:4], 0x06064b50)
	binary.LittleEndian.PutUint64(record[4:12], 44)
	binary.LittleEndian.PutUint16(record[12:14], 45)
	binary.LittleEndian.PutUint16(record[14:16], 45)
	binary.LittleEndian.PutUint64(record[24:32], 2)
	binary.LittleEndian.PutUint64(record[32:40], 2)
	binary.LittleEndian.PutUint64(record[40:48], uint64(binary.LittleEndian.Uint32(end[12:16])))
	binary.LittleEndian.PutUint64(record[48:56], uint64(binary.LittleEndian.Uint32(end[16:20])))
	var locator [20]byte
	binary.LittleEndian.PutUint32(locator[:4], 0x07064b50)
	binary.LittleEndian.PutUint64(locator[8:16], endOffset)
	binary.LittleEndian.PutUint32(locator[16:20], 1)
	binary.LittleEndian.PutUint16(end[8:10], 0xffff)
	binary.LittleEndian.PutUint16(end[10:12], 0xffff)
	binary.LittleEndian.PutUint32(end[12:16], 0xffffffff)
	binary.LittleEndian.PutUint32(end[16:20], 0xffffffff)
	zip64 := append([]byte(nil), original[:len(original)-22]...)
	zip64 = append(zip64, record[:]...)
	zip64 = append(zip64, locator[:]...)
	zip64 = append(zip64, end...)
	for name, data := range map[string][]byte{"zip64": zip64, "prefixed": append([]byte("nonexecuted archive prefix"), original...)} {
		t.Run(name, func(t *testing.T) {
			if err := service.preflightZIP(ctx, bytes.NewReader(data), int64(len(data))); err != nil {
				t.Fatal("valid ZIP compatibility preflight failed", err)
			}
			reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
			if err != nil || len(reader.File) != 2 {
				t.Fatal("ZIP fixture is not valid for standard reader", err)
			}
		})
	}
	empty := append([]byte("empty prefix"), zipFixture(t, 0)...)
	if err := service.preflightZIP(ctx, bytes.NewReader(empty), int64(len(empty))); err != nil {
		t.Fatal("small prefixed empty ZIP was rejected", err)
	}
}
