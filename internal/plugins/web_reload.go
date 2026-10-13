package plugins

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const webGenerationHeader = "X-PowerLinux-Runtime-Generation"

func newWebGeneration() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce[:]), nil
}

func (e *PrivilegedExecutor) renderOpenRestyConfiguration(instance Instance, generation string) string {
	group := e.config.RunGroup
	if group == "" {
		group = e.config.RunUser
	}
	bind, _ := instance.Config["bind"].(string)
	if bind == "" {
		bind = "127.0.0.1"
	}
	proof := ""
	if generation != "" {
		proof = " add_header " + webGenerationHeader + " '" + generation + "' always;"
	}
	return fmt.Sprintf("user %s %s;\nworker_processes %d;\npid %s;\nerror_log %s warn;\nevents { worker_connections %d; }\nhttp {\n default_type application/octet-stream;\n access_log %s;\n client_max_body_size %dm;\n server { listen %s; server_name _; location / { default_type text/plain;%s return 200 'PowerLinux OpenResty\\n'; } }\n include %s;\n}\n", e.config.RunUser, group, setting(instance.Config, "worker_processes", 1), nginxQuote(instance.Paths.RunDir+"/server.pid"), nginxQuote(instance.Paths.LogDir+"/error.log"), setting(instance.Config, "worker_connections", 1024), nginxQuote(instance.Paths.LogDir+"/access.log"), setting(instance.Config, "client_max_body_size_mb", 20), net.JoinHostPort(bind, strconv.Itoa(setting(instance.Config, "port", 18080))), proof, nginxQuote(instance.Paths.ConfigDir+"/sites/*.conf"))
}

// Only the exact protected factory main config is modified. Independent site
// snippets, including administrator-supplied advanced snippets, stay intact.
func (e *PrivilegedExecutor) webGeneration(instance Instance, body []byte) (string, error) {
	if string(body) == e.renderOpenRestyConfiguration(instance, "") {
		return "", nil // Exact owned profile created before generation support.
	}
	prefix := " add_header " + webGenerationHeader + " '"
	_, remainder, ok := strings.Cut(string(body), prefix)
	if ok {
		generation, _, found := strings.Cut(remainder, "'")
		decoded, err := hex.DecodeString(generation)
		if found && err == nil && len(decoded) == 16 && generation == strings.ToLower(generation) && string(body) == e.renderOpenRestyConfiguration(instance, generation) {
			return generation, nil
		}
	}
	return "", failure("runtime_profile_unknown", "Web main configuration differs from its owned factory profile; it was preserved", 412)
}

func webHealthAddress(instance Instance) string {
	bind, _ := instance.Config["bind"].(string)
	if bind == "" || bind == "0.0.0.0" {
		bind = "127.0.0.1"
	}
	if bind == "::" {
		bind = "::1"
	}
	return net.JoinHostPort(bind, strconv.Itoa(setting(instance.Config, "port", 18080)))
}

// A healthy old cycle cannot acknowledge a new nonce. Keep-alive is disabled
// so a connection accepted by an old worker cannot persist across checks.
func probeWebGeneration(ctx context.Context, address, generation string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		return err
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK || string(body) != "PowerLinux OpenResty\n" || response.Header.Get(webGenerationHeader) != generation {
		return failure("reload_not_applied", "The owned Web runtime has not served the requested configuration generation", 502)
	}
	return nil
}

func (e *PrivilegedExecutor) signalWebReload(ctx context.Context, instance Instance) error {
	if _, err := ownedProcessID(instance); err != nil {
		return err
	}
	if e.config.UseSystemd {
		if !ownedUnit(instance) {
			return failure("ownership_invalid", "Service unit must match its managed receipt", 412)
		}
		if err := checkOwnedUnit(instance); err != nil {
			return err
		}
		_, err := runFixed(ctx, "/usr/bin/systemctl", []string{"reload", instance.Service.Unit}, 45*time.Second, nil)
		return err
	}
	args, err := e.serviceArgs(instance, "reload")
	if err != nil {
		return err
	}
	_, err = runFixed(ctx, instance.Service.Program, args, 15*time.Second, e.credential(instance))
	return err
}

func waitWebGeneration(ctx context.Context, instance Instance, master int, generation string) error {
	for {
		pid, err := ownedProcessID(instance)
		if err != nil {
			return err
		}
		if pid != master {
			return failure("runtime_identity_changed", "Owned Web master changed during configuration publication", 409)
		}
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err = probeWebGeneration(probeCtx, webHealthAddress(instance), generation)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return failure("reload_not_applied", "Web reload was acknowledged but the new configuration generation was not observed", 502)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (e *PrivilegedExecutor) reloadOpenResty(ctx context.Context, instance Instance) (generation string, returned error) {
	master, err := ownedProcessID(instance)
	if err != nil {
		return "", err
	}
	old, err := readProfileFile(instance.Paths.ConfigDir, "nginx.conf")
	if err != nil {
		return "", err
	}
	previous, err := e.webGeneration(instance, old.Body)
	if err != nil {
		return "", err
	}
	generation, err = newWebGeneration()
	if err != nil {
		return "", err
	}
	if err = writeAtomicRoot(instance.Paths.ConfigDir, "nginx.conf", []byte(e.renderOpenRestyConfiguration(instance, generation)), 0640, int(e.gid)); err != nil {
		return "", err
	}
	defer func() {
		if returned == nil {
			return
		}
		// Recovery retains the active trusted transaction marker, but is bounded
		// independently from a cancelled task. It only restores the owned main.
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		if err := writeAtomicRoot(instance.Paths.ConfigDir, "nginx.conf", old.Body, old.Mode, old.GID); err != nil {
			returned = failure("runtime_recovery_required", "Web reload failed and its previous owned main configuration needs recovery", 409)
			return
		}
		// A failed advanced snippet may still prevent another cycle. The old
		// cycle serving its exact nonce already proves the previous runtime.
		probeCtx, stop := context.WithTimeout(recovery, 500*time.Millisecond)
		previousLive := probeWebGeneration(probeCtx, webHealthAddress(instance), previous) == nil
		stop()
		if !previousLive {
			if err := e.signalWebReload(recovery, instance); err != nil {
				returned = failure("runtime_recovery_required", "Previous Web main was restored but runtime recovery could not be confirmed", 409)
				return
			}
			if err := waitWebGeneration(recovery, instance, master, previous); err != nil {
				returned = failure("runtime_recovery_required", "Previous Web main was restored but its configuration generation was not observed", 409)
			}
		}
	}()
	args, err := e.serviceArgs(instance, "check")
	if err != nil {
		return "", err
	}
	if _, err = runFixed(ctx, instance.Service.Program, args, 15*time.Second, e.credential(instance)); err != nil {
		return "", err
	}
	if err = e.signalWebReload(ctx, instance); err != nil {
		return "", err
	}
	deadline, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err = waitWebGeneration(deadline, instance, master, generation); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", ctx.Err()
		}
		return "", err
	}
	return generation, nil
}
