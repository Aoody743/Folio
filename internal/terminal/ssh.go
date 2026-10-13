package terminal

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

func (m *Manager) openSSH(ctx context.Context, s *session, target Target, credential Credential) error {
	config := &ssh.ClientConfig{User: target.Username, Timeout: 10 * time.Second, HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		actual := ssh.FingerprintSHA256(key)
		if subtle.ConstantTimeCompare([]byte(actual), []byte(target.HostKey)) != 1 {
			return failure("host_key_mismatch", "SSH server key does not match the pinned fingerprint", 403)
		}
		return nil
	}}
	switch target.AuthType {
	case "password":
		if len(credential.Password) == 0 {
			return failure("missing_secret", "SSH password is unavailable", 422)
		}
		config.Auth = []ssh.AuthMethod{ssh.Password(string(credential.Password))}
	case "key":
		var signer ssh.Signer
		var err error
		if len(credential.Passphrase) > 0 {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(credential.PrivateKey, credential.Passphrase)
		} else {
			signer, err = ssh.ParsePrivateKey(credential.PrivateKey)
		}
		if err != nil {
			return failure("invalid_secret", "SSH private key could not be parsed", 422)
		}
		config.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	default:
		return failure("invalid_input", "SSH authentication must be password or key", 400)
	}
	connection, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(target.Host, strconv.Itoa(target.Port)))
	if err != nil {
		return failure("ssh_connect_failed", "SSH server connection failed", 502)
	}
	connection.SetDeadline(time.Now().Add(10 * time.Second))
	stopCancel := context.AfterFunc(ctx, func() { connection.Close() })
	defer stopCancel()
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, net.JoinHostPort(target.Host, strconv.Itoa(target.Port)), config)
	if err != nil {
		connection.Close()
		var typed *Error
		if errors.As(err, &typed) {
			return typed
		}
		return failure("ssh_auth_failed", "SSH authentication or host key verification failed", 403)
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	channel, err := client.NewSession()
	if err != nil {
		client.Close()
		return failure("ssh_session_failed", "SSH session could not be created", 502)
	}
	if err := channel.RequestPty("xterm-256color", s.info.Rows, s.info.Cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}); err != nil {
		channel.Close()
		client.Close()
		return failure("ssh_pty_failed", "SSH server rejected a PTY", 502)
	}
	stdin, err := channel.StdinPipe()
	if err != nil {
		channel.Close()
		client.Close()
		return err
	}
	stdout, err := channel.StdoutPipe()
	if err != nil {
		channel.Close()
		client.Close()
		return err
	}
	stderr, err := channel.StderrPipe()
	if err != nil {
		channel.Close()
		client.Close()
		return err
	}
	if err := channel.Shell(); err != nil {
		channel.Close()
		client.Close()
		return failure("ssh_shell_failed", "SSH shell could not be started", 502)
	}
	if err := ctx.Err(); err != nil {
		client.Close()
		return err
	}
	connection.SetDeadline(time.Time{})
	s.input = stdin
	s.resize = channel.WindowChange
	s.stop = func() { channel.Close(); client.Close() }
	var readers sync.WaitGroup
	readers.Add(2)
	m.wg.Add(3)
	for _, reader := range []io.Reader{stdout, stderr} {
		go func(reader io.Reader) {
			defer readers.Done()
			defer m.wg.Done()
			buffer := make([]byte, 4096)
			for {
				n, err := reader.Read(buffer)
				if n > 0 {
					s.append(buffer[:n])
				}
				if err != nil {
					return
				}
			}
		}(reader)
	}
	go func() {
		defer m.wg.Done()
		err := channel.Wait()
		client.Close()
		readers.Wait()
		exit := 0
		if err != nil {
			exit = -1
			var status *ssh.ExitError
			if errors.As(err, &status) {
				exit = status.ExitStatus()
			}
		}
		s.exited(exit)
	}()
	return nil
}
