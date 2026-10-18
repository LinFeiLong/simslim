package simslim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Exceptions are the categories and individual daemons one simulator keeps
// enabled whatever profile slims it. `simslim enable` adds to them and `simslim
// disable` takes them away; `on`, `watch`, and `verify` merge them into every
// profile, so a simulator that had iCloud re-enabled keeps it through reboots
// and re-slims until its exceptions are cleared. `off` leaves them recorded.
//
// They are per-simulator state, like the launchd overrides they qualify, and
// sit in the simulator's own device directory beside its device.plist rather
// than in any global file: deleting the simulator deletes them, and they move
// with its device set. An erase keeps them, as it keeps device.plist: simctl
// only resets the data directory. The arrays mirror a profile file's "except"
// and "keep".
type Exceptions struct {
	Except []string `json:"except,omitempty"` // category IDs kept fully enabled
	Keep   []string `json:"keep,omitempty"`   // individual launchd labels kept enabled
}

// exceptionsFileName is the file in a simulator's device directory (the parent
// of its data directory) that holds its exceptions.
const exceptionsFileName = "simslim-exceptions.json"

// String lists the category IDs, then the individual labels.
func (e Exceptions) String() string {
	return strings.Join(append(slices.Clone(e.Except), e.Keep...), ", ")
}

// IsEmpty reports whether the simulator keeps nothing beyond its profile.
func (e Exceptions) IsEmpty() bool {
	return len(e.Except) == 0 && len(e.Keep) == 0
}

// labels returns the managed labels the exceptions keep enabled. An entry this
// build does not know, written by a newer simslim or naming a daemon since
// dropped from the allowlist, keeps nothing here but stays in the file.
func (e Exceptions) labels() map[string]bool {
	managed := managedSet()
	kept := map[string]bool{}
	for _, id := range e.Except {
		if c, ok := CategoryByID(id); ok {
			for _, l := range c.Labels {
				kept[l] = true
			}
		}
	}
	for _, l := range e.Keep {
		if managed[l] {
			kept[l] = true
		}
	}
	return kept
}

// WithExceptions returns the profile that also leaves the exceptions enabled:
// the one `on`, `watch`, and `verify` apply to a simulator that has them.
func (p Profile) WithExceptions(e Exceptions) Profile {
	out := Profile{ExceptCategories: map[string]bool{}, Keep: map[string]bool{}}
	for id := range p.ExceptCategories {
		out.ExceptCategories[id] = true
	}
	for l := range p.Keep {
		out.Keep[l] = true
	}
	for _, id := range e.Except {
		if isCategory(id) {
			out.ExceptCategories[id] = true
		}
	}
	for _, l := range e.Keep {
		out.Keep[l] = true
	}
	return out
}

// apply drops the labels the exceptions keep enabled from a desired-disabled
// set. A label shared by several categories stays enabled when any excepted
// category lists it, exactly as Profile.Desired treats --except.
func (e Exceptions) apply(desired map[string]bool) map[string]bool {
	kept := e.labels()
	if len(kept) == 0 {
		return desired
	}
	out := make(map[string]bool, len(desired))
	for l, v := range desired {
		if !kept[l] {
			out[l] = v
		}
	}
	return out
}

// with returns the exceptions plus the given categories and labels.
func (e Exceptions) with(categories, labels []string) Exceptions {
	return Exceptions{Except: union(e.Except, categories), Keep: union(e.Keep, labels)}
}

// without returns the exceptions minus the given categories and labels.
func (e Exceptions) without(categories, labels []string) Exceptions {
	return Exceptions{Except: minus(e.Except, categories), Keep: minus(e.Keep, labels)}
}

func union(a, b []string) []string {
	if len(a)+len(b) == 0 {
		return nil
	}
	set := map[string]bool{}
	for _, s := range append(slices.Clone(a), b...) {
		set[s] = true
	}
	return sortedKeys(set)
}

func minus(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// exceptionsPath returns where the simulator keeps its exceptions, or "" when
// its device directory is unknown (a Device built by hand rather than listed).
func exceptionsPath(d Device) string {
	if d.DataPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(d.DataPath), exceptionsFileName)
}

// ReadExceptions returns the simulator's exceptions. A simulator that never had
// any has no file, which is the same as none. A file that cannot be parsed is
// an error rather than "no exceptions": slimming past it would silently stop
// the daemons someone asked to keep.
func ReadExceptions(d Device) (Exceptions, error) {
	path := exceptionsPath(d)
	if path == "" {
		return Exceptions{}, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Exceptions{}, nil
	}
	if err != nil {
		return Exceptions{}, fmt.Errorf("read simulator exceptions: %w", err)
	}
	var e Exceptions
	if err := json.Unmarshal(data, &e); err != nil {
		return Exceptions{}, fmt.Errorf("parse simulator exceptions %s: %w (fix or delete the file)", path, err)
	}
	sort.Strings(e.Except)
	sort.Strings(e.Keep)
	return e, nil
}

// writeExceptions records the simulator's exceptions, atomically so a
// concurrent `watch` never reads half a file; no exceptions removes the file.
func writeExceptions(d Device, e Exceptions) error {
	path := exceptionsPath(d)
	if path == "" {
		return fmt.Errorf("cannot record exceptions for %s: its device directory is unknown", d.UDID)
	}
	if e.IsEmpty() {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("clear simulator exceptions: %w", err)
		}
		return nil
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), exceptionsFileName+".*")
	if err != nil {
		return fmt.Errorf("write simulator exceptions: %w", err)
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write simulator exceptions: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write simulator exceptions: %w", err)
	}
	if err := os.Chmod(temp.Name(), 0o644); err != nil {
		return fmt.Errorf("write simulator exceptions: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("write simulator exceptions: %w", err)
	}
	return nil
}

// resolveServiceIDs sorts the IDs `enable` and `disable` accept into category
// IDs and individual managed labels, rejecting anything else.
func resolveServiceIDs(ids []string) (categories, labels []string, err error) {
	managed := managedSet()
	seenCategory, seenLabel := map[string]bool{}, map[string]bool{}
	for _, id := range ids {
		switch {
		case seenCategory[id] || seenLabel[id]:
		case isCategory(id):
			seenCategory[id] = true
			categories = append(categories, id)
		case managed[id]:
			seenLabel[id] = true
			labels = append(labels, id)
		default:
			return nil, nil, fmt.Errorf("unknown category or managed daemon %q (see `simslim profiles`)", id)
		}
	}
	sort.Strings(categories)
	sort.Strings(labels)
	return categories, labels, nil
}

func isCategory(id string) bool {
	_, ok := CategoryByID(id)
	return ok
}

// coveredLabels is every managed label the categories and labels name.
func coveredLabels(categories, labels []string) map[string]bool {
	return Exceptions{Except: categories, Keep: labels}.labels()
}
