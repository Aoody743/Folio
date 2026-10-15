package plugins

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDatabaseReplicationNativeCLI is an opt-in local QA fixture. It creates
// one random schema on each explicitly selected loopback instance, dumps the
// source through the production argv builder, imports the target through the
// production argv builder, verifies rows, and removes only those random
// schemas. It is not run by ordinary unit tests and must not be pointed at
// production instances.
func TestDatabaseReplicationNativeCLI(t *testing.T) {
	if os.Getenv("POWERLINUX_REPLICATION_NATIVE_CONFIRM_QA") != "I_CONFIRM_LOCAL_QA_ONLY" {
		t.Skip("opt-in native MySQL/MariaDB QA fixture is disabled")
	}
	engine := os.Getenv("POWERLINUX_REPLICATION_NATIVE_ENGINE")
	if engine != "mysql" && engine != "mariadb" {
		t.Fatal("native fixture engine must be mysql or mariadb")
	}
	sourceFile := os.Getenv("POWERLINUX_REPLICATION_NATIVE_SOURCE_DEFAULTS_FILE")
	targetFile := os.Getenv("POWERLINUX_REPLICATION_NATIVE_TARGET_DEFAULTS_FILE")
	sourcePort := nativeReplicationPort(t, "POWERLINUX_REPLICATION_NATIVE_SOURCE_PORT")
	targetPort := nativeReplicationPort(t, "POWERLINUX_REPLICATION_NATIVE_TARGET_PORT")
	if sourcePort == targetPort {
		t.Fatal("native fixture requires two distinct local database ports")
	}
	validateNativeReplicationDefaultsFile(t, sourceFile)
	validateNativeReplicationDefaultsFile(t, targetFile)
	dumpProgram := os.Getenv("POWERLINUX_REPLICATION_NATIVE_DUMP_PROGRAM")
	importProgram := os.Getenv("POWERLINUX_REPLICATION_NATIVE_IMPORT_PROGRAM")
	if dumpProgram == "" {
		if engine == "mysql" {
			dumpProgram = "mysqldump"
		} else {
			dumpProgram = "mariadb-dump"
		}
	}
	if importProgram == "" {
		if engine == "mysql" {
			importProgram = "mysql"
		} else {
			importProgram = "mariadb"
		}
	}
	var err error
	if dumpProgram, err = exec.LookPath(dumpProgram); err != nil {
		t.Fatalf("native dump client is unavailable: %v", err)
	}
	if importProgram, err = exec.LookPath(importProgram); err != nil {
		t.Fatalf("native import client is unavailable: %v", err)
	}
	if !filepath.IsAbs(dumpProgram) || !filepath.IsAbs(importProgram) {
		t.Fatal("native clients must resolve to absolute executable paths")
	}

	var random [8]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal("generate private QA schema name", err)
	}
	database := "plrqa_" + hex.EncodeToString(random[:])
	admin := func(ctx context.Context, defaults string, port int, query string) ([]byte, error) {
		args := []string{"--defaults-file=" + defaults, "--protocol=tcp", "--host=127.0.0.1", "--port=" + strconv.Itoa(port), "--batch", "--skip-column-names", "--execute=" + query}
		return runNativeReplicationCommand(ctx, importProgram, args, nil)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cleanupCancel()
	defer func() {
		if _, cleanupErr := admin(cleanupCtx, sourceFile, sourcePort, "DROP DATABASE IF EXISTS `"+database+"`"); cleanupErr != nil {
			t.Errorf("could not remove only the generated source QA schema: %v", cleanupErr)
		}
		if _, cleanupErr := admin(cleanupCtx, targetFile, targetPort, "DROP DATABASE IF EXISTS `"+database+"`"); cleanupErr != nil {
			t.Errorf("could not remove only the generated target QA schema: %v", cleanupErr)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	sourceSQL := "CREATE DATABASE `" + database + "`; CREATE TABLE `" + database + "`.items (id BIGINT PRIMARY KEY, value VARCHAR(32) NOT NULL) ENGINE=InnoDB; INSERT INTO `" + database + "`.items VALUES (1,'qa-a'),(2,'qa-b')"
	if _, err = admin(ctx, sourceFile, sourcePort, sourceSQL); err != nil {
		t.Fatalf("could not create isolated source QA schema: %v", err)
	}
	if _, err = admin(ctx, targetFile, targetPort, "CREATE DATABASE `"+database+"`"); err != nil {
		t.Fatalf("could not create isolated empty target QA schema: %v", err)
	}

	dumpArgs, err := databaseReplicationDumpArgs(sourceFile, database)
	if err != nil {
		t.Fatal("build production dump argv", err)
	}
	dumpArgs = addNativeReplicationLoopback(dumpArgs, sourcePort)
	dump, err := runNativeReplicationCommand(ctx, dumpProgram, dumpArgs, nil)
	if err != nil {
		t.Fatalf("native dump flags failed on %s; this fixture validates client compatibility, including the selected coordinate option: %v", engine, err)
	}
	if len(dump) == 0 || len(dump) > 8<<20 {
		t.Fatalf("native fixture dump exceeds its 8 MiB bound: %d", len(dump))
	}
	if _, _, err = parseDatabaseReplicationCoordinate(dump); err != nil {
		t.Fatalf("native dump did not contain a supported frozen coordinate: %v", err)
	}
	if !databaseReplicationDumpComplete(dump) {
		t.Fatal("native dump omitted its complete-output marker")
	}
	importArgs, err := databaseReplicationImportArgs(targetFile, database)
	if err != nil {
		t.Fatal("build production import argv", err)
	}
	importArgs = addNativeReplicationLoopback(importArgs, targetPort)
	if got := importArgs[len(importArgs)-1]; got != database {
		t.Fatalf("import database operand changed after adding local QA connection flags: %q", got)
	}
	if _, err = runNativeReplicationCommand(ctx, importProgram, importArgs, bytes.NewReader(dump)); err != nil {
		t.Fatalf("native seed import failed for the exact target database: %v", err)
	}

	query := "SELECT CONCAT(id, CHAR(9), value) FROM `" + database + "`.items ORDER BY id"
	rows, err := admin(ctx, targetFile, targetPort, query)
	if err != nil || string(rows) != "1\tqa-a\n2\tqa-b\n" {
		t.Fatalf("native imported target rows differ from the owned source fixture: rows=%q err=%v", rows, err)
	}
}

func nativeReplicationPort(t *testing.T, key string) int {
	t.Helper()
	value := os.Getenv(key)
	port, err := strconv.Atoi(value)
	if err != nil || port < 1024 || port > 65535 {
		t.Fatalf("%s must be an explicit loopback TCP port", key)
	}
	return port
}

func validateNativeReplicationDefaultsFile(t *testing.T, path string) {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Fatalf("native defaults file must be an absolute QA credential-file path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("native defaults file must be a protected regular file: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("native defaults file is not readable by this QA process: %v", err)
	}
	_ = file.Close()
}

func addNativeReplicationLoopback(args []string, port int) []string {
	connection := []string{"--protocol=tcp", "--host=127.0.0.1", "--port=" + strconv.Itoa(port)}
	if len(args) == 0 {
		return connection
	}
	result := make([]string, 0, len(args)+len(connection))
	result = append(result, args[0])
	result = append(result, connection...)
	result = append(result, args[1:]...)
	return result
}

func runNativeReplicationCommand(ctx context.Context, program string, args []string, input *bytes.Reader) ([]byte, error) {
	command := exec.CommandContext(ctx, program, args...)
	stdout := &limitedReplicationBuffer{max: 8 << 20}
	stderr := &limitedReplicationBuffer{max: 64 << 10}
	command.Stdout = stdout
	command.Stderr = stderr
	if input != nil {
		command.Stdin = input
	}
	err := command.Run()
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, message)
	}
	return stdout.Bytes(), nil
}

type limitedReplicationBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitedReplicationBuffer) Write(data []byte) (int, error) {
	if len(data) > b.max-b.Len() {
		return 0, errors.New("native replication fixture output limit exceeded")
	}
	return b.Buffer.Write(data)
}
