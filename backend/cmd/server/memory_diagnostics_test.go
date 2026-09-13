package main

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMemoryDiagnostics(t *testing.T) {
	t.Setenv("SUB2API_MEMORY_DIAGNOSTICS_DIR", "")
	startMemoryDiagnostics()() // Disabled by default, without installing handlers.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "private-link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := writeMemorySnapshot(link); err == nil {
		t.Fatal("symlink diagnostics directory accepted")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := writeMemorySnapshot(dir); err == nil {
		t.Fatal("non-private diagnostics directory accepted")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := writeMemorySnapshot(dir); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("snapshots: %d, error: %v", len(entries), err)
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("snapshot directory not private: %v", err)
		}
		for _, name := range []string{"memory.json", "heap.pprof"} {
			info, err := os.Stat(filepath.Join(path, name))
			if err != nil || info.Mode().Perm() != 0600 || info.Size() == 0 {
				t.Fatalf("invalid private output %s: %v", name, err)
			}
		}
		raw, err := os.ReadFile(filepath.Join(path, "memory.json"))
		var stats struct{ PID, Goroutines int }
		if err != nil || json.Unmarshal(raw, &stats) != nil || stats.PID != os.Getpid() || stats.Goroutines == 0 {
			t.Fatalf("invalid memory stats: %v", err)
		}
		f, err := os.Open(filepath.Join(path, "heap.pprof"))
		if err != nil {
			t.Fatal(err)
		}
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, gz)
		gz.Close()
		f.Close()
		if err != nil || n == 0 {
			t.Fatalf("invalid heap profile: %v", err)
		}
	}
	t.Setenv("SUB2API_MEMORY_DIAGNOSTICS_DIR", dir)
	stop := startMemoryDiagnostics()
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries, err = os.ReadDir(dir)
		if err == nil && len(entries) == 3 {
			return // Deferred stop waits for the complete, signal-triggered sample.
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("SIGUSR2 did not produce a snapshot")
}
