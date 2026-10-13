//go:build linux

package plugins

import (
	"context"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func shortPathDatabaseFixtureAgent(t *testing.T) *PrivilegedExecutor {
	t.Helper()
	worker, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if worker.Uid == "0" {
		worker, err = user.Lookup("nobody")
		if err != nil {
			t.Skip("non-root fixture user unavailable")
		}
	}
	api := "root"
	if worker.Username == api {
		api = "nobody"
	}

	base, err := os.MkdirTemp("/tmp", "pldb-")
	if err != nil {
		t.Fatal("short private database fixture directory could not be created")
	}
	if err = os.Chmod(base, 0700); err != nil {
		os.RemoveAll(base)
		t.Fatal("short private database fixture directory permissions could not be restricted")
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	executor, err := NewPrivilegedExecutor(PrivilegedConfig{RootDir: filepath.Join(base, "software"), StateDir: filepath.Join(base, "private"), RunUser: worker.Username, APIUser: api})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

// installManagedMySQLSocketFixture adapts the QA fixture's authenticated SSH
// TCP forward to the Unix socket path used by production-managed MySQL.
func installManagedMySQLSocketFixture(t *testing.T, instance Instance, fixture DatabaseRegisterInput) {
	t.Helper()
	ip := net.ParseIP(fixture.Host)
	if (fixture.Engine != "mariadb" && fixture.Engine != "mysql") || fixture.Access != "instance" || ip == nil || !ip.IsLoopback() || fixture.Port < 1 || fixture.Port > 65535 {
		t.Fatal("isolated MySQL fixture must use instance access over a loopback IP and valid port")
	}

	socketPath := filepath.Join(instance.Paths.RunDir, "mysql.sock")
	if len(socketPath) > 107 {
		t.Fatal("isolated MySQL fixture socket path exceeds the Linux Unix socket limit")
	}
	if _, err := os.Lstat(socketPath); err == nil {
		t.Fatal("isolated MySQL fixture socket path already exists")
	} else if !os.IsNotExist(err) {
		t.Fatal("isolated MySQL fixture socket path could not be checked")
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal("isolated MySQL fixture socket could not be created")
	}
	if err = os.Chmod(socketPath, 0600); err != nil {
		listener.Close()
		os.Remove(socketPath)
		t.Fatal("isolated MySQL fixture socket permissions could not be restricted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	endpoint := net.JoinHostPort(ip.String(), strconv.Itoa(fixture.Port))
	var mu sync.Mutex
	active := make(map[net.Conn]struct{})
	var bridges sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			local, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			bridges.Add(1)
			go func(local net.Conn) {
				defer bridges.Done()
				defer local.Close()
				upstream, dialErr := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", endpoint)
				if dialErr != nil {
					return
				}
				defer upstream.Close()

				mu.Lock()
				if ctx.Err() != nil {
					mu.Unlock()
					return
				}
				active[local] = struct{}{}
				active[upstream] = struct{}{}
				mu.Unlock()
				defer func() {
					mu.Lock()
					delete(active, local)
					delete(active, upstream)
					mu.Unlock()
				}()

				var directions sync.WaitGroup
				directions.Add(2)
				go func() {
					defer directions.Done()
					_, _ = io.Copy(upstream, local)
					if half, ok := upstream.(interface{ CloseWrite() error }); ok {
						_ = half.CloseWrite()
					}
				}()
				go func() {
					defer directions.Done()
					_, _ = io.Copy(local, upstream)
					if half, ok := local.(interface{ CloseWrite() error }); ok {
						_ = half.CloseWrite()
					}
				}()
				directions.Wait()
			}(local)
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		mu.Lock()
		for conn := range active {
			_ = conn.Close()
		}
		mu.Unlock()
		<-acceptDone
		bridges.Wait()
		_ = os.Remove(socketPath)
	})
}

func TestShortPathManagedMySQLSocketFixtureUsesPrivateCanonicalRunDir(t *testing.T) {
	e := shortPathDatabaseFixtureAgent(t)
	instance := Instance{PluginID: "mariadb", InstanceID: "managed-fixture", Paths: e.instancePaths("mariadb", "managed-fixture")}
	if err := os.MkdirAll(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal("short private fixture RunDir could not be created")
	}
	if instance.Paths != e.instancePaths(instance.PluginID, instance.InstanceID) {
		t.Fatal("short fixture paths do not match the trusted instance layout")
	}
	if err := os.Chmod(instance.Paths.RunDir, 0700); err != nil {
		t.Fatal("short private fixture RunDir permissions could not be restricted")
	}
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("local fixture TCP target could not be started")
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := upstream.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 256)
		for {
			n, readErr := conn.Read(buf)
			if n > 0 {
				if _, writeErr := conn.Write(buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = upstream.Close()
		<-serverDone
	})
	installManagedMySQLSocketFixture(t, instance, DatabaseRegisterInput{Engine: "mariadb", Access: "instance", Host: "127.0.0.1", Port: upstream.Addr().(*net.TCPAddr).Port})

	socketPath := filepath.Join(instance.Paths.RunDir, "mysql.sock")
	if len(socketPath) > 107 {
		t.Fatal("short private fixture RunDir still exceeds the Linux Unix socket limit")
	}
	info, err := os.Stat(socketPath)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("short fixture MySQL socket is missing or not private")
	}
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal("short fixture MySQL socket did not accept a local client")
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal("short fixture MySQL socket deadline could not be set")
	}
	payload := []byte{0x00, 0x01, 0x7f, 0xff, 0x2a}
	if _, err = conn.Write(payload); err != nil {
		t.Fatal("short fixture socket did not forward client bytes")
	}
	received := make([]byte, len(payload))
	if _, err = io.ReadFull(conn, received); err != nil || string(received) != string(payload) {
		t.Fatal("short fixture socket did not forward target bytes")
	}
}
