package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Aoody743/PowerLinux/internal/agent"
	"github.com/Aoody743/PowerLinux/internal/core"
)

var hostedAppIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

type HostedApplication struct {
	HealthCheck      HostedAppHealthCheck `json:"health_check"`
	ID               string               `json:"id"`
	Name             string               `json:"name"`
	Runtime          string               `json:"runtime"`
	RuntimeInstance  string               `json:"runtime_instance"`
	User             string               `json:"user"`
	Status           string               `json:"status"`
	Port             int                  `json:"port"`
	Revision         int64                `json:"revision"`
	Running          bool                 `json:"running"`
	Upstream         string               `json:"upstream"`
	SourceSHA256     string               `json:"source_sha256,omitempty"`
	SourceRepository string               `json:"source_repository,omitempty"`
	SourceBranch     string               `json:"source_branch,omitempty"`
	SourceCommit     string               `json:"source_commit,omitempty"`
	UpdatedAt        time.Time            `json:"updated_at"`
}

type hostedAppReceipt struct {
	HostedApplication
	PreviousSourceSHA256     string `json:"previous_source_sha256,omitempty"`
	PreviousSourceRepository string `json:"previous_source_repository,omitempty"`
	PreviousSourceBranch     string `json:"previous_source_branch,omitempty"`
	PreviousSourceCommit     string `json:"previous_source_commit,omitempty"`
	UID                      uint32 `json:"uid"`
	GID                      uint32 `json:"gid"`
	Root                     string `json:"root"`
	ProjectDir               string `json:"project_dir"`
	DataDir                  string `json:"data_dir"`
	RuntimeProgram           string `json:"runtime_program"`
	RuntimeNPMCLI            string `json:"runtime_npm_cli,omitempty"`
}

type hostedAppCreateInput struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
}

type hostedAppInput struct {
	HealthCheck      *HostedAppHealthCheck `json:"health_check,omitempty"`
	ID               string                `json:"id"`
	ExpectedRevision int64                 `json:"expected_revision,omitempty"`
	Command          string                `json:"command,omitempty"`
	Limit            int                   `json:"limit,omitempty"`
}

type hostedAppPurgeInput struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Confirmation     string `json:"confirmation"`
}

type hostedAppLogResult struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func hostedAppValidID(id string) bool       { return hostedAppIDPattern.MatchString(id) }
func hostedAppStateID(id string) string     { return "application_" + id }
func hostedAppResource(id string) string    { return "hosted-app:" + id }
func isHostedAppRuntime(plugin string) bool { return plugin == "nodejs" || plugin == "golang" }

func hostedAppRuntimePlugin(runtime string) string {
	switch runtime {
	case "nextjs", "nodejs":
		return "nodejs"
	case "go":
		return "golang"
	default:
		return ""
	}
}

func validHostedAppRuntime(runtime string) bool { return hostedAppRuntimePlugin(runtime) != "" }

func pathsOverlap(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if left == right {
		return true
	}
	leftRel, leftErr := filepath.Rel(left, right)
	rightRel, rightErr := filepath.Rel(right, left)
	return leftErr == nil && leftRel != ".." && !strings.HasPrefix(leftRel, ".."+string(filepath.Separator)) || rightErr == nil && rightRel != ".." && !strings.HasPrefix(rightRel, ".."+string(filepath.Separator))
}

func (e *PrivilegedExecutor) hostedAppsEnabled() error {
	if e.config.ApplicationsRootDir == "" {
		return failure("applications_unavailable", "Hosted application services are not enabled in the trusted Agent policy", 412)
	}
	if os.Geteuid() != 0 || !e.config.UseSystemd {
		return failure("service_manager_required", "Hosted application services require the root Agent and systemd", 412)
	}
	if runtimePath, err := filepath.Abs(e.config.ApplicationsRootDir); err != nil || runtimePath != filepath.Clean(e.config.ApplicationsRootDir) || pathsOverlap(runtimePath, e.config.RootDir) || pathsOverlap(runtimePath, e.config.StateDir) {
		return failure("ownership_invalid", "Hosted application directory does not match the trusted policy", 412)
	}
	if err := realDirectory(e.config.ApplicationsRootDir); err != nil {
		return failure("ownership_invalid", "Hosted application root must be a protected real directory", 412)
	}
	info, err := os.Stat(e.config.ApplicationsRootDir)
	if err != nil {
		return failure("ownership_invalid", "Hosted application root must be a protected real directory", 412)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return failure("ownership_invalid", "Hosted application root must be root-owned and not group/world writable", 412)
	}
	return nil
}

func hostedAppUser(id string) string {
	digest := sha256.Sum256([]byte(id))
	return "plapp_" + hex.EncodeToString(digest[:8])
}

func (e *PrivilegedExecutor) hostedAppPaths(id string) (root, project, data string) {
	root = filepath.Join(e.config.ApplicationsRootDir, id)
	return root, filepath.Join(root, "project"), filepath.Join(root, "data")
}

func ensureHostedAppRootTraversable(receipt hostedAppReceipt) error {
	info, err := os.Lstat(receipt.Root)
	if err != nil {
		return err
	}
	stat, ok := statOf(info)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return failure("ownership_invalid", "Hosted application root must be root-owned and protected", 412)
	}
	if info.Mode().Perm() != 0711 {
		if err = os.Chmod(receipt.Root, 0711); err != nil {
			return err
		}
	}
	return nil
}

func (e *PrivilegedExecutor) hostedAppRuntime(ctx context.Context, runtime string) (Instance, string, error) {
	pluginID, program := "", ""
	switch runtime {
	case "nextjs", "nodejs":
		pluginID = "nodejs"
	case "go":
		pluginID = "golang"
	default:
		return Instance{}, "", failure("invalid_input", "Application runtime must be Node.js, Next.js or Go", 400)
	}
	instanceID := pluginID + "-default"
	instance, err := e.get(ctx, instanceID, false)
	if err != nil || instance.PluginID != pluginID || instance.Status == "uninstalled" || instance.Service.Program == "" {
		return Instance{}, "", failure("application_runtime_required", "Install the trusted Node.js/npm or Go runtime from Software before creating this application", 412)
	}
	if runtime == "go" || runtime == "nodejs" || runtime == "nextjs" {
		program = instance.Service.Program
	}
	canonical, err := validateTrustedProgramPath(program)
	if err != nil {
		return Instance{}, "", failure("application_runtime_required", "The managed application runtime executable is missing or no longer trusted", 412)
	}
	if runtime == "go" || runtime == "nodejs" || runtime == "nextjs" {
		instance.Service.Program = canonical
	}
	return instance, canonical, nil
}

func hostedAppNPMCLI(instance Instance) (string, error) {
	if instance.PluginID != "nodejs" || instance.Paths.Root == "" || !filepath.IsAbs(instance.Paths.Root) {
		return "", failure("application_runtime_required", "The managed Node.js runtime receipt is invalid", 412)
	}
	program := filepath.Join(instance.Paths.Root, "runtime", "lib", "node_modules", "npm", "bin", "npm-cli.js")
	canonical, err := validateTrustedProgramPath(program)
	if err != nil {
		return "", failure("application_runtime_required", "The managed npm CLI is missing or no longer trusted", 412)
	}
	return canonical, nil
}

func (e *PrivilegedExecutor) hostedAppReceipt(id string) (hostedAppReceipt, error) {
	var receipt hostedAppReceipt
	if !hostedAppValidID(id) {
		return receipt, failure("invalid_input", "Invalid hosted application ID", 400)
	}
	if err := e.load("private", hostedAppStateID(id), &receipt); err != nil {
		return receipt, err
	}
	root, project, data := e.hostedAppPaths(id)
	if receipt.ID != id || !validHostedAppRuntime(receipt.Runtime) || receipt.Root != root || receipt.ProjectDir != project || receipt.DataDir != data || receipt.User != hostedAppUser(id) || receipt.RuntimeInstance != hostedAppRuntimePlugin(receipt.Runtime)+"-default" || receipt.UID == 0 || receipt.GID == 0 || receipt.Port < 1024 || receipt.Port > 65535 || receipt.Revision < 1 || !filepath.IsAbs(receipt.ProjectDir) || receipt.SourceSHA256 != "" && !validHostedAppSHA256(receipt.SourceSHA256) || receipt.PreviousSourceSHA256 != "" && !validHostedAppSHA256(receipt.PreviousSourceSHA256) || !validHostedAppGitSourceMetadata(receipt.SourceRepository, receipt.SourceBranch, receipt.SourceCommit) || !validHostedAppGitSourceMetadata(receipt.PreviousSourceRepository, receipt.PreviousSourceBranch, receipt.PreviousSourceCommit) {
		return receipt, failure("ownership_invalid", "Hosted application receipt disagrees with the trusted layout", 412)
	}
	if receipt.Status != "created" && receipt.Status != "built" && receipt.Status != "running" && receipt.Status != "stopped" && receipt.Status != "retained" && receipt.Status != "purging" {
		return receipt, failure("record_invalid", "Hosted application receipt has an unknown lifecycle state", 412)
	}
	if receipt.Upstream != "http://127.0.0.1:"+strconv.Itoa(receipt.Port) {
		return receipt, failure("ownership_invalid", "Hosted application upstream disagrees with its trusted port", 412)
	}
	if receipt.Status == "purging" {
		if err := e.validateHostedAppPurgingReceipt(receipt); err != nil {
			return receipt, err
		}
		return receipt, nil
	}
	if receipt.Status != "retained" {
		expectedProgram := ""
		expectedNPMCLI := ""
		runtimeInstance, runtimeErr := e.get(context.Background(), receipt.RuntimeInstance, false)
		if runtimeErr != nil || runtimeInstance.PluginID != hostedAppRuntimePlugin(receipt.Runtime) || runtimeInstance.Status == "uninstalled" {
			return receipt, failure("application_runtime_required", "The application's managed runtime is unavailable", 412)
		}
		if receipt.Runtime == "go" || receipt.Runtime == "nodejs" || receipt.Runtime == "nextjs" {
			expectedProgram = runtimeInstance.Service.Program
		}
		if receipt.Runtime == "nodejs" || receipt.Runtime == "nextjs" {
			expectedNPMCLI, runtimeErr = hostedAppNPMCLI(runtimeInstance)
			if runtimeErr != nil || receipt.RuntimeNPMCLI != expectedNPMCLI {
				return receipt, failure("ownership_invalid", "Hosted application npm CLI no longer matches its managed runtime", 412)
			}
		} else if receipt.RuntimeNPMCLI != "" {
			return receipt, failure("record_invalid", "Non-Node application receipt contains an unexpected npm CLI", 412)
		}
		canonical, programErr := validateTrustedProgramPath(expectedProgram)
		if programErr != nil || canonical != receipt.RuntimeProgram {
			return receipt, failure("ownership_invalid", "Hosted application runtime program is no longer trusted", 412)
		}
	}
	account, err := user.Lookup(receipt.User)
	if err != nil || account.Uid != strconv.FormatUint(uint64(receipt.UID), 10) || account.Gid != strconv.FormatUint(uint64(receipt.GID), 10) {
		return receipt, failure("identity_conflict", "Hosted application system identity changed outside its receipt", 409)
	}
	for _, path := range []string{receipt.Root, receipt.ProjectDir, receipt.DataDir} {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return receipt, failure("ownership_invalid", "Hosted application directories must remain real directories", 412)
		}
	}
	rootInfo, err := os.Lstat(receipt.Root)
	rootStat, rootOK := statOf(rootInfo)
	if err != nil || !rootOK || rootStat.Uid != 0 || rootInfo.Mode().Perm()&0022 != 0 {
		return receipt, failure("ownership_invalid", "Hosted application root ownership changed outside its receipt", 412)
	}
	for _, path := range []string{receipt.ProjectDir, receipt.DataDir} {
		info, statErr := os.Stat(path)
		stat, ok := statOf(info)
		if statErr != nil || !ok || stat.Uid != receipt.UID || stat.Gid != receipt.GID || info.Mode().Perm()&0022 != 0 {
			return receipt, failure("ownership_invalid", "Hosted application content ownership changed outside its receipt", 412)
		}
	}
	return receipt, nil
}

func (e *PrivilegedExecutor) validateHostedAppPurgingReceipt(receipt hostedAppReceipt) error {
	account, err := user.Lookup(receipt.User)
	if err == nil {
		if account.Uid != strconv.FormatUint(uint64(receipt.UID), 10) || account.Gid != strconv.FormatUint(uint64(receipt.GID), 10) {
			return failure("identity_conflict", "Hosted application system identity changed during permanent removal", 409)
		}
	} else if hostedAppUnknownUser(err) {
		if err = hostedAppUIDAvailable(receipt); err != nil {
			return err
		}
	} else {
		return failure("identity_conflict", "Hosted application system identity could not be checked during permanent removal", 409)
	}
	rootInfo, err := os.Lstat(receipt.Root)
	rootExists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if rootExists {
		rootStat, ok := statOf(rootInfo)
		if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || !ok || rootStat.Uid != 0 || rootInfo.Mode().Perm()&0022 != 0 {
			return failure("ownership_invalid", "Hosted application root changed during permanent removal; it was preserved", 412)
		}
	}
	for _, path := range []string{receipt.ProjectDir, receipt.DataDir} {
		info, statErr := os.Lstat(path)
		if os.IsNotExist(statErr) {
			continue
		}
		stat, ok := statOf(info)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != receipt.UID || stat.Gid != receipt.GID || info.Mode().Perm()&0022 != 0 {
			return failure("ownership_invalid", "Hosted application content changed during permanent removal; it was preserved", 412)
		}
	}
	return nil
}

func hostedAppUIDAvailable(receipt hostedAppReceipt) error {
	account, err := user.LookupId(strconv.FormatUint(uint64(receipt.UID), 10))
	if err == nil && account.Username != receipt.User {
		return failure("identity_conflict", "The application UID was reassigned to another system user; its files were preserved", 409)
	}
	if err != nil && !hostedAppUnknownUserID(err) {
		return failure("identity_conflict", "The application UID could not be checked during permanent removal", 409)
	}
	return nil
}

func statOf(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

func (e *PrivilegedExecutor) saveHostedApp(receipt hostedAppReceipt) error {
	return e.save("private", hostedAppStateID(receipt.ID), receipt)
}

func hostedAppTemplates(input hostedAppCreateInput) map[string][]byte {
	nameJSON, _ := json.Marshal(input.Name)
	if input.Runtime == "go" {
		return map[string][]byte{
			"go.mod":  []byte("module powerlinux.local/" + input.ID + "\n\ngo 1.22\n"),
			"go.sum":  []byte(""),
			"main.go": []byte("package main\n\nimport (\n\t\"fmt\"\n\t\"net/http\"\n\t\"os\"\n)\n\nfunc main() {\n\tport := os.Getenv(\"PORT\")\n\tif port == \"\" { port = \"3000\" }\n\thttp.HandleFunc(\"/\", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, " + string(nameJSON) + "+\" is running\") })\n\tif err := http.ListenAndServe(\"127.0.0.1:\"+port, nil); err != nil { panic(err) }\n}\n"),
		}
	}
	if input.Runtime == "nodejs" {
		return map[string][]byte{
			"package.json":      []byte("{\n  \"name\": \"" + input.ID + "\",\n  \"version\": \"1.0.0\",\n  \"private\": true,\n  \"scripts\": {\"start\": \"node server.js\"}\n}\n"),
			"package-lock.json": []byte("{\n  \"name\": \"" + input.ID + "\",\n  \"version\": \"1.0.0\",\n  \"lockfileVersion\": 3,\n  \"requires\": true,\n  \"packages\": {\"\": {\"name\": \"" + input.ID + "\", \"version\": \"1.0.0\"}}\n}\n"),
			"server.js":         []byte("const http = require('node:http');\nconst host = process.env.HOST || '127.0.0.1';\nconst port = Number(process.env.PORT || 3000);\nconst title = " + string(nameJSON) + ";\nhttp.createServer((_request, response) => {\n  response.writeHead(200, { 'content-type': 'text/plain; charset=utf-8' });\n  response.end(`${title} is running\\n`);\n}).listen(port, host);\n"),
		}
	}
	return map[string][]byte{
		"package.json":      []byte("{\n  \"name\": \"" + input.ID + "\",\n  \"version\": \"1.0.0\",\n  \"private\": true,\n  \"scripts\": {\"build\": \"next build\", \"start\": \"next start\"},\n  \"dependencies\": {\"next\": \"15.5.27\", \"react\": \"19.3.1\", \"react-dom\": \"19.3.1\"}\n}\n"),
		"package-lock.json": hostedAppNextPackageLock(input.ID),
		"app/page.js":       []byte("const title = " + string(nameJSON) + ";\nexport default function Home() { return <main><h1>{title}</h1><p>Your Next.js service is ready.</p></main>; }\n"),
		"app/layout.js":     []byte("export const metadata = { title: " + string(nameJSON) + " };\nexport default function RootLayout({ children }) { return <html lang=\"zh-CN\"><body>{children}</body></html>; }\n"),
	}
}

func (e *PrivilegedExecutor) createHostedApp(ctx context.Context, input hostedAppCreateInput) (created HostedApplication, returnErr error) {
	var empty HostedApplication
	if err := e.hostedAppsEnabled(); err != nil {
		return empty, err
	}
	if !hostedAppValidID(input.ID) || !validHostedAppRuntime(input.Runtime) {
		return empty, failure("invalid_input", "Application ID or runtime is invalid", 400)
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 120 || strings.ContainsAny(input.Name, "\x00\r\n") {
		return empty, failure("invalid_input", "Application name must contain 1 to 120 printable characters", 400)
	}
	runtimeUnlock, err := e.lockContext(ctx, "hosted-app-runtimes")
	if err != nil {
		return empty, err
	}
	defer runtimeUnlock()
	runtimeInstance, runtimeProgram, err := e.hostedAppRuntime(ctx, input.Runtime)
	if err != nil {
		return empty, err
	}
	unlock, err := e.lockContext(ctx, "application-"+input.ID)
	if err != nil {
		return empty, err
	}
	defer unlock()
	if _, err := e.hostedAppReceipt(input.ID); err == nil {
		return empty, failure("already_exists", "Hosted application already exists", 409)
	} else if core.ErrorCode(err) != "not_found" {
		return empty, err
	}
	root, project, data := e.hostedAppPaths(input.ID)
	if _, err := os.Lstat(root); err == nil {
		return empty, failure("ownership_conflict", "Application directory exists without a trusted receipt; it was preserved", 409)
	} else if !os.IsNotExist(err) {
		return empty, err
	}
	userName := hostedAppUser(input.ID)
	if _, err = user.Lookup(userName); err == nil {
		return empty, failure("identity_conflict", "Application system user exists without a trusted receipt; it was preserved", 409)
	} else if !hostedAppUnknownUser(err) {
		return empty, err
	}
	port, err := hostedAppFreeLoopbackPort()
	if err != nil {
		return empty, err
	}
	if err = os.Mkdir(root, 0755); err != nil {
		return empty, err
	}
	rootCreated := true
	userCreated := false
	committed := false
	var appUID, appGID uint32
	defer func() {
		if committed || !rootCreated {
			return
		}
		if userCreated {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			userdel, userdelErr := validateTrustedProgramPath("/usr/sbin/userdel")
			if userdelErr == nil {
				_, userdelErr = runFixed(cleanupCtx, userdel, []string{userName}, 30*time.Second, nil)
			}
			cancel()
			if userdelErr != nil {
				returnErr = errors.Join(returnErr, failure("application_create_recovery_required", "Application creation failed and its isolated system user could not be removed; its files were preserved", 500))
				return
			}
			account, lookupErr := user.Lookup(userName)
			if lookupErr == nil {
				uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
				gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
				if uidErr != nil || gidErr != nil || uint32(uid) != appUID || uint32(gid) != appGID {
					returnErr = errors.Join(returnErr, failure("application_create_recovery_required", "Application creation failed and its system identity changed during cleanup; files were preserved", 500))
					return
				}
			} else if !hostedAppUnknownUser(lookupErr) {
				returnErr = errors.Join(returnErr, failure("application_create_recovery_required", "Application identity could not be verified after cleanup; files were preserved", 500))
				return
			}
		}
		if removeErr := os.RemoveAll(root); removeErr != nil {
			returnErr = errors.Join(returnErr, failure("application_create_recovery_required", "Application creation failed and its incomplete files could not be removed", 500))
		}
	}()
	if err = os.Chown(root, 0, 0); err != nil {
		return empty, err
	}
	if err = os.Chmod(root, 0711); err != nil {
		return empty, err
	}
	if err = os.Mkdir(project, 0750); err != nil {
		return empty, err
	}
	if err = os.Mkdir(data, 0750); err != nil {
		return empty, err
	}
	useradd, err := validateTrustedProgramPath("/usr/sbin/useradd")
	if err != nil {
		return empty, failure("identity_failed", "The trusted system user management program is unavailable", 412)
	}
	if _, err = runFixed(ctx, useradd, []string{"--system", "--user-group", "--no-create-home", "--home-dir", project, "--shell", "/usr/sbin/nologin", userName}, 30*time.Second, nil); err != nil {
		if _, lookupErr := user.Lookup(userName); lookupErr == nil || !hostedAppUnknownUser(lookupErr) {
			rootCreated = false
			return empty, failure("identity_conflict", "Application user creation failed with an unverified identity; its directory was preserved", 409)
		}
		return empty, failure("identity_failed", "Could not create the isolated application system user", 500)
	}
	userCreated = true
	account, err := user.Lookup(userName)
	if err != nil {
		return empty, err
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return empty, failure("identity_failed", "Application system identity must be non-root", 500)
	}
	appUID, appGID = uint32(uid), uint32(gid)
	for _, path := range []string{project, data} {
		if err = os.Chown(path, int(uid), int(gid)); err != nil {
			return empty, err
		}
	}
	if err = writeHostedAppFiles(project, hostedAppTemplates(input), uint32(uid), uint32(gid)); err != nil {
		return empty, err
	}
	if err = os.Mkdir(filepath.Join(project, ".home"), 0700); err != nil {
		return empty, err
	}
	if err = os.Chown(filepath.Join(project, ".home"), int(uid), int(gid)); err != nil {
		return empty, err
	}
	runtimeNPMCLI := ""
	if input.Runtime == "nodejs" || input.Runtime == "nextjs" {
		runtimeNPMCLI, err = hostedAppNPMCLI(runtimeInstance)
		if err != nil {
			return empty, err
		}
	}
	receipt := hostedAppReceipt{HostedApplication: HostedApplication{ID: input.ID, Name: input.Name, Runtime: input.Runtime, RuntimeInstance: runtimeInstance.ID, User: userName, Status: "created", Port: port, Revision: 1, Upstream: "http://127.0.0.1:" + strconv.Itoa(port), UpdatedAt: time.Now().UTC()}, UID: uint32(uid), GID: uint32(gid), Root: root, ProjectDir: project, DataDir: data, RuntimeProgram: runtimeProgram, RuntimeNPMCLI: runtimeNPMCLI}
	if err = e.saveHostedApp(receipt); err != nil {
		if _, receiptErr := e.hostedAppReceipt(input.ID); receiptErr == nil {
			committed = true
		}
		return empty, err
	}
	committed = true
	return receipt.HostedApplication, nil
}

func hostedAppFreeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func writeHostedAppFiles(root string, files map[string][]byte, uid, gid uint32) error {
	for name, contents := range files {
		if !filepath.IsLocal(name) {
			return errors.New("generated application template path is invalid")
		}
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0640)
		if err != nil {
			return err
		}
		if _, err = file.Write(contents); err == nil {
			err = file.Sync()
		}
		if err == nil {
			err = file.Chown(int(uid), int(gid))
		}
		if err == nil {
			err = file.Chmod(0640)
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	var directories []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, directory := range directories {
		if err := os.Chown(directory, int(uid), int(gid)); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0750); err != nil {
			return err
		}
	}
	return nil
}

func (e *PrivilegedExecutor) listHostedApps(ctx context.Context) ([]HostedApplication, error) {
	if err := e.hostedAppsEnabled(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(e.config.StateDir, "private"))
	if err != nil {
		return nil, err
	}
	items := []HostedApplication{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "application_") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Name(), "application_"), ".json")
		receipt, err := e.hostedAppReceipt(id)
		if err != nil {
			if core.ErrorCode(err) == "not_found" {
				continue
			}
			return nil, err
		}
		if receipt.Status == "built" || receipt.Status == "running" || receipt.Status == "stopped" {
			receipt.Running, err = e.hostedAppRunning(ctx, receipt, true)
			if err != nil {
				return nil, err
			}
		}
		items = append(items, receipt.HostedApplication)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (e *PrivilegedExecutor) buildHostedApp(ctx context.Context, input hostedAppInput) (HostedApplication, error) {
	var empty HostedApplication
	if err := e.hostedAppsEnabled(); err != nil {
		return empty, err
	}
	runtimeUnlock, err := e.lockContext(ctx, "hosted-app-runtimes")
	if err != nil {
		return empty, err
	}
	defer runtimeUnlock()
	unlock, err := e.lockContext(ctx, "application-"+input.ID)
	if err != nil {
		return empty, err
	}
	defer unlock()
	receipt, err := e.hostedAppReceipt(input.ID)
	if err != nil {
		return empty, err
	}
	if err = ensureHostedAppRootTraversable(receipt); err != nil {
		return empty, err
	}
	if input.ExpectedRevision != receipt.Revision {
		return empty, failure("revision_conflict", "Application revision changed; reload before building", 412)
	}
	if running, err := e.hostedAppRunning(ctx, receipt, true); err != nil {
		return empty, err
	} else if running {
		return empty, failure("application_running", "Stop the application service before building or updating its release", 412)
	}
	runtime, program, err := e.hostedAppRuntime(ctx, receipt.Runtime)
	if err != nil || runtime.ID != receipt.RuntimeInstance {
		return empty, failure("application_runtime_required", "The application's managed runtime is unavailable", 412)
	}
	credential := &syscall.Credential{Uid: receipt.UID, Gid: receipt.GID, Groups: []uint32{receipt.GID}}
	home := filepath.Join(receipt.ProjectDir, ".home")
	environment := []string{"HOME=" + home, "DATA_DIR=" + receipt.DataDir, "PORT=" + strconv.Itoa(receipt.Port), "NODE_ENV=production", "PATH=/usr/local/bin:/usr/bin:/bin", "GOPATH=" + filepath.Join(home, "go"), "GOCACHE=" + filepath.Join(home, ".cache", "go-build"), "GOPROXY=https://proxy.golang.org,direct"}
	if receipt.Runtime == "nodejs" || receipt.Runtime == "nextjs" {
		environment[4] = "PATH=" + filepath.Dir(program) + ":/usr/local/bin:/usr/bin:/bin"
	}
	environment = append(environment, hostedAppOutboundNetworkEnvironment()...)
	if receipt.Runtime == "go" {
		if err = e.buildGoHostedApp(ctx, receipt, program, credential, environment); err != nil {
			return empty, err
		}
	} else {
		if err = e.buildNodeHostedApp(ctx, receipt, program, receipt.RuntimeNPMCLI, credential, environment); err != nil {
			return empty, err
		}
	}
	receipt.Status = "built"
	receipt.Revision++
	receipt.UpdatedAt = time.Now().UTC()
	if err = e.saveHostedApp(receipt); err != nil {
		current, readErr := e.hostedAppReceipt(receipt.ID)
		if readErr == nil && current.Revision == receipt.Revision {
			return empty, err
		}
		if rollbackErr := rollbackHostedAppBuild(receipt); rollbackErr != nil {
			return empty, errors.Join(err, failure("application_build_recovery_required", "Application build was not committed to its receipt and the previous project could not be restored", 500), rollbackErr)
		}
		return empty, err
	}
	return receipt.HostedApplication, nil
}

func (e *PrivilegedExecutor) deployHostedApp(ctx context.Context, input hostedAppInput) (HostedApplication, error) {
	built, err := e.buildHostedApp(ctx, input)
	if err != nil {
		return HostedApplication{}, err
	}
	return e.controlHostedApp(ctx, hostedAppInput{ID: built.ID, ExpectedRevision: built.Revision, Command: "start"})
}

func (e *PrivilegedExecutor) buildGoHostedApp(ctx context.Context, receipt hostedAppReceipt, program string, credential *syscall.Credential, environment []string) error {
	result, err := runFixedDirectoryResult(ctx, program, receipt.ProjectDir, []string{"build", "-o", ".powerlinux-app.new", "."}, 30*time.Minute, credential, environment)
	if err != nil || result.ExitCode != 0 || result.Truncated || result.Canceled || result.TimedOut {
		return failure("application_build_failed", "Go application build failed under its isolated system user; inspect the task output", 502)
	}
	newBinary := filepath.Join(receipt.ProjectDir, ".powerlinux-app.new")
	info, err := os.Lstat(newBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return failure("application_build_failed", "Go build did not create a regular executable", 502)
	}
	current := filepath.Join(receipt.ProjectDir, "powerlinux-app")
	previous := filepath.Join(receipt.ProjectDir, ".powerlinux-app.previous")
	_ = os.Remove(previous)
	if _, err = os.Lstat(current); err == nil {
		if err = os.Rename(current, previous); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = os.Rename(newBinary, current); err != nil {
		if _, restoreErr := os.Lstat(previous); restoreErr == nil {
			_ = os.Rename(previous, current)
		}
		return err
	}
	return nil
}

func hostedAppUnit(receipt hostedAppReceipt) []byte {
	args := []string{}
	if receipt.Runtime == "nextjs" {
		args = []string{"run", "start", "--", "--hostname", "127.0.0.1", "--port", strconv.Itoa(receipt.Port)}
	} else if receipt.Runtime == "nodejs" {
		args = []string{"run", "start"}
	}
	command := unitQuote(receipt.RuntimeProgram)
	if receipt.Runtime == "go" {
		command = unitQuote(filepath.Join(receipt.ProjectDir, "powerlinux-app"))
	} else {
		if receipt.Runtime == "nodejs" || receipt.Runtime == "nextjs" {
			command += " " + unitQuote(receipt.RuntimeNPMCLI)
		}
		for _, arg := range args {
			command += " " + unitQuote(arg)
		}
	}
	hostEnvironment := ""
	pathEnvironment := "/usr/local/bin:/usr/bin:/bin"
	if receipt.Runtime == "nodejs" {
		hostEnvironment = "Environment=HOST=127.0.0.1\nEnvironment=HOSTNAME=127.0.0.1\n"
	}
	if receipt.Runtime == "nodejs" || receipt.Runtime == "nextjs" {
		pathEnvironment = filepath.Dir(receipt.RuntimeProgram) + ":" + pathEnvironment
	}
	return []byte("[Unit]\nDescription=Folio application " + receipt.ID + "\nAfter=network-online.target\nWants=network-online.target\n[Service]\nType=simple\nUser=" + receipt.User + "\nGroup=" + receipt.User + "\nWorkingDirectory=" + receipt.ProjectDir + "\nEnvironment=HOME=" + unitQuote(filepath.Join(receipt.ProjectDir, ".home")) + "\nEnvironment=DATA_DIR=" + unitQuote(receipt.DataDir) + "\nEnvironment=PATH=" + unitQuote(pathEnvironment) + "\nEnvironment=PORT=" + strconv.Itoa(receipt.Port) + "\n" + hostEnvironment + "Environment=NODE_ENV=production\nExecStart=" + command + "\nRestart=on-failure\nRestartSec=3\nNoNewPrivileges=true\nPrivateTmp=true\nProtectHome=true\nProtectSystem=strict\nReadWritePaths=" + unitQuote(receipt.ProjectDir) + " " + unitQuote(receipt.DataDir) + "\n[Install]\nWantedBy=multi-user.target\n")
}

func hostedAppUnitName(id string) string { return "powerlinux-app-" + id + ".service" }

func hostedAppLegacyQuotedWorkingDirectoryUnit(receipt hostedAppReceipt) []byte {
	unit := string(hostedAppUnit(receipt))
	current := "WorkingDirectory=" + receipt.ProjectDir + "\n"
	legacy := "WorkingDirectory=" + unitQuote(receipt.ProjectDir) + "\n"
	return []byte(strings.Replace(unit, current, legacy, 1))
}

func hostedAppUnitMatches(receipt hostedAppReceipt, current []byte) bool {
	return string(current) == string(hostedAppUnit(receipt)) || string(current) == string(hostedAppLegacyQuotedWorkingDirectoryUnit(receipt))
}

func (e *PrivilegedExecutor) ensureHostedAppUnit(ctx context.Context, receipt hostedAppReceipt) (string, error) {
	if !e.config.UseSystemd || !hostedAppValidID(receipt.ID) {
		return "", failure("service_manager_required", "Hosted application requires an owned systemd service", 412)
	}
	if _, err := validateTrustedProgramPath(receipt.RuntimeProgram); err != nil {
		return "", failure("ownership_invalid", "Hosted application runtime executable is no longer trusted", 412)
	}
	body := hostedAppUnit(receipt)
	name := hostedAppUnitName(receipt.ID)
	path := filepath.Join(e.systemdUnitDir(), name)
	current, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if err = createOnlyUnitFile(e.systemdUnitDir(), name, body); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if string(current) == string(hostedAppLegacyQuotedWorkingDirectoryUnit(receipt)) {
		if err = writeAtomicRoot(e.systemdUnitDir(), name, body, 0644, 0); err != nil {
			return "", err
		}
	} else if string(current) != string(body) {
		return "", failure("ownership_conflict", "Application systemd unit changed outside Folio; it was preserved", 409)
	}
	if _, err = runFixed(ctx, "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil); err != nil {
		return "", failure("service_reload_failed", "systemd could not reload the verified application service", 502)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func (e *PrivilegedExecutor) hostedAppRunning(ctx context.Context, receipt hostedAppReceipt, missingOK bool) (bool, error) {
	path := filepath.Join(e.systemdUnitDir(), hostedAppUnitName(receipt.ID))
	if _, err := os.Lstat(path); os.IsNotExist(err) && missingOK {
		return false, nil
	} else if err != nil {
		return false, err
	}
	current, err := os.ReadFile(path)
	if err != nil || !hostedAppUnitMatches(receipt, current) {
		return false, failure("ownership_conflict", "Application systemd unit changed outside Folio; it was preserved", 409)
	}
	legacyUnit := string(current) == string(hostedAppLegacyQuotedWorkingDirectoryUnit(receipt))
	result, err := runFixedResult(ctx, "/usr/bin/systemctl", []string{"show", "--property=ActiveState", "--value", hostedAppUnitName(receipt.ID)}, 15*time.Second, nil, nil)
	if err != nil || result.ExitCode != 0 || result.Truncated || result.Canceled || result.TimedOut || result.Stderr != "" {
		if err == nil && legacyUnit && result.ExitCode != 0 && !result.Truncated && !result.Canceled && !result.TimedOut {
			return false, nil
		}
		return false, failure("service_state_unknown", "Hosted application service state could not be verified", 502)
	}
	switch strings.TrimSpace(result.Stdout) {
	case "active":
		return true, nil
	case "inactive", "failed":
		return false, nil
	default:
		return false, failure("service_state_unknown", "Hosted application service state is unexpected", 502)
	}
}

func (e *PrivilegedExecutor) controlHostedApp(ctx context.Context, input hostedAppInput) (HostedApplication, error) {
	var empty HostedApplication
	if err := e.hostedAppsEnabled(); err != nil {
		return empty, err
	}
	runtimeUnlock, err := e.lockContext(ctx, "hosted-app-runtimes")
	if err != nil {
		return empty, err
	}
	defer runtimeUnlock()
	unlock, err := e.lockContext(ctx, "application-"+input.ID)
	if err != nil {
		return empty, err
	}
	defer unlock()
	receipt, err := e.hostedAppReceipt(input.ID)
	if err != nil {
		return empty, err
	}
	if input.ExpectedRevision != receipt.Revision {
		return empty, failure("revision_conflict", "Application revision changed; reload before changing its service", 412)
	}
	if input.Command == "start" || input.Command == "restart" {
		if err = ensureHostedAppRootTraversable(receipt); err != nil {
			return empty, err
		}
	}
	if receipt.Status == "created" && input.Command != "delete" && input.Command != "rollback" && input.Command != "health" {
		return empty, failure("application_not_built", "Build the application before starting its service", 412)
	}
	if input.Command == "health" {
		if input.HealthCheck == nil {
			return empty, failure("invalid_input", "Health check configuration required", 400)
		}
		if err = validateHostedAppHealth(*input.HealthCheck); err != nil {
			return empty, err
		}
		receipt.HealthCheck = *input.HealthCheck
	} else if input.Command == "rollback" {
		if running, stateErr := e.hostedAppRunning(ctx, receipt, true); stateErr != nil {
			return empty, stateErr
		} else if running {
			if _, err = runFixed(ctx, "/usr/bin/systemctl", []string{"stop", hostedAppUnitName(receipt.ID)}, 30*time.Second, nil); err != nil {
				return empty, err
			}
		}
		sourceHash, sourceRollback, rollbackErr := rollbackHostedApp(receipt)
		if rollbackErr != nil {
			return empty, rollbackErr
		}
		if sourceRollback {
			previousRepository := receipt.PreviousSourceRepository
			previousBranch := receipt.PreviousSourceBranch
			previousCommit := receipt.PreviousSourceCommit
			receipt.PreviousSourceSHA256 = receipt.SourceSHA256
			receipt.SourceSHA256 = sourceHash
			receipt.PreviousSourceRepository = receipt.SourceRepository
			receipt.PreviousSourceBranch = receipt.SourceBranch
			receipt.PreviousSourceCommit = receipt.SourceCommit
			receipt.SourceRepository = previousRepository
			receipt.SourceBranch = previousBranch
			receipt.SourceCommit = previousCommit
		}
		if hostedAppProjectBuilt(receipt.ProjectDir, receipt.Runtime) {
			receipt.Status = "built"
		} else {
			receipt.Status = "created"
		}
	} else if input.Command == "delete" {
		unitPath := filepath.Join(e.systemdUnitDir(), hostedAppUnitName(receipt.ID))
		if _, statErr := os.Lstat(unitPath); statErr == nil {
			current, readErr := os.ReadFile(unitPath)
			if readErr != nil || string(current) != string(hostedAppUnit(receipt)) {
				return empty, failure("ownership_conflict", "Application systemd unit changed outside Folio; application files were preserved", 409)
			}
			if _, err = runFixed(ctx, "/usr/bin/systemctl", []string{"disable", "--now", hostedAppUnitName(receipt.ID)}, 30*time.Second, nil); err != nil {
				return empty, err
			}
		} else if !os.IsNotExist(statErr) {
			return empty, statErr
		}
		if _, statErr := os.Lstat(unitPath); statErr == nil {
			if err = os.Remove(unitPath); err != nil {
				return empty, err
			}
			if _, err = runFixed(ctx, "/usr/bin/systemctl", []string{"daemon-reload"}, 20*time.Second, nil); err != nil {
				return empty, err
			}
		}
		receipt.Status = "retained"
	} else {
		if _, err = e.ensureHostedAppUnit(ctx, receipt); err != nil {
			return empty, err
		}
		unit := hostedAppUnitName(receipt.ID)
		switch input.Command {
		case "start":
			_, err = runFixed(ctx, "/usr/bin/systemctl", []string{"enable", "--now", unit}, 45*time.Second, nil)
		case "stop":
			_, err = runFixed(ctx, "/usr/bin/systemctl", []string{"stop", unit}, 45*time.Second, nil)
		case "restart":
			_, err = runFixed(ctx, "/usr/bin/systemctl", []string{"enable", "--now", unit}, 45*time.Second, nil)
			if err == nil {
				_, err = runFixed(ctx, "/usr/bin/systemctl", []string{"restart", unit}, 45*time.Second, nil)
			}
		default:
			return empty, failure("invalid_input", "Unknown hosted application service operation", 400)
		}
		if err != nil {
			return empty, failure("service_control_failed", "Hosted application service operation failed; inspect its status and logs", 502)
		}
		if input.Command != "stop" {
			if err = waitHostedAppHealthy(ctx, receipt.Port, receipt.HealthCheck); err != nil {
				stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, _ = runFixed(stopCtx, "/usr/bin/systemctl", []string{"stop", unit}, 15*time.Second, nil)
				cancel()
				return empty, failure("application_not_ready", "Application did not answer over its loopback upstream and was stopped; inspect its logs", 502)
			}
		}
		if input.Command == "stop" {
			receipt.Status = "stopped"
		} else {
			receipt.Status = "running"
		}
	}
	receipt.Revision++
	receipt.UpdatedAt = time.Now().UTC()
	if err = e.saveHostedApp(receipt); err != nil {
		return empty, err
	}
	if receipt.Status == "running" {
		receipt.Running, err = e.hostedAppRunning(ctx, receipt, false)
		if err != nil {
			return empty, err
		}
	}
	return receipt.HostedApplication, nil
}

func hostedAppPreviousProject(receipt hostedAppReceipt) string {
	return filepath.Join(receipt.Root, ".project.previous")
}

func parkHostedAppSource(path string) error {
	if err := os.Chown(path, 0, 0); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func validateParkedHostedAppSource(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := statOf(info)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || stat.Uid != 0 || stat.Gid != 0 || info.Mode().Perm() != 0700 {
		return failure("ownership_conflict", "Previous application source is not a root-protected directory; it was preserved", 409)
	}
	return nil
}

func hostedAppProjectBuilt(project, runtime string) bool {
	switch runtime {
	case "nextjs":
		info, err := os.Lstat(filepath.Join(project, ".next"))
		return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
	case "nodejs":
		info, err := os.Lstat(filepath.Join(project, ".powerlinux-node-built"))
		return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
	case "go":
		info, err := os.Lstat(filepath.Join(project, "powerlinux-app"))
		return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode()&0111 != 0
	default:
		return false
	}
}

func rollbackHostedApp(receipt hostedAppReceipt) (string, bool, error) {
	previousProject := hostedAppPreviousProject(receipt)
	if _, err := os.Lstat(previousProject); err == nil {
		if err = validateParkedHostedAppSource(previousProject); err != nil {
			return "", false, err
		}
		random, randomErr := randomPlanID()
		if randomErr != nil {
			return "", false, randomErr
		}
		failed := filepath.Join(receipt.Root, ".project-rollback-"+strings.TrimPrefix(random, "plan_"))
		if err = os.Rename(receipt.ProjectDir, failed); err != nil {
			return "", false, err
		}
		if err = os.Rename(previousProject, receipt.ProjectDir); err != nil {
			_ = os.Rename(failed, receipt.ProjectDir)
			return "", false, err
		}
		if err = os.Chown(receipt.ProjectDir, int(receipt.UID), int(receipt.GID)); err == nil {
			err = os.Chmod(receipt.ProjectDir, 0750)
		}
		if err != nil {
			_ = os.Rename(receipt.ProjectDir, previousProject)
			_ = os.Rename(failed, receipt.ProjectDir)
			return "", false, err
		}
		if err = parkHostedAppSource(failed); err != nil {
			_ = os.Rename(receipt.ProjectDir, previousProject)
			_ = os.Rename(failed, receipt.ProjectDir)
			return "", false, err
		}
		if err = os.Rename(failed, previousProject); err != nil {
			return "", false, err
		}
		return receipt.PreviousSourceSHA256, true, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	if err := rollbackHostedAppBuild(receipt); err != nil {
		return "", false, err
	}
	return receipt.SourceSHA256, false, nil
}

func rollbackHostedAppBuild(receipt hostedAppReceipt) error {
	if receipt.Runtime == "nodejs" {
		return rollbackHostedNodeProject(receipt)
	}
	if receipt.Runtime == "nextjs" {
		buildPrevious := filepath.Join(receipt.Root, ".project.build.previous")
		if _, err := os.Lstat(buildPrevious); err == nil {
			return rollbackHostedNodeProject(receipt)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	current, previous, failed := "", "", ""
	if receipt.Runtime == "go" {
		current = filepath.Join(receipt.ProjectDir, "powerlinux-app")
		previous = filepath.Join(receipt.ProjectDir, ".powerlinux-app.previous")
		failed = filepath.Join(receipt.ProjectDir, ".powerlinux-app.failed")
	} else {
		current = filepath.Join(receipt.ProjectDir, ".next")
		previous = filepath.Join(receipt.ProjectDir, ".next.previous")
		failed = filepath.Join(receipt.ProjectDir, ".next.failed")
	}
	if _, err := os.Lstat(previous); err != nil {
		return failure("rollback_unavailable", "No previous successful build is available", 412)
	}
	_ = os.RemoveAll(failed)
	if _, err := os.Lstat(current); err == nil {
		if err = os.Rename(current, failed); err != nil {
			return err
		}
	}
	if err := os.Rename(previous, current); err != nil {
		if _, restoreErr := os.Lstat(failed); restoreErr == nil {
			_ = os.Rename(failed, current)
		}
		return err
	}
	return os.Rename(failed, previous)
}

func rollbackHostedNodeProject(receipt hostedAppReceipt) error {
	previous := filepath.Join(receipt.Root, ".project.build.previous")
	if err := validateParkedHostedAppSource(previous); err != nil {
		if os.IsNotExist(err) {
			return failure("rollback_unavailable", "No previous successful application project build is available", 412)
		}
		return err
	}
	random, err := randomPlanID()
	if err != nil {
		return err
	}
	failed := filepath.Join(receipt.Root, ".project.build.failed-"+strings.TrimPrefix(random, "plan_"))
	if err = os.Rename(receipt.ProjectDir, failed); err != nil {
		return err
	}
	if err = os.Rename(previous, receipt.ProjectDir); err != nil {
		_ = os.Rename(failed, receipt.ProjectDir)
		return err
	}
	if err = os.Chown(receipt.ProjectDir, int(receipt.UID), int(receipt.GID)); err == nil {
		err = os.Chmod(receipt.ProjectDir, 0750)
	}
	if err != nil {
		_ = os.Rename(receipt.ProjectDir, previous)
		_ = os.Rename(failed, receipt.ProjectDir)
		return err
	}
	if err = parkHostedAppSource(failed); err != nil {
		_ = os.Rename(receipt.ProjectDir, previous)
		_ = os.Rename(failed, receipt.ProjectDir)
		return err
	}
	return os.Rename(failed, previous)
}

func (e *PrivilegedExecutor) logsHostedApp(ctx context.Context, input hostedAppInput) (hostedAppLogResult, error) {
	var output hostedAppLogResult
	if err := e.hostedAppsEnabled(); err != nil {
		return output, err
	}
	receipt, err := e.hostedAppReceipt(input.ID)
	if err != nil {
		return output, err
	}
	if input.Limit == 0 {
		input.Limit = 100
	}
	if input.Limit < 1 || input.Limit > 500 {
		return output, failure("invalid_input", "Application log limit must be 1 to 500 lines", 400)
	}
	path := filepath.Join(e.systemdUnitDir(), hostedAppUnitName(receipt.ID))
	if _, err = os.Lstat(path); os.IsNotExist(err) {
		return hostedAppLogResult{ID: receipt.ID, Text: ""}, nil
	} else if err != nil {
		return output, err
	}
	text, err := runFixed(ctx, "/usr/bin/journalctl", []string{"--unit", hostedAppUnitName(receipt.ID), "--no-pager", "--output=cat", "-n", strconv.Itoa(input.Limit)}, 15*time.Second, nil)
	if err != nil {
		return output, failure("application_logs_failed", "Hosted application logs are unavailable", 502)
	}
	if len(text) > 256<<10 {
		text = text[len(text)-(256<<10):]
	}
	return hostedAppLogResult{ID: receipt.ID, Text: text}, nil
}

func (e *PrivilegedExecutor) deleteHostedApp(ctx context.Context, input hostedAppInput) (HostedApplication, error) {
	input.Command = "delete"
	return e.controlHostedApp(ctx, input)
}

func (e *PrivilegedExecutor) hostedAppUnitInactive(ctx context.Context, id string) error {
	unitPath := filepath.Join(e.systemdUnitDir(), hostedAppUnitName(id))
	if _, err := os.Lstat(unitPath); err == nil {
		return failure("application_disable_required", "Disable and retain the application before permanently removing its files", 412)
	} else if !os.IsNotExist(err) {
		return err
	}
	result, err := runFixedResult(ctx, "/usr/bin/systemctl", []string{"is-active", hostedAppUnitName(id)}, 15*time.Second, nil, nil)
	// systemctl is-active returns exit status 3 for inactive/failed units and 4
	// for unknown units. runFixedResult reports nonzero status as an error, so
	// accept only those two explicit states and reject launch/timeout failures.
	if (err != nil && result.ExitCode != 3 && result.ExitCode != 4) || result.ExitCode < 0 || result.Truncated || result.Canceled || result.TimedOut || result.Stderr != "" {
		return failure("service_state_unknown", "Application service state could not be verified before permanent removal", 502)
	}
	state := strings.TrimSpace(result.Stdout)
	switch {
	case result.ExitCode == 3 && (state == "inactive" || state == "failed"):
		return nil
	case result.ExitCode == 4 && state == "unknown":
		return nil
	default:
		return failure("application_still_active", "Application service is not confirmed inactive; its files were preserved", 409)
	}
}

func (e *PrivilegedExecutor) purgeHostedApp(ctx context.Context, input hostedAppPurgeInput) (map[string]any, error) {
	if err := e.hostedAppsEnabled(); err != nil {
		return nil, err
	}
	if input.Confirmation != "DELETE "+input.ID {
		return nil, failure("confirmation_required", "Type DELETE followed by the application ID to permanently remove its files", 400)
	}
	runtimeUnlock, err := e.lockContext(ctx, "hosted-app-runtimes")
	if err != nil {
		return nil, err
	}
	defer runtimeUnlock()
	unlock, err := e.lockContext(ctx, "application-"+input.ID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	receipt, err := e.hostedAppReceipt(input.ID)
	if err != nil {
		return nil, err
	}
	if receipt.Revision != input.ExpectedRevision {
		return nil, failure("revision_conflict", "Application revision changed; reload before permanent removal", 412)
	}
	if receipt.Status != "retained" && receipt.Status != "purging" {
		return nil, failure("application_disable_required", "Disable and retain the application before permanently removing its files", 412)
	}
	if err = e.hostedAppUnitInactive(ctx, receipt.ID); err != nil {
		return nil, err
	}
	if receipt.Status != "purging" {
		receipt.Status = "purging"
		receipt.Revision++
		receipt.UpdatedAt = time.Now().UTC()
		if err = e.saveHostedApp(receipt); err != nil {
			return nil, err
		}
	}
	if account, lookupErr := user.Lookup(receipt.User); lookupErr == nil {
		if account.Uid != strconv.FormatUint(uint64(receipt.UID), 10) || account.Gid != strconv.FormatUint(uint64(receipt.GID), 10) {
			return nil, failure("identity_conflict", "Hosted application system identity changed; files were preserved", 409)
		}
		userdel, programErr := validateTrustedProgramPath("/usr/sbin/userdel")
		if programErr != nil {
			return nil, failure("identity_failed", "Trusted system user removal is unavailable; application files were preserved", 412)
		}
		if _, err = runFixed(ctx, userdel, []string{receipt.User}, 30*time.Second, nil); err != nil {
			return nil, failure("identity_in_use", "The isolated application user could not be removed; its files were preserved for retry", 409)
		}
		if _, lookupErr := user.Lookup(receipt.User); lookupErr == nil || !hostedAppUnknownUser(lookupErr) {
			return nil, failure("identity_conflict", "The isolated application account could not be confirmed removed; its files were preserved", 409)
		}
		if err = hostedAppUIDAvailable(receipt); err != nil {
			return nil, err
		}
	} else if !hostedAppUnknownUser(lookupErr) {
		return nil, failure("identity_conflict", "Hosted application system identity could not be checked; files were preserved", 409)
	}
	if err = removeOwnedDirectory(e.config.ApplicationsRootDir, receipt.Root); err != nil {
		return nil, failure("application_purge_incomplete", "The application account was removed but its protected files need cleanup retry", 500)
	}
	if err = e.removeRecord("private", hostedAppStateID(receipt.ID)); err != nil {
		return nil, failure("application_purge_incomplete", "Application files were removed but its protected receipt needs cleanup retry", 500)
	}
	return map[string]any{"id": receipt.ID, "status": "purged"}, nil
}

func validHostedAppPurgeInput(raw json.RawMessage) error {
	var input hostedAppPurgeInput
	if err := parse(raw, &input); err != nil {
		return err
	}
	if !hostedAppValidID(input.ID) || input.ExpectedRevision < 1 || input.Confirmation != "DELETE "+input.ID {
		return failure("confirmation_required", "Permanent removal requires the current revision and the exact confirmation DELETE <application ID>", 400)
	}
	return nil
}

func (e *PrivilegedExecutor) validateHostedApp(request agent.Request) error {
	if err := e.hostedAppsEnabled(); err != nil {
		return err
	}
	switch request.Operation {
	case "hosted_app_source_upload":
		return validHostedAppSourceInput(request.Input)
	case "hosted_app_git_import":
		return validHostedAppGitInput(request.Input)
	case "hosted_app_purge":
		return validHostedAppPurgeInput(request.Input)
	case "hosted_app_list":
		var input struct{}
		return parse(request.Input, &input)
	case "hosted_app_create":
		var input hostedAppCreateInput
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !hostedAppValidID(input.ID) || !validHostedAppRuntime(input.Runtime) || len(input.Name) > 120 || strings.TrimSpace(input.Name) == "" || strings.ContainsAny(input.Name, "\x00\r\n") {
			return failure("invalid_input", "Invalid hosted application identity, name or runtime", 400)
		}
	case "hosted_app_build", "hosted_app_deploy", "hosted_app_control", "hosted_app_delete", "hosted_app_logs":
		var input hostedAppInput
		if err := parse(request.Input, &input); err != nil {
			return err
		}
		if !hostedAppValidID(input.ID) {
			return failure("invalid_input", "Invalid hosted application ID", 400)
		}
		if request.Operation == "hosted_app_build" || request.Operation == "hosted_app_deploy" || request.Operation == "hosted_app_control" || request.Operation == "hosted_app_delete" {
			if input.ExpectedRevision < 1 {
				return failure("invalid_input", "Application revision is required", 400)
			}
		}
		if request.Operation == "hosted_app_control" && input.Command != "start" && input.Command != "stop" && input.Command != "restart" && input.Command != "rollback" && input.Command != "health" {
			return failure("invalid_input", "Invalid hosted application service operation", 400)
		}
		if input.HealthCheck != nil {
			if request.Operation != "hosted_app_control" || input.Command != "health" {
				return failure("invalid_input", "Health settings require the health command", 400)
			}
			if err := validateHostedAppHealth(*input.HealthCheck); err != nil {
				return err
			}
		}
		if request.Operation == "hosted_app_control" && input.Command == "health" && input.HealthCheck == nil {
			return failure("invalid_input", "Health check configuration required", 400)
		}
		if request.Operation == "hosted_app_logs" && input.Limit != 0 && (input.Limit < 1 || input.Limit > 500) {
			return failure("invalid_input", "Application log limit must be 1 to 500", 400)
		}
	default:
		return agent.ErrUnauthorized
	}
	return nil
}

func hostedAppUnknownUser(err error) bool {
	var value user.UnknownUserError
	if errors.As(err, &value) {
		return true
	}
	var pointer *user.UnknownUserError
	return errors.As(err, &pointer)
}

func hostedAppUnknownUserID(err error) bool {
	var value user.UnknownUserIdError
	if errors.As(err, &value) {
		return true
	}
	var pointer *user.UnknownUserIdError
	return errors.As(err, &pointer)
}

func (e *PrivilegedExecutor) runHostedApp(ctx context.Context, request agent.Request) (any, error) {
	if err := e.validateHostedApp(request); err != nil {
		return nil, err
	}
	switch request.Operation {
	case "hosted_app_git_import":
		return e.runHostedAppGit(ctx, request.Input)
	case "hosted_app_purge":
		var input hostedAppPurgeInput
		_ = json.Unmarshal(request.Input, &input)
		return e.purgeHostedApp(ctx, input)
	case "hosted_app_list":
		return e.listHostedApps(ctx)
	case "hosted_app_create":
		var input hostedAppCreateInput
		_ = json.Unmarshal(request.Input, &input)
		return e.createHostedApp(ctx, input)
	case "hosted_app_build":
		var input hostedAppInput
		_ = json.Unmarshal(request.Input, &input)
		return e.buildHostedApp(ctx, input)
	case "hosted_app_deploy":
		var input hostedAppInput
		_ = json.Unmarshal(request.Input, &input)
		return e.deployHostedApp(ctx, input)
	case "hosted_app_control":
		var input hostedAppInput
		_ = json.Unmarshal(request.Input, &input)
		return e.controlHostedApp(ctx, input)
	case "hosted_app_delete":
		var input hostedAppInput
		_ = json.Unmarshal(request.Input, &input)
		return e.deleteHostedApp(ctx, input)
	case "hosted_app_logs":
		var input hostedAppInput
		_ = json.Unmarshal(request.Input, &input)
		return e.logsHostedApp(ctx, input)
	}
	return nil, errors.New("unreachable hosted application operation")
}

func (m *Manager) hostedAppBindings() []core.Binding {
	resources := func(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &input) != nil || !hostedAppValidID(input.ID) {
			return nil, failure("invalid_input", "A valid application ID is required", 400)
		}
		return []string{hostedAppResource(input.ID)}, nil
	}
	createResources := func(_ context.Context, _ core.Actor, raw json.RawMessage) ([]string, error) {
		var input hostedAppCreateInput
		if err := parse(raw, &input); err != nil {
			return nil, err
		}
		if !hostedAppValidID(input.ID) || !validHostedAppRuntime(input.Runtime) {
			return nil, failure("invalid_input", "Application ID or runtime is invalid", 400)
		}
		return []string{hostedAppResource(input.ID)}, nil
	}
	bindings := []core.Binding{}
	bindings = append(bindings, m.binding("hosted_apps_source_upload", "Import a bounded tar.gz source archive into an isolated hosted application and retain the previous source for rollback. MCP may provide archive_base64 up to 512 KiB; larger archives use the REST streaming endpoint.", "hosted_apps:manage", false, hostedAppSourceUploadSchema(), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		var input hostedAppSourceUploadInput
		if err := parse(raw, &input); err != nil {
			return core.ActionResult{}, err
		}
		imported, err := m.streamHostedAppSource(ctx, actor, input)
		if err != nil {
			return core.ActionResult{}, err
		}
		return core.ActionResult{Data: imported, Resources: []string{hostedAppResource(input.ID)}, AuditSummary: map[string]any{"source_sha256": imported.SourceSHA256, "revision": imported.Revision}}, nil
	}))
	bindings[len(bindings)-1].Action.Audit = true
	bindings = append(bindings, m.binding("hosted_apps_git_import", "Import a public HTTPS or pinned-host-key SSH Git repository as the application's new source", "hosted_apps:manage", false, hostedAppGitSchema(), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		if err := validHostedAppGitInput(raw); err != nil {
			return core.ActionResult{}, err
		}
		return m.hostedAppTask(ctx, actor, "hosted_app_git_import", raw, resources)
	}))
	bindings = append(bindings, m.binding("hosted_apps_list", "List hosted Node.js, Next.js and Go applications within authorized resources", "hosted_apps:read", true, obj(map[string]any{}), func(context.Context, core.Actor, json.RawMessage) ([]string, error) { return nil, nil }, m.hostedAppsListAction))
	bindings = append(bindings, m.binding("hosted_apps_create", "Create an isolated Node.js, Next.js or Go template with its own unprivileged system user", "hosted_apps:manage", false, obj(map[string]any{"id": idSchema(), "name": textSchema(), "runtime": enumSchema("nodejs", "nextjs", "go")}, "id", "name", "runtime"), createResources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		return m.hostedAppTask(ctx, actor, "hosted_app_create", raw, createResources)
	}))
	bindings = append(bindings, m.binding("hosted_apps_build", "Build a hosted application under its own unprivileged system user and preserve its previous release", "hosted_apps:manage", false, obj(map[string]any{"id": idSchema(), "expected_revision": revisionSchema()}, "id", "expected_revision"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		return m.hostedAppTask(ctx, actor, "hosted_app_build", raw, resources)
	}))
	bindings = append(bindings, m.binding("hosted_apps_deploy", "Build and start a hosted application under its own unprivileged system user", "hosted_apps:manage", false, obj(map[string]any{"id": idSchema(), "expected_revision": revisionSchema()}, "id", "expected_revision"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		return m.hostedAppTask(ctx, actor, "hosted_app_deploy", raw, resources)
	}))
	bindings = append(bindings, m.binding("hosted_apps_control", "Start, stop, restart or roll back a hosted application through its owned systemd unit", "hosted_apps:manage", false, obj(map[string]any{"id": idSchema(), "command": enumSchema("start", "stop", "restart", "rollback", "health"), "health_check": hostedAppHealthSchema(), "expected_revision": revisionSchema()}, "id", "command", "expected_revision"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		return m.hostedAppTask(ctx, actor, "hosted_app_control", raw, resources)
	}))
	bindings = append(bindings, m.binding("hosted_apps_logs", "Read bounded logs from the owned application systemd unit", "hosted_apps:read", true, obj(map[string]any{"id": idSchema(), "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 500}}, "id"), resources, m.hostedAppsLogsAction))
	bindings = append(bindings, m.binding("hosted_apps_delete", "Disable an application service while retaining its source and data", "hosted_apps:manage", false, obj(map[string]any{"id": idSchema(), "expected_revision": revisionSchema()}, "id", "expected_revision"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		return m.hostedAppTask(ctx, actor, "hosted_app_delete", raw, resources)
	}))
	bindings = append(bindings, m.binding("hosted_apps_purge", "Permanently remove a retained hosted application after a separate explicit confirmation", "hosted_apps:manage", false, obj(map[string]any{
		"id":                idSchema(),
		"expected_revision": revisionSchema(),
		"confirmation":      map[string]any{"type": "string", "minLength": 8, "maxLength": 64},
	}, "id", "expected_revision", "confirmation"), resources, func(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
		if err := validHostedAppPurgeInput(raw); err != nil {
			return core.ActionResult{}, err
		}
		return m.hostedAppTask(ctx, actor, "hosted_app_purge", raw, resources)
	}))
	for i := range bindings {
		bindings[i].Action.Task = bindings[i].Action.Name == "hosted_apps_create" || bindings[i].Action.Name == "hosted_apps_git_import" || bindings[i].Action.Name == "hosted_apps_build" || bindings[i].Action.Name == "hosted_apps_deploy" || bindings[i].Action.Name == "hosted_apps_control" || bindings[i].Action.Name == "hosted_apps_delete" || bindings[i].Action.Name == "hosted_apps_purge"
	}
	return bindings
}

func (m *Manager) hostedAppTask(ctx context.Context, actor core.Actor, operation string, raw json.RawMessage, resolve func(context.Context, core.Actor, json.RawMessage) ([]string, error)) (core.ActionResult, error) {
	resources, err := resolve(ctx, actor, raw)
	if err != nil {
		return core.ActionResult{}, err
	}
	for _, resource := range resources {
		if !m.deps.Authorize(actor, "hosted_apps:manage", resource) {
			return core.ActionResult{}, failure("forbidden", "Hosted application is outside the authorized resources", 403)
		}
	}
	job, err := m.deps.Tasks.Submit(ctx, actor, operation, raw, resources)
	if err != nil {
		return core.ActionResult{}, err
	}
	id, _ := job["id"].(string)
	if id == "" {
		id, _ = job["task_id"].(string)
	}
	return core.ActionResult{Data: job, Resources: resources, TaskID: id}, nil
}

func (m *Manager) hostedAppsListAction(ctx context.Context, actor core.Actor, _ json.RawMessage) (core.ActionResult, error) {
	var all []HostedApplication
	if err := m.agentJSON(ctx, "hosted_app_list", json.RawMessage(`{}`), &all); err != nil {
		return core.ActionResult{}, err
	}
	items := []HostedApplication{}
	for _, app := range all {
		if m.deps.Authorize(actor, "hosted_apps:read", hostedAppResource(app.ID)) {
			items = append(items, app)
		}
	}
	return core.ActionResult{Data: map[string]any{"items": items, "total": len(items)}}, nil
}

func (m *Manager) hostedAppsLogsAction(ctx context.Context, actor core.Actor, raw json.RawMessage) (core.ActionResult, error) {
	resources, err := func() ([]string, error) {
		var input struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &input) != nil || !hostedAppValidID(input.ID) {
			return nil, failure("invalid_input", "A valid application ID is required", 400)
		}
		return []string{hostedAppResource(input.ID)}, nil
	}()
	if err != nil {
		return core.ActionResult{}, err
	}
	if !m.deps.Authorize(actor, "hosted_apps:read", resources[0]) {
		return core.ActionResult{}, failure("forbidden", "Hosted application is outside the authorized resources", 403)
	}
	var result hostedAppLogResult
	if err = m.agentJSON(ctx, "hosted_app_logs", raw, &result); err != nil {
		return core.ActionResult{}, err
	}
	return core.ActionResult{Data: result, Resources: resources}, nil
}
