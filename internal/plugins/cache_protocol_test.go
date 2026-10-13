package plugins

import (
	"strings"
	"testing"
)

func TestRedisResponseBoundsBeforeDeclaredAllocation(t *testing.T) {
	for _, payload := range []string{"$1073741824\r\n", "*1073741824\r\n", strings.Repeat("*1\r\n", 33), "$-9\r\n"} {
		conn := &boundedRedisConn{}
		denied := false
		for _, part := range []string{payload[:2], payload[2:]} {
			if err := conn.inspect([]byte(part)); err != nil {
				denied = true
				break
			}
		}
		if !denied {
			t.Fatalf("hostile RESP length was accepted")
		}
	}
}
func TestRedisGuardHandlesNestedAndBinaryPayloadAcrossReads(t *testing.T) {
	conn := &boundedRedisConn{}
	payload := "*2\r\n$4\r\n$9\r\n\r\n*2\r\n:2\r\n$-1\r\n+OK\r\n"
	for _, character := range []byte(payload) {
		if err := conn.inspect([]byte{character}); err != nil {
			t.Fatal("valid bounded RESP rejected", err)
		}
	}
	if len(conn.stack) != 0 || conn.remaining != 0 || len(conn.line) != 0 {
		t.Fatal("protocol parser lost frame alignment")
	}
}
