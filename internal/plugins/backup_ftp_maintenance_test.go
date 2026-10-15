package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

type ftpMaintenanceFixtureEntry struct {
	kind   string
	body   []byte
	target string
	mtime  time.Time
}

type ftpMaintenanceFixture struct {
	listener    net.Listener
	mu          sync.Mutex
	entries     map[string]ftpMaintenanceFixtureEntry
	storStarted chan struct{}
	storOnce    sync.Once
}

func newFTPMaintenanceFixture(t *testing.T) *ftpMaintenanceFixture {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture := &ftpMaintenanceFixture{listener: listener, storStarted: make(chan struct{}), entries: map[string]ftpMaintenanceFixtureEntry{"/": {kind: "directory", mtime: time.Date(2025, 3, 4, 5, 6, 0, 0, time.UTC)}, "/backup": {kind: "directory", mtime: time.Date(2025, 3, 4, 5, 6, 0, 0, time.UTC)}}}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go fixture.serve(connection)
		}
	}()
	return fixture
}

func (f *ftpMaintenanceFixture) addDir(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[name] = ftpMaintenanceFixtureEntry{kind: "directory", mtime: time.Date(2025, 3, 4, 5, 6, 0, 0, time.UTC)}
}

func (f *ftpMaintenanceFixture) addFile(name string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[name] = ftpMaintenanceFixtureEntry{kind: "file", body: append([]byte(nil), body...), mtime: time.Date(2025, 3, 4, 5, 6, 0, 0, time.UTC)}
}

func (f *ftpMaintenanceFixture) addLink(name, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[name] = ftpMaintenanceFixtureEntry{kind: "link", target: target, mtime: time.Date(2025, 3, 4, 5, 6, 0, 0, time.UTC)}
}

func (f *ftpMaintenanceFixture) exists(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.entries[path.Clean(name)]
	return ok
}

func (f *ftpMaintenanceFixture) serve(connection net.Conn) {
	defer connection.Close()
	_, _ = fmt.Fprint(connection, "220 FTP maintenance fixture\r\n")
	scanner := bufio.NewScanner(connection)
	cwd := "/"
	var data net.Listener
	defer func() {
		if data != nil {
			_ = data.Close()
		}
	}()
	for scanner.Scan() {
		command, argument, _ := strings.Cut(scanner.Text(), " ")
		switch command {
		case "USER":
			_, _ = fmt.Fprint(connection, "331 Password required\r\n")
		case "PASS":
			if argument != "maintenance-fixture-password" {
				_, _ = fmt.Fprint(connection, "530 Denied\r\n")
			} else {
				_, _ = fmt.Fprint(connection, "230 Logged in\r\n")
			}
		case "FEAT":
			_, _ = fmt.Fprint(connection, "211 End\r\n")
		case "TYPE", "OPTS":
			_, _ = fmt.Fprint(connection, "200 Okay\r\n")
		case "PWD":
			_, _ = fmt.Fprintf(connection, "257 %q\r\n", cwd)
		case "CWD":
			candidate := ftpMaintenanceFixtureResolve(cwd, argument)
			f.mu.Lock()
			entry, exists := f.entries[candidate]
			f.mu.Unlock()
			if !exists || entry.kind != "directory" {
				_, _ = fmt.Fprint(connection, "550 Not a real directory\r\n")
			} else {
				cwd = candidate
				_, _ = fmt.Fprint(connection, "250 Directory selected\r\n")
			}
		case "CDUP":
			cwd = path.Dir(cwd)
			_, _ = fmt.Fprint(connection, "250 Parent selected\r\n")
		case "EPSV":
			if data != nil {
				_ = data.Close()
			}
			var listenErr error
			data, listenErr = net.Listen("tcp", "127.0.0.1:0")
			if listenErr != nil {
				return
			}
			port := data.Addr().(*net.TCPAddr).Port
			_, _ = fmt.Fprintf(connection, "229 Entering Extended Passive Mode (|||%d|)\r\n", port)
		case "LIST":
			listing := f.listing(cwd)
			if data == nil {
				_, _ = fmt.Fprint(connection, "425 Data connection unavailable\r\n")
				continue
			}
			_, _ = fmt.Fprint(connection, "150 Listing follows\r\n")
			peer, acceptErr := data.Accept()
			if acceptErr != nil {
				return
			}
			_, _ = io.WriteString(peer, listing)
			_ = peer.Close()
			_ = data.Close()
			data = nil
			_, _ = fmt.Fprint(connection, "226 Listing complete\r\n")
		case "STOR":
			if data == nil {
				_, _ = fmt.Fprint(connection, "425 Data connection unavailable\r\n")
				continue
			}
			f.storOnce.Do(func() { close(f.storStarted) })
			_, _ = fmt.Fprint(connection, "150 Upload follows\r\n")
			peer, acceptErr := data.Accept()
			if acceptErr != nil {
				return
			}
			body, readErr := io.ReadAll(io.LimitReader(peer, 16<<20))
			_ = peer.Close()
			_ = data.Close()
			data = nil
			if readErr != nil {
				return
			}
			candidate := ftpMaintenanceFixtureResolve(cwd, argument)
			f.mu.Lock()
			f.entries[candidate] = ftpMaintenanceFixtureEntry{kind: "file", body: body, mtime: time.Date(2025, 3, 4, 5, 6, 0, 0, time.UTC)}
			f.mu.Unlock()
			_, _ = fmt.Fprint(connection, "226 Transfer complete\r\n")
		case "MKD":
			candidate := ftpMaintenanceFixtureResolve(cwd, argument)
			f.mu.Lock()
			if _, exists := f.entries[candidate]; exists {
				f.mu.Unlock()
				_, _ = fmt.Fprint(connection, "550 Exists\r\n")
				continue
			}
			f.entries[candidate] = ftpMaintenanceFixtureEntry{kind: "directory", mtime: time.Date(2025, 3, 4, 5, 6, 0, 0, time.UTC)}
			f.mu.Unlock()
			_, _ = fmt.Fprintf(connection, "257 %q created\r\n", candidate)
		case "DELE":
			candidate := ftpMaintenanceFixtureResolve(cwd, argument)
			f.mu.Lock()
			entry, exists := f.entries[candidate]
			if exists && entry.kind == "file" {
				delete(f.entries, candidate)
				f.mu.Unlock()
				_, _ = fmt.Fprint(connection, "250 Removed\r\n")
			} else {
				f.mu.Unlock()
				_, _ = fmt.Fprint(connection, "550 Not a file\r\n")
			}
		case "RMD":
			candidate := ftpMaintenanceFixtureResolve(cwd, argument)
			f.mu.Lock()
			entry, exists := f.entries[candidate]
			empty := exists && entry.kind == "directory"
			for name := range f.entries {
				if name != candidate && path.Dir(name) == candidate {
					empty = false
				}
			}
			if empty {
				delete(f.entries, candidate)
			}
			f.mu.Unlock()
			if empty {
				_, _ = fmt.Fprint(connection, "250 Removed\r\n")
			} else {
				_, _ = fmt.Fprint(connection, "550 Not empty or not a directory\r\n")
			}
		case "QUIT":
			_, _ = fmt.Fprint(connection, "221 Bye\r\n")
			return
		default:
			_, _ = fmt.Fprint(connection, "200 Okay\r\n")
		}
	}
}

func (f *ftpMaintenanceFixture) listing(directory string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var listing strings.Builder
	for name, entry := range f.entries {
		if name == directory || path.Dir(name) != directory {
			continue
		}
		base := path.Base(name)
		date := entry.mtime.Format("Jan _2 2006")
		switch entry.kind {
		case "directory":
			_, _ = fmt.Fprintf(&listing, "drwxr-xr-x 1 owner group 0 %s %s\r\n", date, base)
		case "file":
			_, _ = fmt.Fprintf(&listing, "-rw-r--r-- 1 owner group %d %s %s\r\n", len(entry.body), date, base)
		case "link":
			_, _ = fmt.Fprintf(&listing, "lrwxrwxrwx 1 owner group %d %s %s -> %s\r\n", len(entry.target), date, base, entry.target)
		}
	}
	return listing.String()
}

func ftpMaintenanceFixtureResolve(cwd, value string) string {
	if strings.HasPrefix(value, "/") {
		return path.Clean(value)
	}
	return path.Clean(path.Join(cwd, value))
}

func ftpMaintenanceServiceFixture(t *testing.T) (*core.Service, core.Actor, *Manager, *ftpMaintenanceFixture) {
	t.Helper()
	ctx := context.Background()
	service, err := core.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	secret, err := os.ReadFile(service.SetupSecretFile())
	if err != nil {
		t.Fatal(err)
	}
	actor, err := service.Setup(ctx, strings.TrimSpace(string(secret)), "ftp-maintenance-admin", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(service.Runtime("plugins"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	bindings := append(manager.ftpBindings(), manager.ftpMaintenanceBindings()...)
	if err = service.RegisterProvider(core.Bindings(bindings)); err != nil {
		t.Fatal(err)
	}
	if err = service.RegisterExecutor(FTPMaintenanceJobOperations(), NewFTPMaintenanceExecutor(manager)); err != nil {
		t.Fatal(err)
	}
	fixture := newFTPMaintenanceFixture(t)
	_, rawPort, _ := net.SplitHostPort(fixture.listener.Addr().String())
	port, _ := strconv.Atoi(rawPort)
	registration, _ := json.Marshal(map[string]any{"id": "maint", "host": "127.0.0.1", "port": port, "username": "fixture", "password": "maintenance-fixture-password", "tls_mode": "loopback", "directory": "/backup", "expected_revision": 0})
	if _, err = service.Execute(ctx, actor, "backup_ftp_destination_register", registration); err != nil {
		t.Fatal("register FTP maintenance fixture", err)
	}
	return service, actor, manager, fixture
}

func waitFTPMaintenanceTask(t *testing.T, service *core.Service, actor core.Actor, taskID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		value, err := service.Execute(context.Background(), actor, "tasks_get", json.RawMessage(fmt.Sprintf(`{"id":%q}`, taskID)))
		if err != nil {
			t.Fatal(err)
		}
		job := value.(map[string]any)
		if job["state"] == "succeeded" || job["state"] == "failed" || job["state"] == "canceled" || job["state"] == "interrupted" {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("FTP maintenance task did not finish")
	return nil
}

func TestFTPDestinationListTracksCurrentRevisionAfterUpdate(t *testing.T) {
	service, actor, manager, _ := ftpMaintenanceServiceFixture(t)
	first, err := service.Execute(context.Background(), actor, "backup_ftp_destinations_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal("list registered destination", err)
	}
	items := first.(map[string]any)["items"].([]FTPDestination)
	if len(items) != 1 || items[0].Revision != 1 || items[0].CredentialRef != "" {
		t.Fatal("destination list did not expose its current redacted revision", first)
	}
	profile, err := manager.ftpProfile(context.Background(), "maint")
	if err != nil {
		t.Fatal(err)
	}
	update, _ := json.Marshal(map[string]any{
		"id": profile.ID, "host": profile.Host, "port": profile.Port, "username": profile.Username,
		"password": "maintenance-fixture-password", "tls_mode": profile.TLSMode, "directory": profile.Directory,
		"expected_revision": profile.Revision,
	})
	if _, err = service.Execute(context.Background(), actor, "backup_ftp_destination_register", update); err != nil {
		t.Fatal("update FTP destination", err)
	}
	second, err := service.Execute(context.Background(), actor, "backup_ftp_destinations_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal("list updated destination", err)
	}
	items = second.(map[string]any)["items"].([]FTPDestination)
	if len(items) != 1 || items[0].Revision != 2 || items[0].CredentialRef != "" {
		t.Fatal("destination list returned a stale revision or credential reference after update", second)
	}
}

func TestFTPDestinationRegisterWaitsForActiveMaintenanceLock(t *testing.T) {
	service, actor, manager, _ := ftpMaintenanceServiceFixture(t)
	profile, err := manager.ftpProfile(context.Background(), "maint")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := acquireFTPMaintenanceLock(context.Background(), profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		input, _ := json.Marshal(map[string]any{
			"id": profile.ID, "host": profile.Host, "port": profile.Port, "username": profile.Username,
			"password": "maintenance-fixture-password", "tls_mode": profile.TLSMode, "directory": profile.Directory,
			"expected_revision": profile.Revision,
		})
		_, err := service.Execute(context.Background(), actor, "backup_ftp_destination_register", input)
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("destination registration completed while the profile lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	current, err := manager.ftpProfile(context.Background(), profile.ID)
	if err != nil || current.Revision != profile.Revision {
		t.Fatalf("blocked registration changed the profile revision: %#v %v", current, err)
	}
	unlock()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal("registration did not continue after releasing the profile lock", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registration did not wake after releasing the profile lock")
	}
	current, err = manager.ftpProfile(context.Background(), profile.ID)
	if err != nil || current.Revision != profile.Revision+1 {
		t.Fatalf("completed registration did not advance the profile revision once: %#v %v", current, err)
	}
}

func TestFTPMaintenanceBoundsBrowsingAndUsesRealProtocolCreateDeleteTasks(t *testing.T) {
	service, actor, _, fixture := ftpMaintenanceServiceFixture(t)
	fixture.addDir("/backup/sub")
	fixture.addFile("/backup/sub/old.sql", []byte("private-backup-content"))
	fixture.addLink("/backup/outside-link", "/outside")
	fixture.addDir("/outside")
	fixture.addFile("/outside/secret.sql", []byte("outside"))

	rootRaw := json.RawMessage(`{"id":"maint","path":""}`)
	listed, err := service.Execute(context.Background(), actor, "backup_ftp_directory_list", rootRaw)
	if err != nil {
		t.Fatal("safe root listing failed", err)
	}
	listing, _ := json.Marshal(listed)
	if !strings.Contains(string(listing), `"name":"outside-link"`) || !strings.Contains(string(listing), `"kind":"link"`) || strings.Contains(string(listing), "secret.sql") {
		t.Fatal("safe listing did not classify or contain link rows", string(listing))
	}
	if listed.(map[string]any)["revision"] != int64(1) {
		t.Fatal("unversioned read did not return the current destination revision", listed)
	}
	if _, err = service.Execute(context.Background(), actor, "backup_ftp_directory_list", json.RawMessage(`{"id":"maint","path":"outside-link","expected_revision":1}`)); core.ErrorCode(err) != "path_unsafe" {
		t.Fatal("directory symlink was followed", err)
	}
	if _, err = service.Execute(context.Background(), actor, "backup_ftp_directory_list", json.RawMessage(`{"id":"maint","path":"../outside","expected_revision":1}`)); core.ErrorCode(err) != "invalid_input" {
		t.Fatal("path traversal was accepted", err)
	}

	createRaw := json.RawMessage(`{"id":"maint","path":"sub","name":"new-folder","expected_revision":1}`)
	created, err := service.Execute(context.Background(), actor, "backup_ftp_directory_create", createRaw)
	if err != nil {
		t.Fatal("directory create task submission failed", err)
	}
	createTask := created.(map[string]any)
	createdJob := waitFTPMaintenanceTask(t, service, actor, createTask["id"].(string))
	if createdJob["state"] != "succeeded" || !fixture.exists("/backup/sub/new-folder") {
		t.Fatal("directory create task did not create the target", createdJob)
	}
	createResult, _ := json.Marshal(createdJob["result"])
	if strings.Contains(string(createResult), "maintenance-fixture-password") {
		t.Fatal("FTP password entered task result", string(createResult))
	}

	previewRaw := json.RawMessage(`{"id":"maint","path":"sub","name":"old.sql","kind":"file","expected_revision":1}`)
	previewAny, err := service.Execute(context.Background(), actor, "backup_ftp_delete_preview", previewRaw)
	if err != nil {
		t.Fatal("file delete preview failed", err)
	}
	preview := previewAny.(map[string]any)
	if preview["revision"] != int64(1) {
		t.Fatal("preview did not freeze the destination revision", preview)
	}
	previewID := preview["preview_id"].(string)
	deleteRaw, _ := json.Marshal(map[string]any{"id": "maint", "path": "sub", "name": "old.sql", "kind": "file", "expected_revision": 1, "preview_id": previewID})
	deleted, err := service.Execute(context.Background(), actor, "backup_ftp_delete", deleteRaw)
	if err != nil {
		t.Fatal("confirmed delete task submission failed", err)
	}
	deleteTask := deleted.(map[string]any)
	deletedJob := waitFTPMaintenanceTask(t, service, actor, deleteTask["id"].(string))
	if deletedJob["state"] != "succeeded" || fixture.exists("/backup/sub/old.sql") {
		t.Fatal("confirmed file delete did not complete", deletedJob)
	}
	if _, err = service.Execute(context.Background(), actor, "backup_ftp_delete", deleteRaw); core.ErrorCode(err) != "preview_used" {
		t.Fatal("one-use preview admitted a second delete", err)
	}
}

func TestFTPMaintenanceDeleteRequiresFreshPreviewAndOnlyRemovesEmptyDirectories(t *testing.T) {
	service, actor, _, fixture := ftpMaintenanceServiceFixture(t)
	fixture.addDir("/backup/empty")
	fixture.addDir("/backup/nonempty")
	fixture.addFile("/backup/nonempty/keep.sql", []byte("keep"))
	fixture.addLink("/backup/link.sql", "/outside/secret.sql")

	if _, err := service.Execute(context.Background(), actor, "backup_ftp_delete_preview", json.RawMessage(`{"id":"maint","path":"","name":"link.sql","kind":"file","expected_revision":1}`)); core.ErrorCode(err) != "target_type_mismatch" {
		t.Fatal("symbolic link was accepted as a regular file", err)
	}
	if _, err := service.Execute(context.Background(), actor, "backup_ftp_delete_preview", json.RawMessage(`{"id":"maint","path":"","name":"nonempty","kind":"directory","expected_revision":1}`)); core.ErrorCode(err) != "directory_not_empty" {
		t.Fatal("nonempty directory received a delete preview", err)
	}
	previewAny, err := service.Execute(context.Background(), actor, "backup_ftp_delete_preview", json.RawMessage(`{"id":"maint","path":"","name":"empty","kind":"directory","expected_revision":1}`))
	if err != nil {
		t.Fatal("empty directory preview failed", err)
	}
	previewID := previewAny.(map[string]any)["preview_id"].(string)
	fixture.addFile("/backup/empty/raced.sql", []byte("arrived after preview"))
	deleteRaw, _ := json.Marshal(map[string]any{"id": "maint", "path": "", "name": "empty", "kind": "directory", "expected_revision": 1, "preview_id": previewID})
	deleteAny, err := service.Execute(context.Background(), actor, "backup_ftp_delete", deleteRaw)
	if err != nil {
		t.Fatal("delete task submission failed", err)
	}
	job := waitFTPMaintenanceTask(t, service, actor, deleteAny.(map[string]any)["id"].(string))
	if job["state"] != "failed" || !fixture.exists("/backup/empty") || !fixture.exists("/backup/empty/raced.sql") {
		t.Fatal("nonempty directory was removed after the preview", job)
	}
}

func TestFTPMaintenanceRechecksFrozenFileIdentityBeforeDelete(t *testing.T) {
	service, actor, _, fixture := ftpMaintenanceServiceFixture(t)
	fixture.addFile("/backup/report.sql", []byte("first version"))
	previewAny, err := service.Execute(context.Background(), actor, "backup_ftp_delete_preview", json.RawMessage(`{"id":"maint","path":"","name":"report.sql","kind":"file","expected_revision":1}`))
	if err != nil {
		t.Fatal("file delete preview failed", err)
	}
	previewID := previewAny.(map[string]any)["preview_id"].(string)
	fixture.addFile("/backup/report.sql", []byte("a different replacement version"))
	deleteRaw, _ := json.Marshal(map[string]any{"id": "maint", "path": "", "name": "report.sql", "kind": "file", "expected_revision": 1, "preview_id": previewID})
	deleteAny, err := service.Execute(context.Background(), actor, "backup_ftp_delete", deleteRaw)
	if err != nil {
		t.Fatal("delete task submission failed", err)
	}
	job := waitFTPMaintenanceTask(t, service, actor, deleteAny.(map[string]any)["id"].(string))
	if job["state"] != "failed" || !fixture.exists("/backup/report.sql") {
		t.Fatal("replacement remote file was removed after preview", job)
	}
}

type ftpMaintenanceGateExecutor struct {
	base    *PrivilegedExecutor
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *ftpMaintenanceGateExecutor) Validate(request agent.Request) error {
	return e.base.Validate(request)
}

func (e *ftpMaintenanceGateExecutor) Run(ctx context.Context, request agent.Request) (agent.Result, error) {
	return e.base.Run(ctx, request)
}

func (e *ftpMaintenanceGateExecutor) Stream(ctx context.Context, request agent.Request, streams agent.IO) (agent.Result, error) {
	e.once.Do(func() { close(e.started) })
	select {
	case <-ctx.Done():
		return agent.Result{Operation: request.Operation, ExitCode: -1}, ctx.Err()
	case <-e.release:
		return e.base.Stream(ctx, request, streams)
	}
}

func TestFTPMaintenanceWaitsForActiveBackupTransfer(t *testing.T) {
	service, actor, manager, fixture := ftpMaintenanceServiceFixture(t)
	base := testAgent(t)
	instance := installIntegration(t, base, "ftp-lock")
	if err := os.WriteFile(filepath.Join(instance.Paths.BackupDir, "held.sql"), []byte("owned backup bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	gate := &ftpMaintenanceGateExecutor{base: base, started: make(chan struct{}), release: make(chan struct{})}
	manager.executor = gate
	if err := service.RegisterExecutor(NativeOperations(), manager); err != nil {
		t.Fatal("register transfer executor", err)
	}
	transferInput, _ := json.Marshal(FTPTransferInput{ID: "maint", SoftwareID: instance.ID, Filename: "held.sql", Direction: "upload"})
	transfer, err := service.Execute(context.Background(), actor, "backup_ftp_transfer", transferInput)
	if err != nil {
		t.Fatal("submit backup transfer", err)
	}
	transferID := transfer.(map[string]any)["id"].(string)
	select {
	case <-gate.started:
	case <-time.After(5 * time.Second):
		t.Fatal("backup transfer did not enter its artifact stream")
	}
	select {
	case <-fixture.storStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("backup transfer did not start its remote STOR command")
	}
	createInput := json.RawMessage(`{"id":"maint","path":"","name":"created-after-transfer","expected_revision":1}`)
	create, err := service.Execute(context.Background(), actor, "backup_ftp_directory_create", createInput)
	if err != nil {
		t.Fatal("submit maintenance task during transfer", err)
	}
	createID := create.(map[string]any)["id"].(string)
	select {
	case <-time.After(150 * time.Millisecond):
		if fixture.exists("/backup/created-after-transfer") {
			t.Fatal("maintenance changed the FTP destination while a transfer held its profile lock")
		}
	case <-gate.release:
		t.Fatal("transfer gate was unexpectedly released")
	}
	close(gate.release)
	transferJob := waitFTPMaintenanceTask(t, service, actor, transferID)
	createJob := waitFTPMaintenanceTask(t, service, actor, createID)
	if transferJob["state"] != "succeeded" || createJob["state"] != "succeeded" || !fixture.exists("/backup/held.sql") || !fixture.exists("/backup/created-after-transfer") {
		t.Fatalf("transfer and maintenance did not complete in lock order: transfer=%#v create=%#v", transferJob, createJob)
	}
}

func TestFTPMaintenanceRejectsDestinationRevisionChangesAndSecretsStayOutOfDurableTask(t *testing.T) {
	service, actor, manager, fixture := ftpMaintenanceServiceFixture(t)
	fixture.addFile("/backup/report.sql", []byte("report"))
	listed, err := service.Execute(context.Background(), actor, "backup_ftp_destinations_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	items := listed.(map[string]any)["items"].([]FTPDestination)
	if len(items) != 1 || items[0].Revision != 1 || items[0].CredentialRef != "" {
		t.Fatal("destination public view did not expose only its current revision", listed)
	}
	previewAny, err := service.Execute(context.Background(), actor, "backup_ftp_delete_preview", json.RawMessage(`{"id":"maint","path":"","name":"report.sql","kind":"file","expected_revision":1}`))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewAny.(map[string]any)
	profile, err := manager.ftpProfile(context.Background(), "maint")
	if err != nil {
		t.Fatal(err)
	}
	profile.Directory = "/backup"
	profile.Revision = 2
	encodedProfile, _ := json.Marshal(profile)
	if _, err = manager.deps.State.CompareAndSwap(context.Background(), "ftp_destinations", "maint", 1, encodedProfile); err != nil {
		t.Fatal(err)
	}
	deleteRaw, _ := json.Marshal(map[string]any{"id": "maint", "path": "", "name": "report.sql", "kind": "file", "expected_revision": 1, "preview_id": preview["preview_id"]})
	if _, err = service.Execute(context.Background(), actor, "backup_ftp_delete", deleteRaw); core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("stale destination revision was queued", err)
	}
	createInput, _ := json.Marshal(ftpMaintenanceJobInput{Command: "create_directory", ID: "maint", Name: "must-not-exist", ExpectedRevision: 1})
	_, err = NewFTPMaintenanceExecutor(manager).Run(context.Background(), agent.Request{Operation: ftpMaintenanceOperation, ActorID: actor.ID, JobID: "revision-recheck", Input: createInput})
	if core.ErrorCode(err) != "revision_conflict" {
		t.Fatal("executor did not recheck its pinned destination revision", err)
	}
	if !fixture.exists("/backup/report.sql") {
		t.Fatal("stale destination revision removed a remote file")
	}
	if fixture.exists("/backup/must-not-exist") {
		t.Fatal("stale task revision created a remote directory")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	canceledInput, _ := json.Marshal(ftpMaintenanceJobInput{Command: "create_directory", ID: "maint", Name: "canceled", ExpectedRevision: 2})
	_, err = NewFTPMaintenanceExecutor(manager).Run(canceled, agent.Request{Operation: ftpMaintenanceOperation, ActorID: actor.ID, JobID: "cancelled-task", Input: canceledInput})
	if !errors.Is(err, context.Canceled) || fixture.exists("/backup/canceled") {
		t.Fatal("cancellation did not stop a task before its remote mutation", err)
	}
	encodedPreview, _ := json.Marshal(preview)
	if strings.Contains(string(encodedPreview), "maintenance-fixture-password") || strings.Contains(string(encodedProfile), "maintenance-fixture-password") {
		t.Fatal("FTP plaintext secret appeared in preview/profile state")
	}
}
