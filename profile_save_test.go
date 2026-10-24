package simslim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertProfileFile compares a saved profile byte for byte.
func assertProfileFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(data) != want {
		t.Errorf("%s =\n%s\nwant\n%s", filepath.Base(path), data, want)
	}
}

// A missing file starts from the default profile, the one `on` applies
// without --profile, which excepts and keeps nothing.
func TestSaveServicesCreatesTheProfile(t *testing.T) {
	useTestCategories(t)
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := SaveServices(path, []string{"cloud"}, []string{"com.apple.mediad"}, true); err != nil {
		t.Fatalf("SaveServices: %v", err)
	}
	assertProfileFile(t, path, "{\n  \"except\": [\n    \"cloud\"\n  ],\n  \"keep\": [\n    \"com.apple.mediad\"\n  ]\n}\n")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("profile mode = %v (%v), want 0644", info.Mode().Perm(), err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the profile (no temporary file left)", len(entries))
	}

	path = filepath.Join(t.TempDir(), "empty.json")
	if err := SaveServices(path, []string{"cloud"}, nil, false); err != nil {
		t.Fatalf("SaveServices disable on a missing file: %v", err)
	}
	assertProfileFile(t, path, "{}\n")
}

// Adding keeps the existing entries, their order, and the other fields; new
// entries go at the end, and nothing is listed twice.
func TestSaveServicesAddsWithoutDuplicates(t *testing.T) {
	useTestCategories(t)
	path := filepath.Join(t.TempDir(), "ci.json")
	if err := os.WriteFile(path, []byte(`{"name":"ci","description":"CI runs","except":["media"],"keep":["com.apple.xpc"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// media is already excepted, com.apple.shared already kept by media, and
	// com.apple.xpc already in keep: only com.apple.lazy is new.
	if err := SaveServices(path, []string{"media"}, []string{"com.apple.shared", "com.apple.xpc", "com.apple.lazy"}, true); err != nil {
		t.Fatalf("SaveServices: %v", err)
	}
	if err := SaveServices(path, []string{"cloud"}, nil, true); err != nil {
		t.Fatalf("SaveServices: %v", err)
	}
	want := `{
  "name": "ci",
  "description": "CI runs",
  "except": [
    "media",
    "cloud"
  ],
  "keep": [
    "com.apple.xpc",
    "com.apple.lazy"
  ]
}
`
	assertProfileFile(t, path, want)
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("profile mode = %v, want the original 0600", info.Mode().Perm())
	}
	if _, err := LoadSlimProfile(path); err != nil {
		t.Errorf("saved profile does not load: %v", err)
	}
}

// Removing drops only what is named and keeps the rest in order.
func TestSaveServicesRemoves(t *testing.T) {
	useTestCategories(t)
	path := filepath.Join(t.TempDir(), "dev.json")
	if err := os.WriteFile(path, []byte(`{"except":["media","cloud"],"keep":["com.apple.mediad","com.apple.xpc"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveServices(path, []string{"media"}, []string{"com.apple.mediad"}, false); err != nil {
		t.Fatalf("SaveServices: %v", err)
	}
	assertProfileFile(t, path, "{\n  \"except\": [\n    \"cloud\"\n  ],\n  \"keep\": [\n    \"com.apple.xpc\"\n  ]\n}\n")

	// A daemon a still-excepted category keeps cannot be removed through keep.
	err := SaveServices(path, nil, []string{"com.apple.cloudd"}, false)
	if err == nil || !strings.Contains(err.Error(), `through category "cloud"`) {
		t.Errorf("removing a daemon cloud keeps = %v, want an error naming cloud", err)
	}
}

// Saving the same change twice leaves the file untouched the second time.
func TestSaveServicesIsIdempotent(t *testing.T) {
	useTestCategories(t)
	path := filepath.Join(t.TempDir(), "dev.json")
	const original = `{"except": ["cloud"]}` // hand-formatted: a rewrite would reformat it
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, enable := range []bool{true, true} {
		if err := SaveServices(path, []string{"cloud"}, []string{"com.apple.lazy"}, enable); err != nil {
			t.Fatalf("SaveServices: %v", err)
		}
	}
	assertProfileFile(t, path, original)
	if err := SaveServices(path, []string{"media"}, nil, false); err != nil {
		t.Fatalf("SaveServices: %v", err)
	}
	assertProfileFile(t, path, original)
}

// A profile that would not load is refused, not rewritten.
func TestSaveServicesRejectsAnInvalidProfile(t *testing.T) {
	useTestCategories(t)
	for name, contents := range map[string]string{
		"unknown field":    `{"exceptt": ["cloud"]}`,
		"unknown category": `{"except": ["nope"]}`,
		"not JSON":         `{"except": [`,
	} {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := SaveServices(path, []string{"cloud"}, nil, true); err == nil {
			t.Errorf("%s: SaveServices accepted %s", name, contents)
		}
		assertProfileFile(t, path, contents)
	}
}

// An empty profile file is refused with an error that says so, not "EOF".
func TestEmptyProfileIsExplained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSlimProfile(path); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("LoadSlimProfile of an empty file = %v, want an is-empty error", err)
	}
	if err := SaveServices(path, []string{"siri"}, nil, true); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("SaveServices on an empty file = %v, want an is-empty error", err)
	}
}
