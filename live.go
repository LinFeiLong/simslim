package simslim

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// EnableServices re-enables categories (IDs from `simslim profiles`) and
// individual managed daemons on one simulator and records them as its
// exceptions, so `on` and `watch` keep them enabled from then on. A booted
// simulator gets them back at once, without a reboot: each disable override is
// lifted and each job loaded into the running boot session. A shutdown one has
// the overrides lifted in the store launchd_sim reads when it starts, so it
// boots with them; a runtime that cannot persist overrides boots stock anyway.
func EnableServices(ctx context.Context, udid string, ids []string, report Reporter) (ServicesOutput, error) {
	categories, labels, err := resolveServiceIDs(ids)
	if err != nil {
		return ServicesOutput{}, err
	}
	if len(categories)+len(labels) == 0 {
		return ServicesOutput{}, fmt.Errorf("name at least one category ID or managed daemon to enable (see `simslim profiles`)")
	}
	d, unlock, err := lockedTarget(ctx, udid, report)
	if err != nil {
		return ServicesOutput{}, err
	}
	defer unlock()
	exceptions, err := ReadExceptions(d)
	if err != nil {
		return ServicesOutput{}, err
	}
	exceptions = exceptions.with(categories, labels)
	// Recorded first: if the live part fails, the next `on` or `watch` still
	// honours the request, and re-running this command finishes the job.
	if err := writeExceptions(d, exceptions); err != nil {
		return ServicesOutput{}, err
	}
	targets := sortedKeys(coveredLabels(categories, labels))
	out := ServicesOutput{Action: "enable", UDID: d.UDID, Live: d.State == "Booted", Labels: targets, Exceptions: exceptions}
	if !out.Live {
		changeOffline(d, nil, targets, report)
		return out, nil
	}
	if err := BootAndWait(ctx, d.Set, d.UDID); err != nil {
		return out, err
	}
	report.report(fmt.Sprintf("Starting %d background services without a reboot...", len(targets)))
	out.NotStarted, err = startServices(ctx, d, targets, report)
	return out, err
}

// DisableServices is the inverse of EnableServices: it drops categories and
// daemons from the simulator's exceptions and stops their daemons at once —
// each one booted out of the running boot session with its disable override
// set, no reboot — or, on a shutdown simulator, disables them in the store it
// boots from. It stops what it is told whatever the profile says. A daemon that
// a remaining exception still lists, or that the platform needs, stays
// enabled. With all, every exception goes and its daemons are stopped, which
// returns the simulator to plain profile slimming.
func DisableServices(ctx context.Context, udid string, ids []string, all bool, report Reporter) (ServicesOutput, error) {
	categories, labels, err := resolveServiceIDs(ids)
	if err != nil {
		return ServicesOutput{}, err
	}
	named := len(categories)+len(labels) > 0
	switch {
	case all && named:
		return ServicesOutput{}, fmt.Errorf("pass category IDs and daemons, or --all, not both")
	case !all && !named:
		return ServicesOutput{}, fmt.Errorf("name at least one category ID or managed daemon to disable, or pass --all to clear every exception (see `simslim profiles`)")
	}
	slimmable := SlimmableSet()
	for _, l := range labels {
		if !slimmable[l] {
			return ServicesOutput{}, fmt.Errorf("%s is never disabled: simslim keeps it running for compatibility (see `simslim profiles`)", l)
		}
	}
	d, unlock, err := lockedTarget(ctx, udid, report)
	if err != nil {
		return ServicesOutput{}, err
	}
	defer unlock()
	exceptions, err := ReadExceptions(d)
	if err != nil {
		return ServicesOutput{}, err
	}
	remaining := exceptions.without(categories, labels)
	if all {
		categories, labels, remaining = exceptions.Except, exceptions.Keep, Exceptions{}
	}
	covered := coveredLabels(categories, labels)
	allowed := desiredForDevice(d, covered)
	kept := remaining.labels()
	stop, still := []string{}, []string(nil)
	for _, l := range sortedKeys(covered) {
		if allowed[l] && slimmable[l] && !kept[l] {
			stop = append(stop, l)
		} else {
			still = append(still, l)
		}
	}
	if err := writeExceptions(d, remaining); err != nil {
		return ServicesOutput{}, err
	}
	out := ServicesOutput{Action: "disable", UDID: d.UDID, Live: d.State == "Booted", Labels: stop, StillEnabled: still, Exceptions: remaining}
	if len(stop) == 0 {
		return out, nil
	}
	if !out.Live {
		changeOffline(d, stop, nil, report)
		return out, nil
	}
	if err := BootAndWait(ctx, d.Set, d.UDID); err != nil {
		return out, err
	}
	report.report(fmt.Sprintf("Stopping %d background services without a reboot...", len(stop)))
	return out, stopServices(ctx, d, stop, report)
}

// serviceTarget resolves the exact simulator `enable` and `disable` act on, so
// a simctl alias such as "all" can never fan out across devices.
func serviceTarget(ctx context.Context, udid string) (Device, error) {
	d, err := FindDevice(ctx, udid, "")
	if err != nil {
		return Device{}, err
	}
	if d.State != "Booted" && d.State != "Shutdown" {
		return Device{}, fmt.Errorf("simulator must be booted or shut down to change its services (it is %s)", d.State)
	}
	return d, nil
}

// lockedTarget resolves the simulator and takes its lock, resolving it again
// after a wait: the watch pass or `on` that held the lock may have rebooted or
// shut it down.
func lockedTarget(ctx context.Context, udid string, report Reporter) (Device, func(), error) {
	d, err := serviceTarget(ctx, udid)
	if err != nil {
		return Device{}, nil, err
	}
	unlock, waited, err := lockSimulator(ctx, d, report)
	if err != nil {
		return Device{}, nil, err
	}
	if !waited {
		return d, unlock, nil
	}
	if d, err = serviceTarget(ctx, udid); err != nil {
		unlock()
		return Device{}, nil, err
	}
	return d, unlock, nil
}

// changeOffline applies transitions to a shutdown simulator by writing the
// store launchd_sim reads when it boots. A runtime that cannot persist
// overrides boots stock, so there is nothing to write. The store is best
// effort, as everywhere: if it cannot be written, the recorded exceptions
// still steer the next `on` or `watch` once the simulator is up.
func changeOffline(d Device, toDisable, toEnable []string, report Reporter) {
	if !d.SupportsPersistentOverrides() {
		return
	}
	current, err := readDisabledStore(d.UDID)
	if err == nil {
		var dis, en []string
		for _, l := range toDisable {
			if !current[l] {
				dis = append(dis, l)
			}
		}
		for _, l := range toEnable {
			if current[l] {
				en = append(en, l)
			}
		}
		if len(dis)+len(en) == 0 {
			return
		}
		err = writeDisabledStore(d.UDID, dis, en)
	}
	if err != nil {
		report.report(fmt.Sprintf("Could not update the launchd overrides while the simulator is off (%v); `simslim on` or `simslim watch` applies the change once it boots.", err))
	}
}

// startServices lifts the disable override of each label and loads its job into
// the running boot session, so the daemons are back without a reboot. One
// `launchctl load -w` per chunk handles every label whose job plist is found;
// what is still disabled afterwards gets a per-label `launchctl enable`, and
// what is still not loaded a per-label bootstrap. A label without a plist (an
// XPC service, for example) only needs its override lifted: launchd starts it
// on demand. Labels launchd re-enabled but would not load in this boot session
// are returned; they start at the next boot. A label left disabled is an error.
func startServices(ctx context.Context, d Device, labels []string, report Reporter) (notStarted []string, err error) {
	set, udid := d.Set, d.UDID
	disabled, loaded, err := serviceState(ctx, set, udid)
	if err != nil {
		return nil, err
	}
	var candidates []string
	for _, l := range labels {
		if disabled[l] || !loaded[l] {
			candidates = append(candidates, l)
		}
	}
	plists, haveRoot := map[string]string{}, false
	if d.RuntimeID != "" && len(candidates) > 0 {
		if root, err := runtimeRoot(ctx, d.RuntimeID); err == nil {
			plists, haveRoot = launchdJobPlists(ctx, root, candidates, readPlistLabel), true
		}
	}
	// Without the RuntimeRoot nothing tells a missing plist from an unknown
	// one, so every unloaded label counts as one launchd should load.
	loadable := func(l string) bool { return plists[l] != "" || !haveRoot }
	var pending []string
	for _, l := range candidates {
		if disabled[l] || loadable(l) {
			pending = append(pending, l)
		}
	}
	if len(pending) == 0 {
		report.report(fmt.Sprintf("  all %d services were already running", len(labels)))
		return nil, nil
	}
	var paths []string
	for _, l := range pending {
		if p := plists[l]; p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) > 0 {
		loadJobs(ctx, set, udid, paths)
		if disabled, loaded, err = serviceState(ctx, set, udid); err != nil {
			return nil, err
		}
		report.report(fmt.Sprintf("  %d/%d services started in one batch", len(pending)-len(stillToStart(pending, disabled, loaded, loadable)), len(pending)))
	}
	var toEnable []string
	unloaded := map[string]string{}
	for _, l := range pending {
		if disabled[l] {
			toEnable = append(toEnable, l)
		}
		if p := plists[l]; p != "" && !loaded[l] {
			unloaded[l] = p
		}
	}
	if len(toEnable) > 0 {
		if err := applyDelta(ctx, set, udid, nil, toEnable, "disable", report); err != nil {
			return nil, err
		}
	}
	if len(unloaded) > 0 {
		bootstrapJobs(ctx, set, udid, unloaded)
	}
	if len(toEnable)+len(unloaded) > 0 {
		if disabled, loaded, err = serviceState(ctx, set, udid); err != nil {
			return nil, err
		}
	}
	var still []string
	for _, l := range pending {
		switch {
		case disabled[l]:
			still = append(still, l)
		case !loaded[l] && loadable(l):
			notStarted = append(notStarted, l)
		}
	}
	if len(still) > 0 {
		return notStarted, fmt.Errorf("%d of %d services are still disabled: %s", len(still), len(pending), strings.Join(still, ", "))
	}
	return notStarted, nil
}

// stillToStart keeps the labels that are still disabled, or still not loaded
// although launchd could load them.
func stillToStart(labels []string, disabled, loaded map[string]bool, loadable func(string) bool) []string {
	var rest []string
	for _, l := range labels {
		if disabled[l] || !loaded[l] && loadable(l) {
			rest = append(rest, l)
		}
	}
	return rest
}

// serviceState reads the disable overrides and the loaded jobs together.
func serviceState(ctx context.Context, set, udid string) (disabled, loaded map[string]bool, err error) {
	if disabled, err = readDisabled(ctx, set, udid); err != nil {
		return nil, nil, err
	}
	if loaded, err = loadedLabels(ctx, set, udid); err != nil {
		return nil, nil, err
	}
	return disabled, loaded, nil
}

// stopServices disables each label and boots its job out of the running boot
// session: one `launchctl unload -w` per chunk first, then a per-label disable
// and bootout for whatever the batch missed. Every override is read back.
func stopServices(ctx context.Context, d Device, labels []string, report Reporter) error {
	remaining := unloadInBatch(ctx, d.Set, d.UDID, d.RuntimeID, labels, readPlistLabel, report)
	if err := applyDelta(ctx, d.Set, d.UDID, remaining, nil, liveDisable, report); err != nil {
		return err
	}
	after, err := readDisabled(ctx, d.Set, d.UDID)
	if err != nil {
		return err
	}
	missing := 0
	for _, l := range labels {
		if !after[l] {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("%d of %d disable overrides did not take", missing, len(labels))
	}
	return nil
}

// splitByExceptions separates the labels disabled beyond a profile into those
// the simulator's exceptions keep enabled, which must start again, and the rest.
func splitByExceptions(extra []string, kept map[string]bool) (restart, beyond []string) {
	for _, l := range extra {
		if kept[l] {
			restart = append(restart, l)
		} else {
			beyond = append(beyond, l)
		}
	}
	sort.Strings(restart)
	sort.Strings(beyond)
	return restart, beyond
}
