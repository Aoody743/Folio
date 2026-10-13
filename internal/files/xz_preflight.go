package files

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

// The pinned xz library treats ReaderConfig.DictCap as a minimum allocation.
// Inspect every indexed block before decoding so an untrusted dictionary size
// cannot cause a multi-gigabyte allocation. Container CRCs are checked here and
// the decoder subsequently verifies compressed data, checksums and the index.
func (s *Service) preflightXZ(ctx context.Context, file io.ReaderAt, size int64) error {
	read := func(offset int64, data []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if offset < 0 || int64(len(data)) > size-offset {
			return failure("invalid_archive", "Invalid XZ container offsets", 400)
		}
		_, err := file.ReadAt(data, offset)
		return err
	}
	end := size
	streams, blocks := 0, 0
	padding := int64(0)
	for end > 0 {
		if end < 24 {
			return failure("invalid_archive", "Truncated XZ stream", 400)
		}
		footer := make([]byte, 12)
		if err := read(end-12, footer); err != nil {
			return err
		}
		if !bytes.Equal(footer[10:], []byte("YZ")) {
			if bytes.Equal(footer[8:], []byte{0, 0, 0, 0}) && padding < 64<<10 {
				end -= 4
				padding += 4
				continue
			}
			return failure("invalid_archive", "Invalid XZ stream footer", 400)
		}
		streams++
		if streams > 32 {
			return failure("too_large", "XZ contains too many concatenated streams", 413)
		}
		if binary.LittleEndian.Uint32(footer[:4]) != crc32.ChecksumIEEE(footer[4:10]) {
			return failure("invalid_archive", "Invalid XZ footer checksum", 400)
		}
		indexSize := (int64(binary.LittleEndian.Uint32(footer[4:8])) + 1) * 4
		if indexSize < 8 || indexSize > 4<<20 || indexSize > end-24 {
			return failure("too_large", "XZ index exceeds bounded metadata budget", 413)
		}
		index := make([]byte, indexSize)
		if err := read(end-12-indexSize, index); err != nil {
			return err
		}
		if index[0] != 0 || binary.LittleEndian.Uint32(index[len(index)-4:]) != crc32.ChecksumIEEE(index[:len(index)-4]) {
			return failure("invalid_archive", "Invalid XZ index checksum", 400)
		}
		position := 1
		count, err := xzVLI(index[:len(index)-4], &position)
		if err != nil {
			return err
		}
		if count > 65536 || uint64(blocks)+count > 65536 {
			return failure("too_large", "XZ block count exceeds metadata budget", 413)
		}
		blocks += int(count)
		sizes := make([]int64, 0, int(count))
		var space, expanded uint64
		for i := uint64(0); i < count; i++ {
			unpadded, err := xzVLI(index[:len(index)-4], &position)
			if err != nil {
				return err
			}
			uncompressed, err := xzVLI(index[:len(index)-4], &position)
			if err != nil {
				return err
			}
			if unpadded < 8 || unpadded > uint64(size) {
				return failure("invalid_archive", "Invalid XZ indexed block size", 400)
			}
			aligned := (unpadded + 3) &^ uint64(3)
			expansionLimit := uint64(s.options.MaxTransferBytes) + uint64(s.options.MaxArchiveEntries)*1024 + 1024
			if aligned > uint64(size)-space || uncompressed > expansionLimit-expanded {
				return failure("too_large", "XZ indexed data exceeds transfer budget", 413)
			}
			space += aligned
			expanded += uncompressed
			sizes = append(sizes, int64(aligned))
		}
		for _, value := range index[position : len(index)-4] {
			if value != 0 {
				return failure("invalid_archive", "Invalid XZ index padding", 400)
			}
		}
		start := end - 12 - indexSize - int64(space) - 12
		header := make([]byte, 12)
		if err := read(start, header); err != nil {
			return err
		}
		if !bytes.Equal(header[:6], []byte{0xfd, '7', 'z', 'X', 'Z', 0}) || !bytes.Equal(header[6:8], footer[8:10]) || binary.LittleEndian.Uint32(header[8:12]) != crc32.ChecksumIEEE(header[6:8]) {
			return failure("invalid_archive", "Invalid XZ stream header", 400)
		}
		offset := start + 12
		for _, blockSize := range sizes {
			first := []byte{0}
			if err := read(offset, first); err != nil {
				return err
			}
			headerSize := (int(first[0]) + 1) * 4
			if first[0] == 0 || int64(headerSize) > blockSize {
				return failure("invalid_archive", "Invalid XZ block header", 400)
			}
			block := make([]byte, headerSize)
			if err := read(offset, block); err != nil {
				return err
			}
			if binary.LittleEndian.Uint32(block[len(block)-4:]) != crc32.ChecksumIEEE(block[:len(block)-4]) {
				return failure("invalid_archive", "Invalid XZ block-header checksum", 400)
			}
			if err := checkXZDictionary(block[:len(block)-4]); err != nil {
				return err
			}
			offset += blockSize
		}
		end = start
	}
	return nil
}

func xzVLI(data []byte, position *int) (uint64, error) {
	var value uint64
	for i := 0; i < 9; i++ {
		if *position >= len(data) {
			return 0, io.ErrUnexpectedEOF
		}
		current := data[*position]
		*position++
		value |= uint64(current&0x7f) << uint(7*i)
		if current&0x80 == 0 {
			if i > 0 && current == 0 {
				return 0, errors.New("invalid XZ variable-length integer")
			}
			return value, nil
		}
	}
	return 0, errors.New("XZ variable-length integer exceeds 63 bits")
}

func checkXZDictionary(header []byte) error {
	if len(header) < 4 || header[1]&0x3f != 0 {
		return failure("unsupported_format", "Only the bounded LZMA2 XZ filter is supported", 422)
	}
	position := 2
	for _, flag := range []byte{0x40, 0x80} {
		if header[1]&flag != 0 {
			if _, err := xzVLI(header, &position); err != nil {
				return err
			}
		}
	}
	filter, err := xzVLI(header, &position)
	if err != nil {
		return err
	}
	length, err := xzVLI(header, &position)
	if err != nil {
		return err
	}
	if filter != 0x21 || length != 1 || position >= len(header) {
		return failure("unsupported_format", "Unsupported XZ filter properties", 422)
	}
	property := header[position]
	if property > 40 {
		return failure("invalid_archive", "Invalid XZ dictionary property", 400)
	}
	capacity := uint64(1<<32 - 1)
	if property < 40 {
		capacity = (2 | uint64(property&1)) << uint(property/2+11)
	}
	if capacity > 8<<20 {
		return failure("too_large", "XZ dictionary exceeds eight MiB memory budget", 413)
	}
	return nil
}
