package cronstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"
)

const Schema = "bob-icu.cron-state.v1"

var jobIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

type State struct {
	Schema         string `json:"schema"`
	JobID          string `json:"job_id"`
	LastStartedAt  string `json:"last_started_at,omitempty"`
	LastFinishedAt string `json:"last_finished_at,omitempty"`
	LastSuccessAt  string `json:"last_success_at,omitempty"`
	LastExitCode   *int   `json:"last_exit_code,omitempty"`
	LastDurationMS *int64 `json:"last_duration_ms,omitempty"`
	Running        bool   `json:"running"`
}

func ValidJobID(jobID string) bool { return jobIDRE.MatchString(jobID) }

func Path(directory, jobID string) (string, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) == "/" {
		return "", errors.New("cron state directory must be an absolute non-root path")
	}
	if !ValidJobID(jobID) {
		return "", errors.New("invalid cron job_id")
	}
	return filepath.Join(filepath.Clean(directory), jobID+".json"), nil
}

func Read(path string) (State, error) {
	var state State
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return state, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return state, errors.New("open cron state")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return state, err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return state, errors.New("cron state must be a regular file no larger than 64 KiB")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, fmt.Errorf("decode cron state: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return state, errors.New("decode cron state: trailing data")
	}
	if state.Schema != Schema || !ValidJobID(state.JobID) {
		return state, errors.New("invalid cron state identity")
	}
	return state, nil
}

func WriteAtomic(directory string, state State) error {
	path, err := Path(directory, state.JobID)
	if err != nil {
		return err
	}
	if state.Schema != Schema {
		return errors.New("invalid cron state schema")
	}
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("cron state directory is not a directory")
	}
	tmp, err := os.CreateTemp(directory, "."+state.JobID+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0644); err != nil {
		return err
	}
	encoder := json.NewEncoder(tmp)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(state); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return err
	}
	ok = true
	return nil
}
