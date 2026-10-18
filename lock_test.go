package simslim

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// quickRetries shortens the lock poll and the launchd retry pauses, so the
// tests below wait milliseconds where a simulator would take seconds.
func quickRetries(t *testing.T) {
	t.Helper()
	poll, delays := lockPoll, launchdRetryDelays
	lockPoll = 5 * time.Millisecond
	launchdRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { lockPoll, launchdRetryDelays = poll, delays })
}

// notDone fails the test if the command has already returned.
func notDone(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s ran while another command held the simulator (err %v)", what, err)
	case <-time.After(300 * time.Millisecond):
	}
}

// enable waits for a watch pass in progress on the same simulator instead of
// starting daemons the pass is about to stop.
func TestEnableWaitsForTheSimulatorsLock(t *testing.T) {
	useTestCategories(t)
	quickRetries(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	f.slimmed(t, labels...)
	unlock, _, err := lockSimulator(context.Background(), f.device(), nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := EnableServices(context.Background(), f.udid, []string{"cloud"}, nil)
		done <- err
	}()
	notDone(t, done, "enable")
	if loads := f.spawns(t, "load"); len(loads) != 0 {
		t.Fatalf("enable loaded jobs while the simulator was locked: %v", loads)
	}
	if e, err := ReadExceptions(f.device()); err != nil || !e.IsEmpty() {
		t.Fatalf("enable recorded exceptions while the simulator was locked: %+v, %v", e, err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("EnableServices: %v", err)
	}
	f.assertEnabled(t, cloudLabels, "com.apple.cloudd", "com.apple.shared")
}

// The race seen with `simslim enable` right after a boot: a watch pass that
// starts while enable runs must read the exceptions enable records, not the
// ones from before, or it stops the daemons enable has just started.
func TestNoRebootReadsTheExceptionsOnceItHoldsTheLock(t *testing.T) {
	useTestCategories(t)
	quickRetries(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	unlock, _, err := lockSimulator(context.Background(), f.device(), nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := EnableSlimNoReboot(context.Background(), "default", f.udid, Profile{}, nil)
		done <- err
	}()
	notDone(t, done, "the watch pass")
	// What enable does while it holds the lock: record cloud, start it.
	if err := writeExceptions(f.device(), Exceptions{Except: []string{"cloud"}}); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatalf("EnableSlimNoReboot: %v", err)
	}
	f.assertEnabled(t, cloudLabels, "com.apple.cloudd", "com.apple.shared", "com.apple.xpc", "com.apple.lazy")
	f.assertStopped(t, []string{"com.apple.mediad"})
}

// Right after a reboot launchd_sim answers spawns with error 124 for a few
// seconds. A watch pass retries instead of giving up on the simulator's
// exceptions, and only reports the error once the retries run out.
func TestNoRebootWaitsForLaunchdToSettle(t *testing.T) {
	useTestCategories(t)
	quickRetries(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	f.slimmed(t, labels...)
	if err := writeExceptions(f.device(), Exceptions{Except: []string{"cloud"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.teardown, []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnableSlimNoReboot(context.Background(), "default", f.udid, Profile{}, nil); err != nil {
		t.Fatalf("EnableSlimNoReboot while launchd settles: %v", err)
	}
	f.assertEnabled(t, cloudLabels, "com.apple.cloudd", "com.apple.shared")
	f.assertStopped(t, []string{"com.apple.mediad"})

	if err := os.WriteFile(f.teardown, []byte("1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EnableSlimNoReboot(context.Background(), "default", f.udid, Profile{}, nil)
	if err == nil || !strings.Contains(err.Error(), "tearing down") {
		t.Errorf("EnableSlimNoReboot with launchd never ready = %v, want the 124 error", err)
	}
}

// A command waiting for the lock gives up with its context.
func TestLockSimulatorGivesUpWithItsContext(t *testing.T) {
	quickRetries(t)
	d := Device{UDID: "00000000-0000-0000-0000-0000000000L1", DataPath: t.TempDir() + "/data"}
	unlock, waited, err := lockSimulator(context.Background(), d, nil)
	if waited {
		t.Error("a free lock reported a wait")
	}
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var lines []string
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := lockSimulator(ctx, d, func(s string) { lines = append(lines, s) }); err == nil || !strings.Contains(err.Error(), "still holds") {
		t.Errorf("second lock = %v, want a timeout", err)
	}
	if len(lines) != 1 {
		t.Errorf("reported %q, want one waiting line", lines)
	}
	unlock()
	again, _, err := lockSimulator(context.Background(), d, nil)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
}
