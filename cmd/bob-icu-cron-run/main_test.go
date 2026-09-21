package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thundersteff/bob-icu-client/internal/cronstate"
	"golang.org/x/sys/unix"
)

func sequenceClock(values ...time.Time) func() time.Time {
	index := 0
	return func() time.Time {
		value := values[index]
		if index < len(values)-1 {
			index++
		}
		return value
	}
}

func TestRunRecordsSuccessAndFailureWithoutCommandData(t *testing.T) {
	directory := t.TempDir()
	start := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	if code := run([]string{"-job", "backup", "-state-dir", directory, "--", "/bin/sh", "-c", "exit 0"}, sequenceClock(start, start.Add(2*time.Second))); code != 0 {
		t.Fatalf("success exit=%d", code)
	}
	path, _ := cronstate.Path(directory, "backup")
	state, err := cronstate.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Running || state.LastExitCode == nil || *state.LastExitCode != 0 || state.LastDurationMS == nil || *state.LastDurationMS != 2000 || state.LastSuccessAt == "" {
		t.Fatalf("unexpected success state: %#v", state)
	}
	previousSuccess := state.LastSuccessAt
	if code := run([]string{"-job", "backup", "-state-dir", directory, "--", "/bin/sh", "-c", "exit 7"}, sequenceClock(start.Add(time.Hour), start.Add(time.Hour+time.Second))); code != 7 {
		t.Fatalf("failure exit=%d", code)
	}
	state, err = cronstate.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastExitCode == nil || *state.LastExitCode != 7 || state.LastSuccessAt != previousSuccess {
		t.Fatalf("unexpected failure state: %#v", state)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || contains(string(data), "/bin/sh") || contains(string(data), "exit 7") {
		t.Fatalf("state leaks command data: %s", data)
	}
}

func TestRunRepairsInvalidExistingStateWithoutBlockingJob(t *testing.T) {
	directory := t.TempDir()
	path, _ := cronstate.Path(directory, "backup")
	if err := os.WriteFile(path, []byte(`{"schema":"wrong"}`), 0644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "executed")
	code := run([]string{"-job", "backup", "-state-dir", directory, "--", "/usr/bin/touch", marker}, time.Now)
	if code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("command was blocked by invalid monitoring state")
	}
	state, err := cronstate.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastExitCode == nil || *state.LastExitCode != 0 || state.Running {
		t.Fatalf("state was not repaired: %#v", state)
	}
}

func TestRunExecutesJobWhenMonitoringDirectoryIsUnavailable(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	marker := filepath.Join(t.TempDir(), "executed")
	code := run([]string{"-job", "backup", "-state-dir", directory, "--", "/usr/bin/touch", marker}, time.Now)
	if code != 125 {
		t.Fatalf("exit=%d, want monitoring failure 125", code)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("job was blocked by unavailable monitoring directory")
	}
}

func TestRunPreservesJobFailureWhenMonitoringDirectoryIsUnavailable(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing")
	code := run([]string{"-job", "backup", "-state-dir", directory, "--", "/bin/sh", "-c", "exit 7"}, time.Now)
	if code != 7 {
		t.Fatalf("exit=%d, want original job exit 7", code)
	}
}

func TestRunRejectsUnsafeJobID(t *testing.T) {
	if code := run([]string{"-job", "../bad", "-state-dir", t.TempDir(), "--", "/bin/true"}, time.Now); code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
}

func TestRunRejectsOverlappingExecution(t *testing.T) {
	directory := t.TempDir()
	lockPath := filepath.Join(directory, "backup.lock")
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC, 0644)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "executed")
	code := run([]string{"-job", "backup", "-state-dir", directory, "--", "/usr/bin/touch", marker}, time.Now)
	if code != 75 {
		t.Fatalf("exit=%d, want 75", code)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("overlapping command ran")
	}
}

func contains(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
