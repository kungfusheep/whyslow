package main

// record.go drives xctrace. every recording is time-limited, uses a fixed-period timer trigger and
// refuses to start on an already overloaded machine: PMU interrupts are system-wide, and an event
// trigger on something frequent (cache misses) can stall the whole machine.

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

//go:embed instrument/whyslow.instrpkg
var instrpkg []byte

const counterInstrument = "whyslow Counters"

// counterEvents is the pmc-events order in the instrument, i.e. the order of each sample's counters.
var counterEvents = []string{
	"cycles",
	"instructions",
	"l1d_load_misses",
	"l1d_store_misses",
	"l1i_misses",
	"branch_mispredicts",
	"dtlb_misses",
	"l1d_miss_wait_cycles",
}

const (
	evCycles = iota
	evInstructions
	evL1DLoadMiss
	evL1DStoreMiss
	evL1IMiss
	evMispredict
	evDTLBMiss
	evL1DWait
	numEvents
)

// instrumentPackage builds the embedded instrument package once per content hash into the user cache.
func instrumentPackage() (string, error) {
	sum := sha256.Sum256(instrpkg)
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "whyslow", hex.EncodeToString(sum[:6]))
	dst := filepath.Join(dir, "whyslow.instrdst")
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	src := filepath.Join(dir, "whyslow.instrpkg")
	if err := os.WriteFile(src, instrpkg, 0o644); err != nil {
		return "", err
	}
	pkgs, err := instrumentsPackagesDir()
	if err != nil {
		return "", err
	}
	out, err := exec.Command("xcrun", "instrumentbuilder", "-i", src, "-o", dst, "-l", "CPU Counters", "-L", pkgs).CombinedOutput()
	if _, statErr := os.Stat(dst); err != nil || statErr != nil {
		return "", fmt.Errorf("building instrument package: %v\n%s", err, out)
	}
	return dst, nil
}

func instrumentsPackagesDir() (string, error) {
	out, err := exec.Command("xcode-select", "-p").Output()
	if err != nil {
		return "", fmt.Errorf("xcode-select -p: %w (whyslow needs Xcode for xctrace)", err)
	}
	dev := strings.TrimSpace(string(out)) // .../Xcode.app/Contents/Developer
	dir := filepath.Join(filepath.Dir(dev), "Applications", "Instruments.app", "Contents", "Packages")
	if _, err := os.Stat(filepath.Join(dir, "CPU Counters.instrdst")); err != nil {
		return "", fmt.Errorf("CPU Counters instrument not found in %s", dir)
	}
	return dir, nil
}

// target is what gets launched under xctrace.
type target struct {
	argv []string
	dir  string // working directory, "" for the current one
}

type recording struct {
	trace  string
	stdout string
}

// recordCounters samples stacks plus PMU counters on a 1ms timer, with Time Profiler alongside: its
// frames carry each image's load address, which the counter samples' raw addresses need.
func recordCounters(ctx context.Context, t target, out string, limit time.Duration) (recording, error) {
	pkg, err := instrumentPackage()
	if err != nil {
		return recording{}, err
	}
	return record(ctx, t, out, "counters", limit, "--package", pkg, "--instrument", counterInstrument, "--instrument", "Time Profiler")
}

// recordTopDown runs Instruments' own CPU Bottlenecks mode for the useful/delivery/processing/discarded split.
func recordTopDown(ctx context.Context, t target, out string, limit time.Duration) (recording, error) {
	return record(ctx, t, out, "topdown", limit, "--template", "CPU Counters")
}

// recordEvents runs one of Instruments' event-sampling counting modes (e.g. l1d_miss_sampling): each
// sample is a PMU interrupt after a fixed number of events, carrying the exact pc that caused it.
// Instruments picks the thresholds, which keeps the interrupt rate safe.
func recordEvents(ctx context.Context, t target, out, name, mode string, limit time.Duration) (recording, error) {
	opts, err := countingModeOptions(mode, filepath.Join(out, name+".json"))
	if err != nil {
		return recording{}, err
	}
	return record(ctx, t, out, name, limit, "--template", "CPU Counters", "--instrument", "Time Profiler", "--recording-options", opts)
}

// countingModeOptions writes CPU Counters recording options selecting a counting mode. xctrace
// rejects partial option files, so it starts from the full defaults this Xcode reports.
func countingModeOptions(mode, path string) (string, error) {
	out, err := exec.Command("xctrace", "record", "--template", "CPU Counters", "--show-recording-options", "--launch", "--", "/usr/bin/true").Output()
	if err != nil {
		return "", fmt.Errorf("reading CPU Counters options: %w", err)
	}
	var opts map[string]map[string]any
	if err := json.Unmarshal(out, &opts); err != nil {
		return "", fmt.Errorf("parsing CPU Counters options: %w", err)
	}
	cc, ok := opts["CPU Counters"]
	if !ok {
		return "", fmt.Errorf("no CPU Counters options from xctrace")
	}
	cc["selectedCountingMode"] = map[string]string{"analysisMode": "bottleneck", "countingMode": mode}
	b, err := json.Marshal(opts)
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, b, 0o644)
}

func record(ctx context.Context, t target, out, name string, limit time.Duration, args ...string) (recording, error) {
	if err := checkLoad(); err != nil {
		return recording{}, err
	}
	rec := recording{trace: filepath.Join(out, name+".trace"), stdout: filepath.Join(out, name+".stdout")}
	argv := append([]string{"record"}, args...)
	argv = append(argv, "--time-limit", fmt.Sprintf("%dms", limit.Milliseconds()), "--output", rec.trace,
		"--target-stdout", rec.stdout, "--launch", "--")
	argv = append(argv, t.argv...)

	cmd := exec.CommandContext(ctx, "xctrace", argv...)
	// an interrupt makes xctrace stop the target and finalize the trace; a kill would leave both behind
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	cmd.Dir = t.dir
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return rec, fmt.Errorf("xctrace record: %w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	if _, err := os.Stat(rec.trace); err != nil {
		return rec, fmt.Errorf("xctrace produced no trace:\n%s", strings.TrimSpace(stderr.String()))
	}
	return rec, nil
}

// checkLoad refuses to record when the 1-minute load average already exceeds the core count.
func checkLoad() error {
	// vm.loadavg is a binary struct that syscall.Sysctl would truncate; sysctl(8) prints "{ 1.74 5.26 4.88 }"
	raw, err := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	if err != nil {
		return nil // not fatal: just skip the check
	}
	fields := strings.Fields(strings.Trim(strings.TrimSpace(string(raw)), "{} "))
	if len(fields) == 0 {
		return nil
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return nil
	}
	if load > float64(runtime.NumCPU()) {
		return fmt.Errorf("load average is %.1f on %d cores; refusing to record until the machine settles", load, runtime.NumCPU())
	}
	return nil
}
