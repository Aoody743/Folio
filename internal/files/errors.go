package files

import (
	"context"
	"errors"
	"io/fs"
	"syscall"
)

func moduleError(err error) error {
	if err == nil {
		return nil
	}
	var typed *Error
	if errors.As(err, &typed) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, fs.ErrNotExist):
		return failure("not_found", "Managed file or transfer was not found", 404)
	case errors.Is(err, fs.ErrExist):
		return failure("conflict", "Destination already exists", 409)
	case errors.Is(err, fs.ErrPermission):
		return failure("forbidden", "Managed file access is not permitted", 403)
	case errors.Is(err, syscall.ELOOP), isRootEscape(err):
		return failure("unsafe_path", "Symbolic link paths and root escapes are forbidden", 403)
	default:
		return err
	}
}
