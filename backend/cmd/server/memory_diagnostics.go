package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"
)

// Opt-in, local-only incident diagnostics. No HTTP handlers, forced GC, request
// payloads or credentials. The operator provisions a private directory first.
func startMemoryDiagnostics() func() {
	dir := os.Getenv("SUB2API_MEMORY_DIAGNOSTICS_DIR")
	if dir == "" {
		return func() {}
	}
	if err := memoryDiagnosticsDirectory(dir); err != nil {
		slog.Error("memory_diagnostics_disabled", "error", err)
		return func() {}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR2)
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		var last time.Time
		count := 0
		for {
			select {
			case <-done:
				return
			case <-signals:
				// Bound diagnostic overhead and disk use, including repeated signals.
				if count >= 24 || time.Since(last) < 5*time.Second {
					continue
				}
				last = time.Now()
				count++
				if err := writeMemorySnapshot(dir); err != nil {
					slog.Error("memory_snapshot_failed", "error", err)
				} else {
					slog.Info("memory_snapshot_saved", "sample", count)
				}
			}
		}
	}()
	slog.Info("memory_diagnostics_enabled", "signal", "SIGUSR2", "max_samples", 24)
	return func() { signal.Stop(signals); close(done); <-stopped }
}

func memoryDiagnosticsDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dir) || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("memory diagnostics requires an absolute private directory (0700)")
	}
	return nil
}

func writeMemorySnapshot(dir string) error {
	if err := memoryDiagnosticsDirectory(dir); err != nil {
		return err
	}
	now := time.Now().UTC()
	path := filepath.Join(dir, fmt.Sprintf("snapshot-%d-%d", os.Getpid(), now.UnixNano()))
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	raw, err := json.Marshal(struct {
		At         time.Time
		PID        int
		Goroutines int
		Memory     runtime.MemStats
	}{now, os.Getpid(), runtime.NumGoroutine(), stats})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(path, "memory.json"), raw, 0600); err != nil {
		return err
	}
	heap, err := os.OpenFile(filepath.Join(path, "heap.pprof"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	profileErr := pprof.WriteHeapProfile(heap)
	closeErr := heap.Close()
	if profileErr != nil {
		return profileErr
	}
	return closeErr
}
