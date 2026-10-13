package files

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
)

func (s *Service) freezeArchive(ctx context.Context, input io.Reader, suffix string) (*os.File, int64, func(), error) {
	name := filepath.Join("staging", newID()+suffix)
	file, err := s.state.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, 0, nil, err
	}
	cleanup := func() { file.Close(); s.state.Remove(name) }
	n, err := io.CopyBuffer(file, io.LimitReader(contextReader{ctx, input}, s.options.MaxTransferBytes+1), make([]byte, 32<<10))
	if err == nil && n > s.options.MaxTransferBytes {
		err = failure("too_large", "Compressed archive exceeds transfer budget", 413)
	}
	if err != nil {
		cleanup()
		return nil, 0, nil, err
	}
	return file, n, cleanup, nil
}

// archive/zip allocates the complete directory during NewReader, before the
// caller can inspect its entry count. Bound both its declared preallocation
// and its actual records/variable metadata before invoking that constructor.
// The reader must contain private frozen bytes so these checks cannot race
// a public file's owner changing its end record or central directory.
func (s *Service) preflightZIP(ctx context.Context, file io.ReaderAt, size int64) error {
	read := func(offset int64, data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if offset < 0 || int64(len(data)) > size-offset {
			return failure("invalid_archive", "Invalid ZIP directory offsets", 400)
		}
		_, err := file.ReadAt(data, offset)
		return err
	}
	if size < 22 {
		return failure("invalid_archive", "Truncated ZIP end record", 400)
	}
	tail := make([]byte, min(size, 65*1024))
	if err := read(size-int64(len(tail)), tail); err != nil {
		return err
	}
	position := -1
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:i+4]) == 0x06054b50 {
			if i+22+int(binary.LittleEndian.Uint16(tail[i+20:i+22])) > len(tail) {
				return failure("invalid_archive", "Truncated ZIP comment", 400)
			}
			position = i
			break
		}
	}
	if position < 0 {
		return failure("invalid_archive", "Missing ZIP end record", 400)
	}
	end := tail[position : position+22]
	if binary.LittleEndian.Uint16(end[4:6]) != 0 || binary.LittleEndian.Uint16(end[6:8]) != 0 || binary.LittleEndian.Uint16(end[8:10]) != binary.LittleEndian.Uint16(end[10:12]) {
		return failure("unsupported_format", "Multi-disk ZIP archives are unsupported", 422)
	}
	count := uint64(binary.LittleEndian.Uint16(end[10:12]))
	directorySize := uint64(binary.LittleEndian.Uint32(end[12:16]))
	directoryOffset := uint64(binary.LittleEndian.Uint32(end[16:20]))
	endOffset := size - int64(len(tail)) + int64(position)
	if count == 0xffff || directorySize == 0xffffffff || directoryOffset == 0xffffffff {
		locator := make([]byte, 20)
		if endOffset >= 20 {
			if err := read(endOffset-20, locator); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(locator[:4]) == 0x07064b50 {
				if binary.LittleEndian.Uint32(locator[4:8]) != 0 || binary.LittleEndian.Uint32(locator[16:20]) != 1 {
					return failure("unsupported_format", "Multi-disk ZIP64 archives are unsupported", 422)
				}
				offset := binary.LittleEndian.Uint64(locator[8:16])
				if offset > uint64(endOffset-20) {
					return failure("invalid_archive", "Invalid ZIP64 end offset", 400)
				}
				record := make([]byte, 56)
				if err := read(int64(offset), record); err != nil {
					return err
				}
				if binary.LittleEndian.Uint32(record[:4]) != 0x06064b50 || binary.LittleEndian.Uint64(record[4:12]) < 44 || binary.LittleEndian.Uint32(record[16:20]) != 0 || binary.LittleEndian.Uint32(record[20:24]) != 0 || binary.LittleEndian.Uint64(record[24:32]) != binary.LittleEndian.Uint64(record[32:40]) {
					return failure("invalid_archive", "Invalid ZIP64 directory end", 400)
				}
				count = binary.LittleEndian.Uint64(record[32:40])
				directorySize = binary.LittleEndian.Uint64(record[40:48])
				directoryOffset = binary.LittleEndian.Uint64(record[48:56])
				endOffset = int64(offset)
			}
		}
	}
	if count > uint64(s.options.MaxArchiveEntries) || directorySize > 8<<20 {
		return failure("too_large", "ZIP central directory exceeds entry or eight MiB metadata budget", 413)
	}
	if directorySize > uint64(endOffset) || directoryOffset > uint64(size) {
		return failure("invalid_archive", "Invalid ZIP central directory extent", 400)
	}
	start := endOffset - int64(directorySize)
	// Match archive/zip's compatibility fallback for prefixed ZIPs whose
	// recorded offset already refers to the physical central directory.
	if size >= 46 && start > int64(directoryOffset) && directoryOffset <= uint64(size-46) {
		header := make([]byte, 46)
		if err := read(int64(directoryOffset), header); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(header[:4]) == 0x02014b50 {
			start = int64(directoryOffset)
		}
	}
	var records, metadata uint64
	for offset := start; ; {
		signature := make([]byte, 4)
		if err := read(offset, signature); err != nil {
			return err
		}
		if binary.LittleEndian.Uint32(signature) != 0x02014b50 {
			break
		}
		header := make([]byte, 46)
		if err := read(offset, header); err != nil {
			return err
		}
		length := uint64(46) + uint64(binary.LittleEndian.Uint16(header[28:30])) + uint64(binary.LittleEndian.Uint16(header[30:32])) + uint64(binary.LittleEndian.Uint16(header[32:34]))
		records++
		if records > uint64(s.options.MaxArchiveEntries) || length > (8<<20)-metadata {
			return failure("too_large", "ZIP actual directory exceeds entry or eight MiB metadata budget", 413)
		}
		if length > uint64(size-offset) {
			return failure("invalid_archive", "Truncated ZIP central directory", 400)
		}
		metadata += length
		offset += int64(length)
	}
	if uint16(records) != uint16(count) {
		return failure("invalid_archive", "ZIP directory count mismatch", 400)
	}
	return nil
}
