package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// validateTrustedProgramPath resolves system aliases such as /bin to their
// actual target and verifies that the executable and every containing
// directory are root-owned and protected against untrusted replacement.
// Callers should use the returned canonical path when writing an execution
// configuration, and repeat this validation immediately before use.
func validateTrustedProgramPath(path string) (string, error) {
	if !filepath.IsAbs(path) || len(path) > 4096 {
		return "", errors.New("trusted program path must be absolute and bounded")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("trusted program path cannot be resolved")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("trusted program path cannot be resolved")
	}
	resolved = filepath.Clean(resolved)
	if resolved == string(filepath.Separator) {
		return "", errors.New("trusted program must resolve to a protected executable")
	}

	program, err := os.Lstat(resolved)
	if err != nil || !program.Mode().IsRegular() || program.Mode().Perm()&0111 == 0 || program.Mode().Perm()&0022 != 0 {
		return "", errors.New("trusted program must be a protected executable file")
	}
	stat, ok := program.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return "", errors.New("trusted program must be root owned")
	}

	for directory := filepath.Dir(resolved); ; directory = filepath.Dir(directory) {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("trusted program directories must be real protected directories")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return "", errors.New("trusted program directories must be root owned and protected against replacement")
		}
		if directory == string(filepath.Separator) {
			break
		}
	}
	return resolved, nil
}
