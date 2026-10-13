package terminal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

func sshFixture(t *testing.T) (string, int, string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if conn.User() == "fixture" && string(password) == "fixture-only-password" {
			return nil, nil
		}
		return nil, failure("unauthorized", "fixture credential rejected", 401)
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections sync.Map
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Store(conn, true)
			go func() {
				defer connections.Delete(conn)
				defer conn.Close()
				_, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						incoming.Reject(ssh.UnknownChannelType, "only sessions")
						continue
					}
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					go func() {
						defer channel.Close()
						for request := range requests {
							switch request.Type {
							case "pty-req", "window-change":
								request.Reply(true, nil)
							case "shell":
								request.Reply(true, nil)
								io.Copy(channel, channel)
								channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Code uint32 }{0}))
								return
							default:
								request.Reply(false, nil)
							}
						}
					}()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connections.Range(func(key, value any) bool { key.(net.Conn).Close(); return true })
	})
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	number, _ := strconv.Atoi(port)
	return host, number, ssh.FingerprintSHA256(signer.PublicKey())
}
func TestSSHAuthenticationPinningAndPTYProtocol(t *testing.T) {
	host, port, fingerprint := sshFixture(t)
	manager, err := New(Options{Config: Config{StateDir: filepath.Join(t.TempDir(), "state")}, ResolveSecret: func(ctx context.Context, actor, target, ref string) (Credential, error) {
		return Credential{Password: []byte("fixture-only-password")}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()
	target, err := manager.CreateTarget(ctx, "human", Input{Label: "SSH fixture", Host: host, Port: port, Username: "fixture", AuthType: "password", SecretRef: "bound-fixture-ref", HostKey: fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	session, err := manager.Open(ctx, "human", OpenInput{TargetID: target.ID, Rows: 31, Cols: 109}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if session.Type != "ssh" {
		t.Fatal("not a real SSH channel")
	}
	text := "SSH_NATIVE_PAYLOAD"
	if _, err := manager.Input(ctx, "human", Input{ID: session.ID, Data: base64.StdEncoding.EncodeToString([]byte(text))}); err != nil {
		t.Fatal(err)
	}
	waitOutput(t, manager, "human", session.ID, 0, text)
	if _, err := manager.Resize(ctx, "human", Input{ID: session.ID, Rows: 42, Cols: 80}); err != nil {
		t.Fatal(err)
	}
	bad, err := manager.CreateTarget(ctx, "human", Input{Label: "Wrong key", Host: host, Port: port, Username: "fixture", AuthType: "password", SecretRef: "bound-fixture-ref", HostKey: "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Open(ctx, "human", OpenInput{TargetID: bad.ID}, nil); err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatal("SSH host key substitution accepted", err)
	}
	manager.CloseSession(ctx, "human", Input{ID: session.ID})
}
