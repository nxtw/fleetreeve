package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Some files root writes live in a directory that belongs to the server's
// own user: its Vault token and the CA it verifies Vault with, which the
// server's container must be able to read. Whoever owns a directory can
// put a symbolic link in it, so root must not open such a file by name
// and trust what it gets: a link to /etc/shadow would have root overwrite
// that instead. These helpers open the name itself, never what a link at
// that name points to, look at what was opened before using it, and
// change owner and mode on what was opened, not on the name again.

// openOwned opens path for reading or writing without following a link
// at its last component, and refuses anything but an ordinary file with
// one name.
func openOwned(path string, flag int, mode os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW, mode)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, fmt.Errorf("%s is a symbolic link, and is not followed: remove it and put the file itself there", path)
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not an ordinary file", path)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
		f.Close()
		return nil, fmt.Errorf("%s has more than one name (a hard link), and is not written through", path)
	}
	return f, nil
}

// readOwned reads a file from a directory someone else owns.
func readOwned(path string) ([]byte, error) {
	f, err := openOwned(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// writeOwned writes a file in a directory someone else owns, creating it
// if it is not there, and gives it mode and - unless uid is negative -
// owner. An existing file is only emptied once it is known to be an
// ordinary file.
func writeOwned(path string, data []byte, mode os.FileMode, uid, gid int) error {
	f, err := openOwned(path, os.O_WRONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		f, err = openOwned(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if uid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			return err
		}
	}
	return f.Close()
}
