package plugins

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

const phpHealthPath = "/powerlinux-health"
const phpHealthResponse = "Folio PHP-FPM"

func fastCGIRecord(kind byte, data []byte) []byte {
	record := make([]byte, 8+len(data))
	record[0], record[1], record[3] = 1, kind, 1
	binary.BigEndian.PutUint16(record[4:6], uint16(len(data)))
	copy(record[8:], data)
	return record
}
func fastCGIHealthParams() []byte {
	result := []byte{}
	for _, pair := range [][2]string{{"REQUEST_METHOD", "GET"}, {"SCRIPT_NAME", phpHealthPath}, {"SCRIPT_FILENAME", phpHealthPath}, {"REQUEST_URI", phpHealthPath}, {"SERVER_PROTOCOL", "HTTP/1.1"}, {"CONTENT_LENGTH", "0"}} {
		result = append(result, byte(len(pair[0])), byte(len(pair[1])))
		result = append(result, pair[0]...)
		result = append(result, pair[1]...)
	}
	return result
}

// An open socket alone is not an FPM health response. The protected default
// pool serves a fixed ping over FastCGI; no user script or filesystem path is
// requested. Both its body and successful END_REQUEST are required.
func probePHPFastCGI(ctx context.Context, socket string) error {
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { connection.Close() })
	defer stop()
	deadline := time.Now().Add(500 * time.Millisecond)
	if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
		deadline = earlier
	}
	if err := connection.SetDeadline(deadline); err != nil {
		return err
	}
	request := fastCGIRecord(1, []byte{0, 1, 0, 0, 0, 0, 0, 0})
	request = append(request, fastCGIRecord(4, fastCGIHealthParams())...)
	request = append(request, fastCGIRecord(4, nil)...)
	request = append(request, fastCGIRecord(5, nil)...)
	if _, err := io.Copy(connection, strings.NewReader(string(request))); err != nil {
		return err
	}
	output := []byte{}
	for records := 0; records < 128; records++ {
		var header [8]byte
		if _, err := io.ReadFull(connection, header[:]); err != nil {
			return err
		}
		if header[0] != 1 || binary.BigEndian.Uint16(header[2:4]) != 1 {
			return errors.New("invalid FPM health record")
		}
		length := int(binary.BigEndian.Uint16(header[4:6]))
		if length > 8192 || len(output)+length > 32768 {
			return errors.New("FPM health response exceeded bound")
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(connection, body); err != nil {
			return err
		}
		if _, err := io.CopyN(io.Discard, connection, int64(header[6])); err != nil {
			return err
		}
		switch header[1] {
		case 6:
			output = append(output, body...)
		case 7:
			if len(body) != 0 {
				return errors.New("FPM health returned diagnostic errors")
			}
		case 3:
			if len(body) != 8 || binary.BigEndian.Uint32(body[:4]) != 0 || body[4] != 0 {
				return errors.New("FPM health did not complete successfully")
			}
			_, text, ok := strings.Cut(string(output), "\r\n\r\n")
			if !ok || strings.TrimSpace(text) != phpHealthResponse {
				return errors.New("FPM health response did not match its owned pool")
			}
			return nil
		default:
			return errors.New("unexpected FPM health record")
		}
	}
	return errors.New("FPM health record count exceeded bound")
}
