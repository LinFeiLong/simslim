package simslim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockFileName is the file in a simulator's device directory, beside its
// exceptions, whose flock serializes the commands that change its daemons.
const lockFileName = "simslim.lock"

// lockPoll is how often a command waiting for a simulator's lock retries.
// flock has no timeout of its own, so waiting is a non-blocking poll that
// gives up when ctx does.
var lockPoll = 200 * time.Millisecond

// lockSimulator takes the simulator's lock and returns its release, and
// whether it had to wait for it. `on`, `enable`, `disable`, and each `watch`
// pass hold it from the moment they read the simulator's exceptions until their
// last launchctl transition, so a watch pass that started before `enable`
// recorded an exception can no longer stop the daemons enable has just
// started: one waits for the other, then reads the exceptions the other left. The lock is an flock, so it dies with its process
// and a crashed run never leaves a simulator locked; the file itself stays. A
// Device built by hand has no device directory and is not locked.
func lockSimulator(ctx context.Context, d Device, report Reporter) (unlock func(), waited bool, err error) {
	if d.DataPath == "" {
		return func() {}, false, nil
	}
	path := filepath.Join(filepath.Dir(d.DataPath), lockFileName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("lock simulator %s: %w", d.UDID, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, waited, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, false, fmt.Errorf("lock simulator %s: %w", d.UDID, err)
		}
		if !waited {
			waited = true
			report.report("Waiting for another simslim command on this simulator to finish...")
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, true, fmt.Errorf("another simslim command still holds simulator %s: %w", d.UDID, ctx.Err())
		case <-time.After(lockPoll):
		}
	}
}
