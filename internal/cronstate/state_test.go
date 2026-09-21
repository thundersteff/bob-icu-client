package cronstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAtomicAndRead(t *testing.T) {
	directory := t.TempDir()
	exit := 0
	state := State{Schema: Schema, JobID: "job", LastExitCode: &exit}
	if err := WriteAtomic(directory, state); err != nil {
		t.Fatal(err)
	}
	path, _ := Path(directory, "job")
	loaded, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.JobID != "job" || loaded.LastExitCode == nil || *loaded.LastExitCode != 0 {
		t.Fatalf("state=%#v", loaded)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("mode=%o", info.Mode().Perm())
	}
	if matches, _ := filepath.Glob(filepath.Join(directory, ".job.tmp-*")); len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestReadRejectsSymlinkAndUnknownFields(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte(`{"schema":"bob-icu.cron-state.v1","job_id":"job"}`), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "job.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link); err == nil {
		t.Fatal("symlink was accepted")
	}
	if err := os.WriteFile(target, []byte(`{"schema":"bob-icu.cron-state.v1","job_id":"job","surprise":true}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(target); err == nil {
		t.Fatal("unknown field was accepted")
	}
}
