package simslim

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// useTestCategories swaps in a two-category catalog: cloud and media share
// com.apple.shared, and media has a compatibility service.
func useTestCategories(t *testing.T) {
	t.Helper()
	saved := Categories
	Categories = []Category{
		{ID: "cloud", Name: "Cloud", ApproxMemoryMB: 1, Labels: []string{"com.apple.cloudd", "com.apple.lazy", "com.apple.shared", "com.apple.xpc"}},
		{ID: "media", Name: "Media", ApproxMemoryMB: 1, Labels: []string{"com.apple.mediad", "com.apple.shared"},
			AlwaysEnabled: []AlwaysEnabledService{{Label: "com.apple.required", Reason: "needed"}}},
	}
	t.Cleanup(func() { Categories = saved })
}

// cloudLabels are the test catalog's cloud category (see useTestCategories).
var cloudLabels = []string{"com.apple.cloudd", "com.apple.lazy", "com.apple.shared", "com.apple.xpc"}

// slimmed leaves labels the way a slim boot does: disabled and not loaded.
func (f batchFake) slimmed(t *testing.T, labels ...string) {
	t.Helper()
	for _, l := range labels {
		if err := os.WriteFile(filepath.Join(f.dis, l), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(f.loaded, l)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

// spawns returns the logged `launchctl <verb>` spawns.
func (f batchFake) spawns(t *testing.T, verb string) []string {
	t.Helper()
	var out []string
	for _, call := range xcrunCalls(t, f.log) {
		if strings.Contains(call, " launchctl "+verb+" ") {
			out = append(out, call)
		}
	}
	return out
}

// assertEnabled checks that no label has a disable override, and that each
// label in loaded is also loaded.
func (f batchFake) assertEnabled(t *testing.T, labels []string, loaded ...string) {
	t.Helper()
	for _, l := range labels {
		if _, err := os.Stat(filepath.Join(f.dis, l)); err == nil {
			t.Errorf("%s still has a disable override", l)
		}
	}
	for _, l := range loaded {
		if _, err := os.Stat(filepath.Join(f.loaded, l)); err != nil {
			t.Errorf("%s is not loaded", l)
		}
	}
}

// assertNoSpawn fails if a command did more than look the simulator up.
func (f batchFake) assertNoSpawn(t *testing.T, what string) {
	t.Helper()
	if _, err := os.Stat(f.log); os.IsNotExist(err) {
		return // not even a lookup
	}
	for _, call := range xcrunCalls(t, f.log) {
		if call != "simctl list devices -j" {
			t.Errorf("%s ran %q", what, call)
		}
	}
}

// enable on a booted simulator lifts every override and loads the jobs with one
// `launchctl load -w`, without a reboot. A label without a job plist only needs
// its override lifted; one launchd will not load in this boot session is
// bootstrapped once and reported, not failed.
func TestEnableServicesStartsDaemonsWithoutAReboot(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "com.apple.xpc")
	f.slimmed(t, labels...)
	if err := os.WriteFile(f.noload, []byte("com.apple.lazy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := EnableServices(context.Background(), f.udid, []string{"cloud"}, "", nil)
	if err != nil {
		t.Fatalf("EnableServices: %v", err)
	}
	if !out.Live || !reflect.DeepEqual(out.Labels, cloudLabels) || !reflect.DeepEqual(out.NotStarted, []string{"com.apple.lazy"}) {
		t.Errorf("EnableServices = %+v, want live, labels %v, and lazy not started", out, cloudLabels)
	}
	if out.Saved != "" {
		t.Errorf("EnableServices without --save saved %q", out.Saved)
	}
	if loads := f.spawns(t, "load"); len(loads) != 1 || strings.Count(loads[0], ".plist") != 3 {
		t.Errorf("load spawns = %v, want one batch of the three plists", loads)
	}
	if enables := f.spawns(t, "enable"); !reflect.DeepEqual(enables, []string{"simctl spawn " + f.udid + " launchctl enable system/com.apple.xpc"}) {
		t.Errorf("per-label enables = %v, want only the plist-less com.apple.xpc", enables)
	}
	if boots := f.spawns(t, "bootstrap"); len(boots) != 1 || !strings.HasSuffix(boots[0], "/com.apple.lazy.plist") {
		t.Errorf("bootstraps = %v, want one retry of com.apple.lazy", boots)
	}
	for _, call := range xcrunCalls(t, f.log) {
		if strings.Contains(call, "simctl shutdown") || strings.Contains(call, " launchctl unload ") || strings.Contains(call, " launchctl disable ") {
			t.Errorf("enable ran %q", call)
		}
	}
	f.assertEnabled(t, cloudLabels, "com.apple.cloudd", "com.apple.shared")
	f.assertStopped(t, []string{"com.apple.mediad"})
}

// disable stops a category at once, except the daemons another category that
// is fully enabled right now still needs: media keeps com.apple.shared running.
func TestDisableServicesKeepsWhatAnEnabledCategoryShares(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "com.apple.xpc")

	out, err := DisableServices(context.Background(), f.udid, []string{"cloud"}, "", nil)
	if err != nil {
		t.Fatalf("DisableServices: %v", err)
	}
	stopped := []string{"com.apple.cloudd", "com.apple.lazy", "com.apple.xpc"}
	if !out.Live || !reflect.DeepEqual(out.Labels, stopped) || !reflect.DeepEqual(out.StillEnabled, []string{"com.apple.shared"}) {
		t.Errorf("DisableServices = %+v, want %v stopped and com.apple.shared kept", out, stopped)
	}
	unloads, perLabel := f.calls(t)
	if len(unloads) != 1 || len(perLabel) != 2 {
		t.Errorf("%d unload and %d per-label spawns, want one batch plus the disable and bootout of com.apple.xpc", len(unloads), len(perLabel))
	}
	f.assertStopped(t, stopped)
	f.assertEnabled(t, []string{"com.apple.shared", "com.apple.mediad"}, "com.apple.shared", "com.apple.mediad")
}

// A category that is already partly slimmed protects nothing: with mediad
// disabled, stopping cloud stops com.apple.shared too.
func TestDisableServicesStopsWhatNoEnabledCategoryNeeds(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	f.slimmed(t, "com.apple.mediad")

	out, err := DisableServices(context.Background(), f.udid, []string{"cloud"}, "", nil)
	if err != nil {
		t.Fatalf("DisableServices: %v", err)
	}
	if !reflect.DeepEqual(out.Labels, cloudLabels) || len(out.StillEnabled) != 0 {
		t.Errorf("DisableServices = %+v, want every cloud label stopped", out)
	}
	f.assertStopped(t, cloudLabels)
}

// Without --save, a simulator that is not booted is refused: there is nothing
// to change live and no state to record.
func TestServicesRefuseASimulatorThatIsNotBooted(t *testing.T) {
	useTestCategories(t)
	for _, state := range []string{"Shutdown", "Shutting Down"} {
		f := newBatchFake(t, "iOS-26-5", nil, "")
		t.Setenv("FAKE_STATE", state)
		_, err := EnableServices(context.Background(), f.udid, []string{"cloud"}, "", nil)
		if err == nil || !strings.Contains(err.Error(), "is not booted") || !strings.Contains(err.Error(), "--profile") {
			t.Errorf("EnableServices while %s = %v, want a not-booted error suggesting on --profile", state, err)
		}
		_, err = DisableServices(context.Background(), f.udid, []string{"cloud"}, "", nil)
		if err == nil || !strings.Contains(err.Error(), "is not booted") {
			t.Errorf("DisableServices while %s = %v, want a not-booted error", state, err)
		}
		f.assertNoSpawn(t, "a refused command")
	}
}

// With --save, a shutdown simulator only gets the profile written; nothing is
// applied and nothing is spawned.
func TestServicesSaveOnAShutdownSimulator(t *testing.T) {
	useTestCategories(t)
	f := newBatchFake(t, "iOS-26-5", nil, "")
	t.Setenv("FAKE_STATE", "Shutdown")
	path := filepath.Join(t.TempDir(), "dev.json")

	out, err := EnableServices(context.Background(), f.udid, []string{"cloud", "com.apple.mediad"}, path, nil)
	if err != nil {
		t.Fatalf("EnableServices --save: %v", err)
	}
	if out.Live || out.Saved != path || len(out.Labels) != 0 {
		t.Errorf("EnableServices --save = %+v, want not live, nothing applied, saved to %s", out, path)
	}
	assertProfileFile(t, path, `{
  "except": [
    "cloud"
  ],
  "keep": [
    "com.apple.mediad"
  ]
}
`)
	out, err = DisableServices(context.Background(), f.udid, []string{"cloud"}, path, nil)
	if err != nil || out.Live || out.Saved != path {
		t.Fatalf("DisableServices --save = (%+v, %v), want saved, not live", out, err)
	}
	assertProfileFile(t, path, `{
  "keep": [
    "com.apple.mediad"
  ]
}
`)
	f.assertNoSpawn(t, "--save on a shutdown simulator")
}

// With --save on a booted simulator, the change is applied live and saved.
func TestEnableServicesSavesAndAppliesLive(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	f.slimmed(t, labels...)
	path := filepath.Join(t.TempDir(), "ci.json")

	out, err := EnableServices(context.Background(), f.udid, []string{"media"}, path, nil)
	if err != nil {
		t.Fatalf("EnableServices --save: %v", err)
	}
	if !out.Live || out.Saved != path || !reflect.DeepEqual(out.Labels, []string{"com.apple.mediad", "com.apple.shared"}) {
		t.Errorf("EnableServices --save = %+v, want media started live and saved", out)
	}
	f.assertEnabled(t, []string{"com.apple.mediad", "com.apple.shared"}, "com.apple.mediad", "com.apple.shared")
	if p, err := LoadSlimProfile(path); err != nil || !p.ExceptCategories["media"] {
		t.Errorf("saved profile = (%+v, %v), want media excepted", p, err)
	}
}

// An invalid profile file fails the command before anything changes.
func TestServicesSaveRejectsAnInvalidProfileFirst(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	f.slimmed(t, labels...)
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"except": ["nope"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EnableServices(context.Background(), f.udid, []string{"cloud"}, path, nil)
	if err == nil || !strings.Contains(err.Error(), `unknown category "nope"`) {
		t.Errorf("EnableServices with a bad profile = %v, want a validation error", err)
	}
	f.assertNoSpawn(t, "a refused --save")
}

// Argument mistakes fail before any simulator is looked up.
func TestServicesRejectBadArguments(t *testing.T) {
	useTestCategories(t)
	t.Setenv("PATH", t.TempDir()) // no xcrun: a lookup would fail with another error
	ctx := context.Background()
	const udid = "00000000-0000-0000-0000-0000000000C3"
	for _, tc := range []struct {
		name string
		run  func() error
		want string
	}{
		{"enable nothing", func() error { _, err := EnableServices(ctx, udid, nil, "", nil); return err }, "name at least one category ID or managed daemon to enable"},
		{"enable unknown", func() error { _, err := EnableServices(ctx, udid, []string{"bogus"}, "", nil); return err }, `unknown category or managed daemon "bogus"`},
		{"disable nothing", func() error { _, err := DisableServices(ctx, udid, nil, "", nil); return err }, "name at least one category ID or managed daemon to disable"},
		{"disable required", func() error {
			_, err := DisableServices(ctx, udid, []string{"com.apple.required"}, "", nil)
			return err
		}, "com.apple.required is never disabled"},
	} {
		if err := tc.run(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// Right after a boot launchd_sim answers 124 ("Domain is tearing down") for a
// few seconds. enable retries its reads instead of failing, and reports the
// error only once the retries run out.
func TestEnableServicesWaitsForLaunchdToSettle(t *testing.T) {
	useTestCategories(t)
	delays := launchdRetryDelays
	launchdRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { launchdRetryDelays = delays })
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	f.slimmed(t, labels...)
	if err := os.WriteFile(f.teardown, []byte("3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := EnableServices(context.Background(), f.udid, []string{"cloud"}, "", nil); err != nil {
		t.Fatalf("EnableServices while launchd settles: %v", err)
	}
	f.assertEnabled(t, cloudLabels, "com.apple.cloudd", "com.apple.shared")

	if err := os.WriteFile(f.teardown, []byte("1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := EnableServices(context.Background(), f.udid, []string{"media"}, "", nil)
	if err == nil || !strings.Contains(err.Error(), "tearing down") {
		t.Errorf("EnableServices with launchd never ready = %v, want the 124 error", err)
	}
}

// A daemon named on its own is stopped even while every category that lists
// it is fully enabled: only a named category's daemons are kept for a neighbour.
func TestDisableServicesStopsANamedDaemon(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")

	for _, label := range []string{"com.apple.mediad", "com.apple.shared"} {
		out, err := DisableServices(context.Background(), f.udid, []string{label}, "", nil)
		if err != nil {
			t.Fatalf("DisableServices %s: %v", label, err)
		}
		if !reflect.DeepEqual(out.Labels, []string{label}) || len(out.StillEnabled) != 0 {
			t.Errorf("DisableServices %s = %+v, want it stopped and nothing kept", label, out)
		}
		f.assertStopped(t, []string{label})
	}
	f.assertEnabled(t, []string{"com.apple.cloudd"}, "com.apple.cloudd")
}

// With --save, a live change that fails leaves the profile file untouched and
// says so.
func TestServicesSaveOnlyAfterTheLiveChange(t *testing.T) {
	useTestCategories(t)
	delays := launchdRetryDelays
	launchdRetryDelays = []time.Duration{time.Millisecond}
	t.Cleanup(func() { launchdRetryDelays = delays })
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	f.slimmed(t, labels...)
	if err := os.WriteFile(f.teardown, []byte("1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ci.json")

	out, err := EnableServices(context.Background(), f.udid, []string{"media"}, path, nil)
	if err == nil || !strings.Contains(err.Error(), "not changed") {
		t.Errorf("EnableServices --save with a failing simulator = %v, want an error saying the profile was not changed", err)
	}
	if out.Saved != "" {
		t.Errorf("EnableServices --save reported %q saved after a failure", out.Saved)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("profile written after a failed live change: %v", err)
	}
	_, err = DisableServices(context.Background(), f.udid, []string{"media"}, path, nil)
	if err == nil || !strings.Contains(err.Error(), "not changed") {
		t.Errorf("DisableServices --save with a failing simulator = %v, want an error saying the profile was not changed", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("profile written after a failed live change: %v", err)
	}
}

// Only launchd_sim's error 124 is retried; any other failure of a read is
// returned at once, as before.
func TestReadLaunchdRetriesOnlyTheTransientError(t *testing.T) {
	f := newBatchFake(t, "iOS-26-5", nil, "")
	t.Setenv("FAKE_SPAWN_ERROR", "1")
	_, err := readDisabled(context.Background(), "default", f.udid)
	if err == nil || !strings.Contains(err.Error(), "Invalid device state") {
		t.Fatalf("readDisabled = %v, want the spawn error", err)
	}
	if calls := xcrunCalls(t, f.log); len(calls) != 1 {
		t.Errorf("readDisabled spawned %d times on a non-transient error, want once: %v", len(calls), calls)
	}
}

// A read that runs out of time is retried: on a heavily loaded host one
// `launchctl list` can take longer than SpawnTimeout and the next one succeed.
func TestReadLaunchdRetriesATimedOutRead(t *testing.T) {
	f := newBatchFake(t, "iOS-26-5", nil, "")
	once := filepath.Join(t.TempDir(), "hang-once")
	if err := os.WriteFile(once, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_HANG_ONCE", once)
	savedTimeout, savedWait, savedDelays := SpawnTimeout, spawnWaitDelay, launchdRetryDelays
	SpawnTimeout, spawnWaitDelay, launchdRetryDelays = 200*time.Millisecond, 100*time.Millisecond, []time.Duration{time.Millisecond}
	t.Cleanup(func() { SpawnTimeout, spawnWaitDelay, launchdRetryDelays = savedTimeout, savedWait, savedDelays })

	if _, err := readDisabled(context.Background(), "default", f.udid); err != nil {
		t.Fatalf("readDisabled after one timed-out attempt = %v, want it retried", err)
	}
	if calls := xcrunCalls(t, f.log); len(calls) != 2 {
		t.Errorf("readDisabled spawned %d times, want 2 (one timeout, one retry): %v", len(calls), calls)
	}
}

// A spawn that times out while a child process still holds its output pipe
// returns instead of waiting for that child forever, both for a read and for
// a per-label transition.
func TestLaunchdSpawnsDoNotHangOnAHeldPipe(t *testing.T) {
	f := newBatchFake(t, "iOS-26-5", []string{"com.apple.cloudd"}, "")
	t.Setenv("FAKE_HANG", "1")
	savedTimeout, savedWait, savedDelays := SpawnTimeout, spawnWaitDelay, launchdRetryDelays
	SpawnTimeout, spawnWaitDelay, launchdRetryDelays = 200*time.Millisecond, 100*time.Millisecond, []time.Duration{time.Millisecond}
	t.Cleanup(func() { SpawnTimeout, spawnWaitDelay, launchdRetryDelays = savedTimeout, savedWait, savedDelays })

	done := make(chan error, 2)
	go func() {
		_, err := readDisabled(context.Background(), "default", f.udid)
		done <- err
	}()
	go func() {
		done <- applyDelta(context.Background(), "default", f.udid, []string{"com.apple.cloudd"}, nil, liveDisable, nil)
	}()
	for range 2 {
		select {
		case err := <-done:
			if err == nil {
				t.Error("a hung spawn succeeded, want a timeout error")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a hung spawn never returned")
		}
	}
}
