package simslim

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// useTestCategories swaps in a small catalog: cloud and media share
// com.apple.shared, and media lists a compatibility service that is managed but
// never slimmable.
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

// The exceptions sit beside the simulator's data directory, are read back
// sorted, and an empty set removes the file rather than leaving an empty one.
func TestExceptionsLiveInTheDeviceDirectory(t *testing.T) {
	dir := t.TempDir()
	d := Device{UDID: "00000000-0000-0000-0000-0000000000C1", DataPath: filepath.Join(dir, "data")}
	path := filepath.Join(dir, exceptionsFileName)

	if e, err := ReadExceptions(d); err != nil || !e.IsEmpty() {
		t.Fatalf("ReadExceptions without a file = (%+v, %v), want none", e, err)
	}
	if err := writeExceptions(d, Exceptions{Except: []string{"media", "cloud"}, Keep: []string{"com.apple.mediad"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("no exceptions file beside the data directory: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("exceptions file mode = %v, want 0644", info.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("device directory holds %d entries after a write, want only the exceptions file (%v)", len(entries), err)
	}
	want := Exceptions{Except: []string{"cloud", "media"}, Keep: []string{"com.apple.mediad"}}
	if got, err := ReadExceptions(d); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ReadExceptions = (%+v, %v), want %+v", got, err, want)
	}

	for range 2 { // clearing twice is not an error
		if err := writeExceptions(d, Exceptions{}); err != nil {
			t.Fatalf("clear exceptions: %v", err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("clearing the exceptions left %s behind (%v)", path, err)
	}
}

// A Device built by hand has no device directory: it has no exceptions, and
// recording some is an error rather than a write to the wrong place.
func TestExceptionsNeedADeviceDirectory(t *testing.T) {
	d := Device{UDID: "00000000-0000-0000-0000-0000000000C2"}
	if e, err := ReadExceptions(d); err != nil || !e.IsEmpty() {
		t.Errorf("ReadExceptions without a data path = (%+v, %v), want none", e, err)
	}
	err := writeExceptions(d, Exceptions{Except: []string{"cloud"}})
	if err == nil || !strings.Contains(err.Error(), "device directory is unknown") {
		t.Errorf("writeExceptions without a data path = %v, want an unknown-directory error", err)
	}
}

// Slimming past a file it cannot parse would stop daemons someone asked to
// keep, so a corrupt file is an error, not "no exceptions".
func TestReadExceptionsRejectsACorruptFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, exceptionsFileName), []byte(`{"except": ["cloud"`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadExceptions(Device{DataPath: filepath.Join(dir, "data")})
	if err == nil || !strings.Contains(err.Error(), "fix or delete the file") {
		t.Errorf("ReadExceptions on a corrupt file = %v, want a parse error", err)
	}
}

// An entry this build does not know, from a newer simslim or for a daemon since
// dropped from the allowlist, keeps nothing enabled but stays recorded.
func TestExceptionsIgnoreButKeepUnknownEntries(t *testing.T) {
	useTestCategories(t)
	e := Exceptions{Except: []string{"cloud", "future"}, Keep: []string{"com.apple.unmanaged"}}
	want := []string{"com.apple.cloudd", "com.apple.lazy", "com.apple.shared", "com.apple.xpc"}
	if got := sortedKeys(e.labels()); !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want only cloud's %v", got, want)
	}
	if p := (Profile{}).WithExceptions(e); p.ExceptCategories["future"] {
		t.Error("WithExceptions excepted a category this build does not know")
	}
	if got := e.without([]string{"cloud"}, nil); !reflect.DeepEqual(got, Exceptions{Except: []string{"future"}, Keep: []string{"com.apple.unmanaged"}}) {
		t.Errorf("without(cloud) = %+v, want the unknown entries kept", got)
	}
}

// A label two categories share stays enabled while either is an exception,
// exactly as --except treats it, and WithExceptions agrees with apply.
func TestExceptionsFollowTheExceptOverlapRule(t *testing.T) {
	useTestCategories(t)
	e := Exceptions{Except: []string{"media"}}
	want := []string{"com.apple.cloudd", "com.apple.lazy", "com.apple.xpc"}
	if got := sortedKeys(e.apply(Profile{}.Desired())); !reflect.DeepEqual(got, want) {
		t.Errorf("apply(media) = %v, want %v", got, want)
	}
	if got := sortedKeys(Profile{}.WithExceptions(e).Desired()); !reflect.DeepEqual(got, want) {
		t.Errorf("WithExceptions(media).Desired = %v, want %v", got, want)
	}

	p := Profile{ExceptCategories: map[string]bool{"cloud": true}}
	keep := Exceptions{Keep: []string{"com.apple.mediad"}}
	if got := keep.apply(p.Desired()); len(got) != 0 {
		t.Errorf("profile except cloud plus kept mediad still disables %v", sortedKeys(got))
	}
	if got := p.WithExceptions(keep).Desired(); len(got) != 0 {
		t.Errorf("WithExceptions(keep mediad).Desired = %v, want nothing", sortedKeys(got))
	}
	if len(p.Keep) != 0 || len(p.ExceptCategories) != 1 {
		t.Errorf("WithExceptions changed the profile it extends: %+v", p)
	}
}

func TestExceptionsWithAndWithout(t *testing.T) {
	e := Exceptions{}.with([]string{"media", "cloud"}, []string{"com.apple.b"})
	e = e.with([]string{"cloud"}, []string{"com.apple.a"})
	want := Exceptions{Except: []string{"cloud", "media"}, Keep: []string{"com.apple.a", "com.apple.b"}}
	if !reflect.DeepEqual(e, want) {
		t.Fatalf("with = %+v, want %+v", e, want)
	}
	if got := e.String(); got != "cloud, media, com.apple.a, com.apple.b" {
		t.Errorf("String = %q", got)
	}
	e = e.without([]string{"cloud", "absent"}, []string{"com.apple.a", "com.apple.b"})
	if !reflect.DeepEqual(e, Exceptions{Except: []string{"media"}}) {
		t.Errorf("without = %+v, want only media", e)
	}
	if !e.without([]string{"media"}, nil).IsEmpty() {
		t.Error("removing the last exception left some behind")
	}
}

func TestResolveServiceIDs(t *testing.T) {
	useTestCategories(t)
	categories, labels, err := resolveServiceIDs([]string{"media", "com.apple.required", "cloud", "media", "com.apple.cloudd"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(categories, []string{"cloud", "media"}) || !reflect.DeepEqual(labels, []string{"com.apple.cloudd", "com.apple.required"}) {
		t.Errorf("resolveServiceIDs = (%v, %v), want categories [cloud media] and labels [com.apple.cloudd com.apple.required]", categories, labels)
	}
	if _, _, err := resolveServiceIDs([]string{"cloud", "com.apple.unmanaged"}); err == nil || !strings.Contains(err.Error(), `"com.apple.unmanaged"`) {
		t.Errorf("an unmanaged label resolved: %v", err)
	}
}
