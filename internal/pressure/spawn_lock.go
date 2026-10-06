package pressure

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrSpawnAdmissionBusy means another process retained the admission fence
// throughout the bounded wait. No inventory read or spawn is authorized.
var ErrSpawnAdmissionBusy = errors.New("another spawn owns fleet admission")

// AcquireSpawnAdmission serializes cooperating local spawns from their fleet
// observation through their last launch. The rendezvous is per-user cache root,
// NOT per project, session, configuration file, or Go process. A caller must
// acquire before counting and release only after every launch has returned.
// The returned release is idempotent and must also be deferred for error exits.
// Kernel ownership is released on process exit; the rendezvous file is never
// removed (unlinking a live lock would allow two independent owners).
func AcquireSpawnAdmission(ctx context.Context) (func(), error) {
	if ctx == nil {
		return nil, errors.New("spawn admission requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	root, err := os.UserCacheDir()
	if err != nil || !filepath.IsAbs(root) {
		return nil, errors.New("spawn admission requires an absolute user cache directory")
	}
	dir := filepath.Join(root, "ntm", "spawn-admission")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create spawn admission directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("inspect spawn admission directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return nil, errors.New("spawn admission directory must be a real, non-shared directory")
	}
	return acquireSpawnAdmissionFile(ctx, filepath.Join(dir, "fleet.lock"), 30*time.Second)
}

func acquireSpawnAdmissionFile(ctx context.Context, path string, maxWait time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := openSpawnAdmissionFile(path)
	if err != nil {
		return nil, fmt.Errorf("open spawn admission fence: %w", err)
	}
	owned := false
	defer func() {
		if !owned {
			_ = file.Close()
		}
	}()
	waitCtx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if waitCtx.Err() != nil {
			return nil, ErrSpawnAdmissionBusy
		}
		locked, err := trySpawnAdmissionLock(file)
		if err != nil {
			return nil, fmt.Errorf("lock spawn admission fence: %w", err)
		}
		if locked {
			// Cancellation that races acquisition must not authorize a spawn.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			owned = true
			var once sync.Once
			return func() { once.Do(func() { _ = file.Close() }) }, nil
		}
		select {
		case <-waitCtx.Done():
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, ErrSpawnAdmissionBusy
		case <-ticker.C:
		}
	}
}
