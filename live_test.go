package simslim

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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

// device is the simulator as FindDevice lists it, for its exceptions file.
func (f batchFake) device() Device {
	return Device{UDID: f.udid, DataPath: f.data}
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

	out, err := EnableServices(context.Background(), f.udid, []string{"cloud"}, nil)
	if err != nil {
		t.Fatalf("EnableServices: %v", err)
	}
	if !out.Live || !reflect.DeepEqual(out.Labels, cloudLabels) || !reflect.DeepEqual(out.NotStarted, []string{"com.apple.lazy"}) {
		t.Errorf("EnableServices = %+v, want live, labels %v, and lazy not started", out, cloudLabels)
	}
	want := Exceptions{Except: []string{"cloud"}}
	if got, err := ReadExceptions(f.device()); err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(out.Exceptions, want) {
		t.Errorf("exceptions = (%+v, %v), reported %+v; want %+v", got, err, out.Exceptions, want)
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

// On a shutdown simulator enable spawns nothing: it lifts the overrides in the
// store launchd_sim boots from and records the exception.
func TestEnableServicesOnAShutdownSimulatorWritesTheStore(t *testing.T) {
	useTestCategories(t)
	useTempStoreRoot(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	t.Setenv("FAKE_STATE", "Shutdown")
	if err := writeDisabledStore(f.udid, labels, nil); err != nil {
		t.Fatal(err)
	}

	out, err := EnableServices(context.Background(), f.udid, []string{"com.apple.mediad"}, nil)
	if err != nil {
		t.Fatalf("EnableServices: %v", err)
	}
	if out.Live || !reflect.DeepEqual(out.Labels, []string{"com.apple.mediad"}) || !reflect.DeepEqual(out.Exceptions, Exceptions{Keep: []string{"com.apple.mediad"}}) {
		t.Errorf("EnableServices = %+v, want mediad enabled offline and kept", out)
	}
	disabled, err := readDisabledStore(f.udid)
	if err != nil {
		t.Fatal(err)
	}
	if disabled["com.apple.mediad"] || !disabled["com.apple.cloudd"] {
		t.Errorf("store after enable = %v, want only mediad lifted", disabled)
	}
	for _, call := range xcrunCalls(t, f.log) {
		if call != "simctl list devices -j" {
			t.Errorf("enable on a shutdown simulator ran %q", call)
		}
	}
}

// disable stops a category at once, except the daemons another exception still
// needs: media keeps com.apple.shared running after cloud is dropped.
func TestDisableServicesKeepsWhatAnotherExceptionNeeds(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "com.apple.xpc")
	if err := writeExceptions(f.device(), Exceptions{Except: []string{"cloud", "media"}}); err != nil {
		t.Fatal(err)
	}

	out, err := DisableServices(context.Background(), f.udid, []string{"cloud"}, false, nil)
	if err != nil {
		t.Fatalf("DisableServices: %v", err)
	}
	stopped := []string{"com.apple.cloudd", "com.apple.lazy", "com.apple.xpc"}
	if !out.Live || !reflect.DeepEqual(out.Labels, stopped) || !reflect.DeepEqual(out.StillEnabled, []string{"com.apple.shared"}) {
		t.Errorf("DisableServices = %+v, want %v stopped and com.apple.shared kept", out, stopped)
	}
	want := Exceptions{Except: []string{"media"}}
	if got, err := ReadExceptions(f.device()); err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(out.Exceptions, want) {
		t.Errorf("exceptions = (%+v, %v), reported %+v; want %+v", got, err, out.Exceptions, want)
	}
	unloads, perLabel := f.calls(t)
	if len(unloads) != 1 || len(perLabel) != 2 {
		t.Errorf("%d unload and %d per-label spawns, want one batch plus the disable and bootout of com.apple.xpc", len(unloads), len(perLabel))
	}
	f.assertStopped(t, stopped)
	f.assertEnabled(t, []string{"com.apple.shared", "com.apple.mediad"}, "com.apple.shared", "com.apple.mediad")
}

// disable --all clears the record and turns every excepted daemon back off; on
// a shutdown simulator that is a store write, with no spawn.
func TestDisableServicesAllReturnsToTheProfile(t *testing.T) {
	useTestCategories(t)
	useTempStoreRoot(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	t.Setenv("FAKE_STATE", "Shutdown")
	if err := writeExceptions(f.device(), Exceptions{Except: []string{"cloud"}, Keep: []string{"com.apple.mediad"}}); err != nil {
		t.Fatal(err)
	}

	out, err := DisableServices(context.Background(), f.udid, nil, true, nil)
	if err != nil {
		t.Fatalf("DisableServices --all: %v", err)
	}
	if out.Live || !reflect.DeepEqual(out.Labels, labels) || len(out.StillEnabled) != 0 || !out.Exceptions.IsEmpty() {
		t.Errorf("DisableServices --all = %+v, want every label disabled offline and no exceptions", out)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.data), exceptionsFileName)); !os.IsNotExist(err) {
		t.Errorf("--all left the exceptions file behind (%v)", err)
	}
	disabled, err := readDisabledStore(f.udid)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range labels {
		if !disabled[l] {
			t.Errorf("%s is not disabled in the store", l)
		}
	}
	for _, call := range xcrunCalls(t, f.log) {
		if call != "simctl list devices -j" {
			t.Errorf("disable on a shutdown simulator ran %q", call)
		}
	}
}

// Argument mistakes fail before any simulator is looked up or recorded.
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
		{"enable nothing", func() error { _, err := EnableServices(ctx, udid, nil, nil); return err }, "name at least one category ID or managed daemon to enable"},
		{"enable unknown", func() error { _, err := EnableServices(ctx, udid, []string{"bogus"}, nil); return err }, `unknown category or managed daemon "bogus"`},
		{"disable nothing", func() error { _, err := DisableServices(ctx, udid, nil, false, nil); return err }, "or pass --all"},
		{"disable both", func() error { _, err := DisableServices(ctx, udid, []string{"cloud"}, true, nil); return err }, "not both"},
		{"disable required", func() error {
			_, err := DisableServices(ctx, udid, []string{"com.apple.required"}, false, nil)
			return err
		}, "com.apple.required is never disabled"},
	} {
		if err := tc.run(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A simulator mid-boot or mid-shutdown is refused before anything is recorded.
func TestServicesRefuseASimulatorInTransition(t *testing.T) {
	useTestCategories(t)
	f := newBatchFake(t, "iOS-26-5", nil, "")
	t.Setenv("FAKE_STATE", "Shutting Down")
	_, err := EnableServices(context.Background(), f.udid, []string{"cloud"}, nil)
	if err == nil || !strings.Contains(err.Error(), "must be booted or shut down") {
		t.Errorf("EnableServices while shutting down = %v, want a state error", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.data), exceptionsFileName)); !os.IsNotExist(err) {
		t.Errorf("a refused enable recorded exceptions (%v)", err)
	}
}

// watch slims each booting simulator with EnableSlimNoReboot, so that is where
// a simulator's exceptions must hold: excepted daemons found disabled start
// again, and the rest of the profile is applied around them.
func TestNoRebootRestartsTheSimulatorsExceptions(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	f.slimmed(t, labels...)
	if err := writeExceptions(f.device(), Exceptions{Except: []string{"cloud"}}); err != nil {
		t.Fatal(err)
	}

	var lines []string
	report := Reporter(func(s string) { lines = append(lines, s) })
	changed, err := EnableSlimNoReboot(context.Background(), "default", f.udid, Profile{}, report)
	if err != nil || !changed {
		t.Fatalf("EnableSlimNoReboot = (%t, %v), want a change", changed, err)
	}
	f.assertEnabled(t, cloudLabels, cloudLabels...)
	f.assertStopped(t, []string{"com.apple.mediad"})
	if out := strings.Join(lines, "\n"); !strings.Contains(out, "Keeping this simulator's exceptions enabled: cloud") {
		t.Errorf("report = %q, want the exceptions named", out)
	}
}

// On a stock boot the excepted daemons are never stopped in the first place.
func TestNoRebootLeavesTheSimulatorsExceptionsRunning(t *testing.T) {
	useTestCategories(t)
	labels := sortedLabels(SlimmableSet())
	f := newBatchFake(t, "iOS-26-5", labels, "")
	if err := writeExceptions(f.device(), Exceptions{Keep: []string{"com.apple.shared"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := EnableSlimNoReboot(context.Background(), "default", f.udid, Profile{}, nil); err != nil {
		t.Fatalf("EnableSlimNoReboot: %v", err)
	}
	unloads, perLabel := f.calls(t)
	for _, call := range append(unloads, perLabel...) {
		if strings.Contains(call, "com.apple.shared") {
			t.Errorf("slimming passed the kept com.apple.shared to launchctl: %s", call)
		}
	}
	f.assertEnabled(t, []string{"com.apple.shared"}, "com.apple.shared")
	f.assertStopped(t, []string{"com.apple.cloudd", "com.apple.lazy", "com.apple.mediad", "com.apple.xpc"})
}

// verify checks a simulator against its profile plus its exceptions, and names
// the exceptions it counted.
func TestVerifyProfileIncludesTheSimulatorsExceptions(t *testing.T) {
	useTestCategories(t)
	f := newBatchFake(t, "iOS-26-5", sortedLabels(SlimmableSet()), "")
	f.slimmed(t, "com.apple.mediad")
	ctx := context.Background()

	r, err := VerifyProfile(ctx, f.udid, Profile{})
	if err != nil || r.OK || r.Exceptions != nil {
		t.Fatalf("VerifyProfile without exceptions = (%+v, %v), want drift", r, err)
	}
	if err := writeExceptions(f.device(), Exceptions{Except: []string{"cloud"}}); err != nil {
		t.Fatal(err)
	}
	r, err = VerifyProfile(ctx, f.udid, Profile{})
	if err != nil || !r.OK || r.Exceptions == nil || !reflect.DeepEqual(*r.Exceptions, Exceptions{Except: []string{"cloud"}}) {
		t.Errorf("VerifyProfile with cloud excepted = (%+v, %v), want OK naming cloud", r, err)
	}
}

// `on` slims through ensure: a label the simulator keeps as an exception is
// lifted from the store although the profile disables it.
func TestEnsureKeepsTheSimulatorsExceptions(t *testing.T) {
	const udid = "00000000-0000-0000-0000-0000000000E7"
	useTempStoreRoot(t)
	logPath := fakeSimctl(t, udid, "iOS-26-5", "Shutdown")
	labels := twoSlimmableLabels(t)
	if err := writeDisabledStore(udid, labels, nil); err != nil {
		t.Fatal(err)
	}
	device := Device{UDID: udid, DataPath: filepath.Join(filepath.Dir(logPath), "data")}
	if err := writeExceptions(device, Exceptions{Keep: labels[:1]}); err != nil {
		t.Fatal(err)
	}

	if _, err := ensure(context.Background(), "default", udid, map[string]bool{labels[0]: true, labels[1]: true}, nil); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	disabled, err := readDisabledStore(udid)
	if err != nil {
		t.Fatal(err)
	}
	if disabled[labels[0]] || !disabled[labels[1]] {
		t.Errorf("store = %v, want %s kept enabled and %s disabled", disabled, labels[0], labels[1])
	}
}
