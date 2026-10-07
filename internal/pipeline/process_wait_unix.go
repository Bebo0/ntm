//go:build !windows && !linux

package pipeline

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// Platforms without the Linux waitid/procfs ownership protocol retain their
// existing leader-based cleanup. WaitDelay bounds inherited output pipes.
func waitCommandWithProcessGroupCleanup(ctx context.Context, cmd *exec.Cmd) commandCleanupResult {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return commandCleanupResult{Err: err, Settled: errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)}
	case <-ctx.Done():
		select {
		case err := <-done:
			if err == nil {
				err = ctx.Err()
			}
			return commandCleanupResult{Cancelled: true, Err: err, Settled: errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)}
		default:
		}
		result := commandCleanupResult{Cancelled: true, SignalSent: "SIGTERM"}
		_ = signalCommandProcessGroup(cmd, syscall.SIGTERM)
		timer := time.NewTimer(commandCancelGracePeriod)
		defer timer.Stop()
		select {
		case result.Err = <-done:
		case <-timer.C:
			select {
			case result.Err = <-done:
			default:
				result.SignalSent = "SIGTERM,SIGKILL"
				_ = cancelCommandProcessGroup(cmd)
				result.Err = <-done
			}
		}
		if result.Err == nil {
			result.Err = ctx.Err()
		}
		result.Settled = errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH)
		return result
	}
}
