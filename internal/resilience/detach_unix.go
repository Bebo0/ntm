//go:build unix

package resilience

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func monitorPlatformSupported() error { return nil }

// Every caller opens its own descriptor. The lock is held until close and
// released by the kernel on process exit; its path is never unlinked. This
// primitive also protects short manifest mutations, on a separate lock path.
func tryMonitorLock(path string) (*os.File, error) {
	file, err := tryControlLock(path, false)
	if errors.Is(err, errControlLockBusy) {
		return nil, ErrSessionMonitorOwned
	}
	return file, err
}

// tryControlLock takes a non-blocking exclusive or shared flock on a private
// session control file. Contention is reported as errControlLockBusy.
func tryControlLock(path string, shared bool) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 || info.Mode().Perm()&0077 != 0 {
		_ = file.Close()
		return nil, errors.New("monitor/manifest lock must be a private, owned regular file with one link")
	}
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	if err := syscall.Flock(fd, how|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
			return nil, errControlLockBusy
		}
		return nil, fmt.Errorf("lock session monitor: %w", err)
	}
	// A waiter must not acquire an unlinked/replaced inode and then mistake
	// it for the rendezvous currently used by other processes.
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
		_ = file.Close()
		return nil, errors.New("monitor/manifest lock path changed while acquiring ownership")
	}
	return file, nil
}

// setDetachedProcess configures the command to run in a new session,
// detached from the terminal so it survives when the parent exits.
func setDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
