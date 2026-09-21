package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/thundersteff/bob-icu-client/internal/cronstate"
	"golang.org/x/sys/unix"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() { os.Exit(run(os.Args[1:], time.Now)) }

func run(args []string, now func() time.Time) int {
	flags := flag.NewFlagSet("bob-icu-cron-run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	jobID := flags.String("job", "", "stable cron job ID")
	stateDirectory := flags.String("state-dir", "/var/lib/bob-icu-cron", "cron state directory")
	showVersion := flags.Bool("version", false, "print version and exit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Printf("bob-icu-cron-run %s (%s) %s/%s\n", version, commit, runtime.GOOS, runtime.GOARCH)
		return 0
	}
	command := flags.Args()
	if !cronstate.ValidJobID(*jobID) || len(command) == 0 {
		fmt.Fprintln(os.Stderr, "usage: bob-icu-cron-run -job JOB_ID [ -state-dir DIR ] -- COMMAND [ARG ...]")
		return 2
	}
	path, err := cronstate.Path(*stateDirectory, *jobID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	lock, err := acquireJobLock(*stateDirectory, *jobID)
	if err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			fmt.Fprintln(os.Stderr, "cron job is already running")
			return 75
		}
		fmt.Fprintln(os.Stderr, "warning: cannot lock cron job; running without monitoring lock:", err)
	} else {
		defer lock.Close()
	}
	state := cronstate.State{Schema: cronstate.Schema, JobID: *jobID}
	if previous, err := cronstate.Read(path); err == nil {
		if previous.JobID != *jobID {
			fmt.Fprintln(os.Stderr, "warning: cron state job ID mismatch; replacing state")
		} else {
			state = previous
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(os.Stderr, "warning: existing cron state is invalid; replacing state")
	}
	started := now().UTC()
	state.Schema = cronstate.Schema
	state.JobID = *jobID
	state.LastStartedAt = started.Format(time.RFC3339Nano)
	state.Running = true
	if err := cronstate.WriteAtomic(*stateDirectory, state); err != nil {
		fmt.Fprintln(os.Stderr, "warning: cannot write cron start state; job will still run:", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	finished := now().UTC()
	exitCode := 0
	if err != nil {
		exitCode = 127
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() != nil {
			exitCode = 143
		}
	}
	duration := finished.Sub(started).Milliseconds()
	state.LastFinishedAt = finished.Format(time.RFC3339Nano)
	state.LastExitCode = &exitCode
	state.LastDurationMS = &duration
	state.Running = false
	if exitCode == 0 {
		state.LastSuccessAt = state.LastFinishedAt
	}
	if writeErr := cronstate.WriteAtomic(*stateDirectory, state); writeErr != nil {
		fmt.Fprintln(os.Stderr, "cannot write cron finish state:", writeErr)
		if exitCode == 0 {
			return 125
		}
	}
	return exitCode
}

func acquireJobLock(directory, jobID string) (*os.File, error) {
	lockPath := filepath.Join(filepath.Clean(directory), jobID+".lock")
	fd, err := unix.Open(lockPath, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("open cron lock")
	}
	return file, nil
}
