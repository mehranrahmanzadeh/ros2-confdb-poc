package state

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPublishAndRemoveManagedFiles(t *testing.T) {
	common := t.TempDir()
	store := openTestStore(t, common)
	if err := store.Publish(0, "params.yaml", []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(0, "params.yaml", []byte("second")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(common, "params", "params.yaml")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Fatalf("published output = %q, err = %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("output permissions = %v, err = %v", info, err)
	}

	unmanaged := filepath.Join(common, "params", "unmanaged.yaml")
	if err := os.WriteFile(unmanaged, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveUnconfigured(0, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed output was not removed: %v", err)
	}
	if data, err := os.ReadFile(unmanaged); err != nil || string(data) != "keep" {
		t.Fatalf("unmanaged output changed: %q, %v", data, err)
	}
}

func TestStaleGenerationCannotPublishOrDelete(t *testing.T) {
	common := t.TempDir()
	store := openTestStore(t, common)
	if err := store.Publish(0, "params.yaml", []byte("good")); err != nil {
		t.Fatal(err)
	}
	if err := store.Bump(); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(0, "params.yaml", []byte("stale")); !errors.Is(err, ErrStale) {
		t.Fatalf("stale publish error = %v", err)
	}
	if err := store.RemoveUnconfigured(0, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("stale removal error = %v", err)
	}
	path := filepath.Join(common, "params", "params.yaml")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "good" {
		t.Fatalf("last good output changed: %q, %v", data, err)
	}
	if err := store.RemoveUnconfigured(1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current generation failed to remove output: %v", err)
	}
}

func TestRefusesToOverwriteUnmanagedFile(t *testing.T) {
	common := t.TempDir()
	store := openTestStore(t, common)
	path := filepath.Join(common, "params", "foreign.yaml")
	if err := os.WriteFile(path, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(0, "foreign.yaml", []byte("new")); err == nil {
		t.Fatal("expected unmanaged output error")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep" {
		t.Fatalf("unmanaged output changed: %q, %v", data, err)
	}
}

func TestCorruptManifestFailsWithoutRemovingFiles(t *testing.T) {
	common := t.TempDir()
	store := openTestStore(t, common)
	if err := store.Publish(0, "params.yaml", []byte("keep")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(common, ".agent", "managed.json"), []byte(`["../outside"]`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveUnconfigured(0, nil); err == nil {
		t.Fatal("expected invalid manifest error")
	}
	if _, err := os.Stat(filepath.Join(common, "params", "params.yaml")); err != nil {
		t.Fatalf("output was removed: %v", err)
	}
}

func TestConcurrentBumpsDoNotLoseGenerations(t *testing.T) {
	store := openTestStore(t, t.TempDir())
	const count = 20
	var workers sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errs <- store.Bump()
		}()
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	generation, err := store.Generation()
	if err != nil || generation != count {
		t.Fatalf("generation = %d, err = %v, want %d", generation, err, count)
	}
}

func TestManagedSymlinkIsNotFollowed(t *testing.T) {
	common := t.TempDir()
	store := openTestStore(t, common)
	if err := store.Publish(0, "params.yaml", []byte("good")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(common, "params", "params.yaml")
	outside := filepath.Join(common, "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(0, "params.yaml", []byte("bad")); err == nil {
		t.Fatal("expected symlink error")
	}
	if err := store.RemoveUnconfigured(0, nil); err == nil {
		t.Fatal("expected symlink error on removal")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
		t.Fatalf("symlink target changed: %q, %v", data, err)
	}
}

func TestOpenRejectsSymlinkedAgentDirectories(t *testing.T) {
	for _, name := range []string{".agent", "params"} {
		t.Run(name, func(t *testing.T) {
			common := t.TempDir()
			target := t.TempDir()
			if err := os.Symlink(target, filepath.Join(common, name)); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(common); err == nil {
				t.Fatal("expected symlinked directory to be rejected")
			}
		})
	}
}

func TestCleanupRemovesOnlyAgentTemporaryFiles(t *testing.T) {
	common := t.TempDir()
	store := openTestStore(t, common)
	lock, err := store.LockDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	dir := filepath.Join(common, ".agent")
	for _, name := range []string{".staged-orphan", ".state-orphan", "keep"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CleanupTemporary(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".staged-orphan", ".state-orphan"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary file %s remains: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Fatalf("unrelated state file was removed: %v", err)
	}
}

func openTestStore(t *testing.T, common string) *Store {
	t.Helper()
	store, err := Open(common)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
