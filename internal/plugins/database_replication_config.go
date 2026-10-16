package plugins

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

const databaseReplicationConfigMarker = "# PowerLinux managed database replication configuration v1"

func databaseReplicationServerIDs(pairID string) (uint32, uint32, error) {
	if !databaseReplicationPairPattern.MatchString(pairID) {
		return 0, 0, failure("invalid_input", "Invalid replication pair identity", 400)
	}
	decoded, err := hex.DecodeString(pairID)
	if err != nil || len(decoded) != sha256.Size/2 {
		return 0, 0, failure("invalid_input", "Invalid replication pair identity", 400)
	}
	digest := sha256.Sum256(decoded)
	// Reserve 0, avoid signed wraparound, and keep the two members adjacent.
	source := binary.BigEndian.Uint32(digest[:4]) & 0x7ffffffe
	source += 2
	if source >= 0x7ffffffe {
		source = 2
	}
	target := source + 1
	if source == target || source == 0 || target == 0 {
		return 0, 0, failure("identity_generation_failed", "Replication server identities could not be generated", 500)
	}
	return source, target, nil
}

func databaseReplicationConfigAfter(before []byte, pairID, engine, role, database string, serverID uint32) ([]byte, error) {
	if !databaseReplicationPairPattern.MatchString(pairID) || canonicalDatabaseReplicationEngine(engine) == "" ||
		(role != "source" && role != "target") || !validDatabaseName(database) || serverID == 0 || len(before) == 0 || len(before) > databaseReplicationMaxConfig {
		return nil, failure("invalid_input", "Invalid managed replication configuration descriptor", 400)
	}
	body := append([]byte(nil), before...)
	text := string(body)
	if strings.Contains(text, databaseReplicationConfigMarker) {
		return nil, failure("configuration_unmanaged", "A replication configuration marker already exists; inspect it before retrying", 412)
	}
	if role == "source" {
		_, managed, state := parseDatabaseBinlogConfig(body)
		if !managed || state != "settled" {
			return nil, failure("configuration_unmanaged", "Source replication requires an exact managed MySQL binary-log configuration", 412)
		}
		if strings.Contains(string(body), databaseBinlogConfigMarker) {
			index := strings.Index(string(body), databaseBinlogConfigMarker)
			if index < 0 {
				return nil, failure("configuration_unmanaged", "Managed source binary-log marker could not be located", 412)
			}
			body = append([]byte(nil), body[:index]...)
		}
		if hasReplicationConfigDirective(body, true) {
			return nil, failure("configuration_unmanaged", "Source contains an unmanaged server ID or replication filter; it will not be overwritten", 412)
		}
	} else if hasReplicationConfigDirective(body, false) {
		return nil, failure("configuration_unmanaged", "Target contains an unmanaged server ID or replication filter; it will not be overwritten", 412)
	}
	if len(body) > 0 && body[len(body)-1] != '\n' {
		body = append(body, '\n')
	}
	block := fmt.Sprintf("%s\n# pair=%s role=%s\n[mysqld]\nserver-id=%d\n", databaseReplicationConfigMarker, pairID, role, serverID)
	if role == "source" {
		block += "log-bin=mysql-bin\nbinlog_format=ROW\n"
	} else {
		// The target is a dedicated managed instance and must reject application
		// writes while it is a replica. MySQL's super_read_only also blocks
		// administrative SQL writes while allowing the replication SQL thread.
		block += "read_only=ON\n"
		if engine == "mysql" {
			block += "super_read_only=ON\n"
		} else {
			// MariaDB applies this exact wildcard to every replication channel.
			block += "replicate-wild-do-table=" + databaseReplicationMariaWildcard(database) + "\n"
		}
	}
	return append(body, []byte(block)...), nil
}

// The replication wildcard grammar treats underscore as one arbitrary
// character. Database names accept underscores, so they must be escaped to
// keep app_db isolated from appXdb. MySQL option files consume backslash
// escapes before the server filter parser; double it here so the server sees
// exactly one wildcard escape.
func databaseReplicationMariaWildcard(database string) string {
	pattern := databaseReplicationSQLWildcard(database)
	return strings.ReplaceAll(pattern, `\`, `\\`)
}

// MySQL receives this pattern in a string literal under the managed admin
// connection's NO_BACKSLASH_ESCAPES SQL mode, so each backslash is preserved.
func databaseReplicationSQLWildcard(database string) string {
	return strings.ReplaceAll(strings.ReplaceAll(database, `\`, `\\`), `_`, `\_`) + ".%"
}

func hasReplicationConfigDirective(body []byte, includeBinlog bool) bool {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(strings.SplitN(line, "=", 2)[0]))
		key = strings.TrimPrefix(key, "loose-")
		normalized := strings.ReplaceAll(key, "_", "-")
		if normalized == "server-id" || strings.HasPrefix(normalized, "replicate-") {
			return true
		}
		if includeBinlog {
			switch normalized {
			case "log-bin", "skip-log-bin", "disable-log-bin", "log-bin-index", "binlog-format":
				return true
			}
		} else if normalized == "read-only" || normalized == "super-read-only" {
			return true
		}
	}
	return false
}

func databaseReplicationConfigAtExpectedState(current, before, after []byte) (string, bool) {
	switch string(current) {
	case string(before):
		return "before", true
	case string(after):
		return "after", true
	default:
		return "unknown", false
	}
}
