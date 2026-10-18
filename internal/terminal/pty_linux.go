//go:build linux

package terminal

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func (m *Manager) openPTY(s *session, profile resolvedProfile) error {
	masterFD, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return failure("pty_unavailable", "Linux PTY allocation is unavailable", 422)
	}
	master := os.NewFile(uintptr(masterFD), "powerlinux-pty")
	if err := unix.IoctlSetPointerInt(masterFD, unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return err
	}
	number, err := unix.IoctlGetInt(masterFD, unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return err
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return err
	}
	if err := unix.IoctlSetWinsize(masterFD, unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(s.info.Rows), Col: uint16(s.info.Cols)}); err != nil {
		master.Close()
		slave.Close()
		return err
	}
	args := []string{"-i"}
	switch filepath.Base(profile.Shell) {
	case "bash":
		args = []string{"--noprofile", "--norc", "-i"}
	case "zsh":
		args = []string{"-f", "-i"}
	case "fish":
		args = []string{"--no-config", "-i"}
	}
	cmd := exec.Command(profile.Shell, args...)
	cmd.Dir = profile.WorkDir
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + profile.Home, "USER=" + profile.User, "LOGNAME=" + profile.User, "TERM=xterm-256color", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "PS1=Folio\\$ "}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if os.Geteuid() == 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: profile.uid, Gid: profile.gid, Groups: []uint32{}}
	}
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		return failure("pty_start_failed", "Isolated terminal process could not be started", 502)
	}
	slave.Close()
	s.info.ProcessID = cmd.Process.Pid
	s.input = master
	s.resize = func(rows, cols int) error {
		return unix.IoctlSetWinsize(masterFD, unix.TIOCSWINSZ, &unix.Winsize{Row: uint16(rows), Col: uint16(cols)})
	}
	s.stop = func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); master.Close() }
	readDone := make(chan struct{})
	m.wg.Add(2)
	go func() {
		defer m.wg.Done()
		defer close(readDone)
		buffer := make([]byte, 4096)
		for {
			n, err := master.Read(buffer)
			if n > 0 {
				s.append(buffer[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer m.wg.Done()
		err := cmd.Wait()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-readDone:
		case <-time.After(250 * time.Millisecond):
			master.Close()
			<-readDone
		}
		master.Close()
		exit := cmd.ProcessState.ExitCode()
		if err != nil && exit == 0 {
			exit = -1
		}
		s.exited(exit)
	}()
	return nil
}
