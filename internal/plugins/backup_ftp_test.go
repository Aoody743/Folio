package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type bufferSink struct{ bytes.Buffer }

func (*bufferSink) Close() error { return nil }
func installIntegration(t *testing.T, e *PrivilegedExecutor, label string) Instance {
	t.Helper()
	plan, err := e.plan(context.Background(), PlanInput{Operation: "install", PluginID: "webssh", InstanceID: label})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := e.apply(context.Background(), ApplyInput{PlanID: plan.ID, IdempotencyKey: "fixture-install-" + label})
	if err != nil {
		t.Fatal(err)
	}
	return instance
}
func TestBackupArtifactStreamsFullContentAtomicallyWithoutLogging(t *testing.T) {
	executor := testAgent(t)
	instance := installIntegration(t, executor, "backup")
	data := bytes.Repeat([]byte("PRIVATE-BACKUP-CONTENT\n"), 120000)
	input, _ := json.Marshal(map[string]any{"id": instance.ID, "filename": "backup.sql", "direction": "download", "expected_bytes": len(data)})
	result, err := executor.Stream(context.Background(), agent.Request{Operation: "backup_artifact", Input: input}, agent.IO{Stdin: io.NopCloser(bytes.NewReader(data))})
	if err != nil || result.ExitCode != 0 {
		t.Fatal("artifact write failed", result, err)
	}
	if result.Stdout != "" || result.Stderr != "" {
		t.Fatal("backup content entered task logs", result)
	}
	actual, err := os.ReadFile(filepath.Join(instance.Paths.BackupDir, "backup.sql"))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("artifact stream truncated content", err)
	}
	input, _ = json.Marshal(map[string]any{"id": instance.ID, "filename": "backup.sql", "direction": "upload"})
	sink := &bufferSink{}
	result, err = executor.Stream(context.Background(), agent.Request{Operation: "backup_artifact", Input: input}, agent.IO{Stdout: sink})
	if err != nil || !bytes.Equal(sink.Bytes(), data) || result.StdoutBytes != int64(len(data)) || result.Stdout != "" {
		t.Fatal("artifact read stream incorrect", result, err)
	}
	input, _ = json.Marshal(map[string]any{"id": instance.ID, "filename": "incomplete.sql", "direction": "download", "expected_bytes": len(data) + 1})
	if _, err = executor.Stream(context.Background(), agent.Request{Operation: "backup_artifact", Input: input}, agent.IO{Stdin: io.NopCloser(bytes.NewReader(data))}); err == nil {
		t.Fatal("short incoming artifact was committed")
	}
	if _, err = os.Stat(filepath.Join(instance.Paths.BackupDir, "incomplete.sql")); !os.IsNotExist(err) {
		t.Fatal("failed artifact became visible", err)
	}
	entries, _ := os.ReadDir(instance.Paths.BackupDir)
	if len(entries) != 1 {
		t.Fatal("partial temporary backup leaked", entries)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err = os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(instance.Paths.BackupDir, "escape.sql")); err != nil {
		t.Fatal(err)
	}
	input, _ = json.Marshal(map[string]any{"id": instance.ID, "filename": "escape.sql", "direction": "upload"})
	if _, err = executor.Stream(context.Background(), agent.Request{Operation: "backup_artifact", Input: input}, agent.IO{Stdout: &bufferSink{}}); err == nil {
		t.Fatal("owned artifact reader followed a symlink")
	}
}

func TestGenericBackupStreamCannotExportDatabaseScopedArtifacts(t *testing.T) {
	executor := testAgent(t)
	for _, plugin := range []string{"mysql", "mysql-community", "mariadb", "postgresql", "mongodb"} {
		id := plugin + "-scope"
		instance := Instance{ID: id, PluginID: plugin, InstanceID: "scope", Paths: executor.instancePaths(plugin, "scope"), Revision: 1, Ownership: "managed", Service: ServiceProfile{ID: id, Unit: "powerlinux-plugin-" + id + ".service"}}
		if err := executor.save("receipts", id, instance); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(instance.Paths.BackupDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(instance.Paths.BackupDir, "anything.dump"), []byte("DB-PRIVATE-CONTENT"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, direction := range []string{"upload", "download"} {
			input, _ := json.Marshal(map[string]any{"id": id, "filename": "anything.dump", "direction": direction})
			sink := &bufferSink{}
			result, err := executor.Stream(context.Background(), agent.Request{Operation: "backup_artifact", Input: input}, agent.IO{Stdout: sink, Stdin: io.NopCloser(strings.NewReader("UNTRUSTED"))})
			if core.ErrorCode(err) != "database_backup_scope_required" || result.ExitCode == 0 || sink.Len() != 0 {
				t.Fatal("Generic software resource bypassed database export/import authorization", plugin, direction, result, err)
			}
		}
	}
}

func TestBackupArtifactCancellationReleasesWaiterBeforeOwnedResourceUnlock(t *testing.T) {
	executor := testAgent(t)
	instance := installIntegration(t, executor, "waiting")
	release := executor.lock(instance.ID)
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	sink := &bufferSink{}
	input, _ := json.Marshal(map[string]any{"id": instance.ID, "filename": "blocked.dump", "direction": "upload"})
	go func() {
		_, err := executor.Stream(ctx, agent.Request{Operation: "backup_artifact", Input: input}, agent.IO{Stdout: sink})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("Canceled artifact waiter returned another outcome", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Canceled artifact waiter held a shared task slot until unrelated resource finished")
	}
	if sink.Len() != 0 {
		t.Fatal("Canceled resource waiter emitted backup bytes")
	}
}
func TestBackupStreamCancellationReleasesBlockedInputAndKeepsNoArtifact(t *testing.T) {
	executor := testAgent(t)
	instance := installIntegration(t, executor, "cancel")
	reader, writer := io.Pipe()
	defer writer.Close()
	input, _ := json.Marshal(map[string]any{"id": instance.ID, "filename": "cancel.sql", "direction": "download", "expected_bytes": 100})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := executor.Stream(ctx, agent.Request{Operation: "backup_artifact", Input: input}, agent.IO{Stdin: reader})
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled transfer succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("artifact cancellation did not release blocked reader")
	}
	if _, err := os.Stat(filepath.Join(instance.Paths.BackupDir, "cancel.sql")); !os.IsNotExist(err) {
		t.Fatal("canceled artifact was committed")
	}
}

// A protocol fixture exercises the real FTP driver and shared durable job
// engine. This is not evidence that an external provider or PureFTP passed.
type ftpFixture struct {
	listener net.Listener
	mu       sync.Mutex
	files    map[string][]byte
}

func newFTPFixture(t *testing.T) *ftpFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &ftpFixture{listener: listener, files: map[string][]byte{}}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go fixture.serve(connection)
		}
	}()
	return fixture
}
func (f *ftpFixture) serve(connection net.Conn) {
	defer connection.Close()
	fmt.Fprint(connection, "220 FTP protocol fixture\r\n")
	scanner := bufio.NewScanner(connection)
	var data net.Listener
	defer func() {
		if data != nil {
			data.Close()
		}
	}()
	for scanner.Scan() {
		command, argument, _ := strings.Cut(scanner.Text(), " ")
		switch command {
		case "USER":
			fmt.Fprint(connection, "331 Password required\r\n")
		case "PASS":
			if argument != "private-fixture-password" {
				fmt.Fprint(connection, "530 Denied\r\n")
			} else {
				fmt.Fprint(connection, "230 Logged in\r\n")
			}
		case "FEAT":
			fmt.Fprint(connection, "211 End\r\n")
		case "TYPE", "OPTS":
			fmt.Fprint(connection, "200 Okay\r\n")
		case "CWD":
			fmt.Fprint(connection, "250 Directory selected\r\n")
		case "EPSV":
			var err error
			data, err = net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				return
			}
			port := data.Addr().(*net.TCPAddr).Port
			fmt.Fprintf(connection, "229 Entering Extended Passive Mode (|||%d|)\r\n", port)
		case "SIZE":
			f.mu.Lock()
			body, exists := f.files[argument]
			f.mu.Unlock()
			if !exists {
				fmt.Fprint(connection, "550 Missing\r\n")
			} else {
				fmt.Fprintf(connection, "213 %d\r\n", len(body))
			}
		case "STOR", "RETR":
			if data == nil {
				return
			}
			fmt.Fprint(connection, "150 Data follows\r\n")
			peer, err := data.Accept()
			if err != nil {
				return
			}
			if command == "STOR" {
				body, err := io.ReadAll(io.LimitReader(peer, 16<<20))
				if err != nil {
					peer.Close()
					return
				}
				f.mu.Lock()
				f.files[argument] = body
				f.mu.Unlock()
			} else {
				f.mu.Lock()
				body := append([]byte(nil), f.files[argument]...)
				f.mu.Unlock()
				_, _ = peer.Write(body)
			}
			peer.Close()
			data.Close()
			data = nil
			fmt.Fprint(connection, "226 Transfer complete\r\n")
		case "QUIT":
			fmt.Fprint(connection, "221 Bye\r\n")
			return
		default:
			fmt.Fprint(connection, "200 Okay\r\n")
		}
	}
}
func TestFTPTransferUsesSharedJobsSecretsAndActualByteStream(t *testing.T) {
	executor := testAgent(t)
	instance := installIntegration(t, executor, "ftp")
	body := bytes.Repeat([]byte("DATABASE-EXPORT\n"), 160000)
	if err := os.WriteFile(filepath.Join(instance.Paths.BackupDir, "source.sql"), body, 0600); err != nil {
		t.Fatal(err)
	}
	fixture := newFTPFixture(t)
	host, rawPort, _ := net.SplitHostPort(fixture.listener.Addr().String())
	port, _ := strconv.Atoi(rawPort)
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	secret, _ := os.ReadFile(service.SetupSecretFile())
	actor, err := service.Setup(context.Background(), strings.TrimSpace(string(secret)), "administrator", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(service.Runtime("plugins"), Options{Executor: executor})
	if err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterProvider(manager); err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterExecutor(NativeOperations(), manager); err != nil {
		t.Fatal(err)
	}
	registration, _ := json.Marshal(map[string]any{"id": "fixture", "host": host, "port": port, "username": "fixture", "password": "private-fixture-password", "tls_mode": "loopback", "directory": "/", "expected_revision": 0})
	if _, err = service.Execute(context.Background(), actor, "backup_ftp_destination_register", registration); err != nil {
		t.Fatal(err)
	}
	transfer, _ := json.Marshal(FTPTransferInput{ID: "fixture", SoftwareID: instance.ID, Filename: "source.sql", Direction: "upload"})
	submitted, err := service.Execute(context.Background(), actor, "backup_ftp_transfer", transfer)
	if err != nil {
		t.Fatal(err)
	}
	task := submitted.(map[string]any)
	id := task["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, err := service.Execute(context.Background(), actor, "tasks_get", json.RawMessage(fmt.Sprintf(`{"id":%q}`, id)))
		if err != nil {
			t.Fatal(err)
		}
		job := status.(map[string]any)
		if job["state"] == "succeeded" {
			break
		}
		if job["state"] == "failed" {
			t.Fatal("FTP job failed", job)
		}
		if time.Now().After(deadline) {
			t.Fatal("FTP transfer did not finish", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
	fixture.mu.Lock()
	uploaded := append([]byte(nil), fixture.files["/source.sql"]...)
	fixture.mu.Unlock()
	if !bytes.Equal(body, uploaded) {
		t.Fatal("shared FTP job truncated real content")
	}
	listed, err := service.Execute(context.Background(), actor, "backup_ftp_destinations_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(listed)
	if bytes.Contains(raw, []byte("private-fixture-password")) || bytes.Contains(raw, []byte(`"credential_ref":"secret_`)) {
		t.Fatal("FTP public listing revealed credentials")
	}
}
func TestFTPPlaintextRemoteTransportIsRejected(t *testing.T) {
	if _, err := ftpDial(context.Background(), FTPDestination{Host: "192.0.2.1", Port: 21, TLSMode: "loopback"}, "secret"); core.ErrorCode(err) != "tls_required" {
		t.Fatal("remote plaintext FTP allowed", err)
	}
}

func TestFTPProtocolCredentialsRejectControlCharactersBeforeConnecting(t *testing.T) {
	profile := FTPDestination{Host: "127.0.0.1", Port: 21, TLSMode: "loopback", Username: "safe-user"}
	for _, test := range []struct {
		name     string
		profile  FTPDestination
		password string
	}{
		{name: "username carriage return", profile: FTPDestination{Host: profile.Host, Port: profile.Port, TLSMode: profile.TLSMode, Username: "user\rNOOP"}, password: "safe-password"},
		{name: "username newline", profile: FTPDestination{Host: profile.Host, Port: profile.Port, TLSMode: profile.TLSMode, Username: "user\nPASS x"}, password: "safe-password"},
		{name: "password newline", profile: profile, password: "safe\nNOOP"},
		{name: "password NUL", profile: profile, password: "safe\x00password"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ftpDialLimited(context.Background(), test.profile, test.password, 1024); core.ErrorCode(err) != "invalid_credentials" {
				t.Fatalf("FTP command injection control character was not rejected before dialing: %v", err)
			}
		})
	}
}
