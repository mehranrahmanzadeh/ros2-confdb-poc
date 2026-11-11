package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/config"
	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/state"
)

type readFunc func(context.Context, bool) ([]byte, error)

func (read readFunc) Read(ctx context.Context, previous bool) ([]byte, error) {
	return read(ctx, previous)
}

type fetchFunc func(context.Context, string, config.Config) ([]byte, error)

func (fetch fetchFunc) Fetch(ctx context.Context, source string, cfg config.Config) ([]byte, error) {
	return fetch(ctx, source, cfg)
}

func TestRetriesOnlyFailedOutputsAndRetainsLastGood(t *testing.T) {
	common := t.TempDir()
	store := testStore(t, common)
	if err := store.Publish(0, "failed.yaml", []byte("last good")); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(0, "removed.yaml", []byte("remove me")); err != nil {
		t.Fatal(err)
	}
	configJSON := `{"sources":[
		{"source":"https://example.com/fail","output":"failed.yaml","version":2},
		{"source":"https://example.com/good","output":"good.yaml","version":1}
	]}`
	calls := make(map[string]int)
	runner := testReconciler(store, configJSON, fetchFunc(func(_ context.Context, source string, _ config.Config) ([]byte, error) {
		calls[source]++
		if strings.HasSuffix(source, "/fail") {
			return nil, errors.New("source unavailable")
		}
		return []byte("node:\n  ros__parameters:\n    value: 1\n"), nil
	}))
	if err := runner.Reconcile(context.Background()); err == nil {
		t.Fatal("expected exhausted retry error")
	}
	if calls["https://example.com/fail"] != 4 || calls["https://example.com/good"] != 1 {
		t.Fatalf("fetch counts = %#v", calls)
	}
	assertFile(t, filepath.Join(common, "params", "failed.yaml"), "last good")
	if _, err := os.Stat(filepath.Join(common, "params", "good.yaml")); err != nil {
		t.Fatalf("independent good output not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(common, "params", "removed.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed output still exists: %v", err)
	}
}

func TestStaleDownloadCannotRecreateRemovedOutput(t *testing.T) {
	common := t.TempDir()
	store := testStore(t, common)
	configJSON := `{"sources":[{"source":"https://example.com/old","output":"old.yaml","version":1}]}`
	runner := testReconciler(store, configJSON, fetchFunc(func(_ context.Context, _ string, _ config.Config) ([]byte, error) {
		if err := store.Bump(); err != nil {
			t.Fatal(err)
		}
		return []byte("node: {ros__parameters: {value: 1}}\n"), nil
	}))
	if err := runner.Reconcile(context.Background()); !errors.Is(err, state.ErrStale) {
		t.Fatalf("expected stale refresh, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(common, "params", "old.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale output was published: %v", err)
	}
}

func TestSnapshotIsRereadWhenGenerationChanges(t *testing.T) {
	common := t.TempDir()
	store := testStore(t, common)
	oldJSON := `{"sources":[{"source":"https://example.com/old","output":"old.yaml","version":1}]}`
	newJSON := `{"sources":[{"source":"https://example.com/new","output":"new.yaml","version":2}]}`
	count := 0
	runner := testReconciler(store, newJSON, fetchFunc(func(_ context.Context, source string, _ config.Config) ([]byte, error) {
		if !strings.HasSuffix(source, "/new") {
			t.Fatalf("unexpected stale source %q", source)
		}
		return []byte("node: {ros__parameters: {value: 1}}\n"), nil
	}))
	runner.View = readFunc(func(context.Context, bool) ([]byte, error) {
		count++
		if count == 1 {
			if err := store.Bump(); err != nil {
				t.Fatal(err)
			}
			return []byte(oldJSON), nil
		}
		return []byte(newJSON), nil
	})
	if err := runner.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("snapshot was read %d times, want 2", count)
	}
	if _, err := os.Stat(filepath.Join(common, "params", "new.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestConfigurationReadRetries(t *testing.T) {
	store := testStore(t, t.TempDir())
	count := 0
	runner := testReconciler(store, `{}`, fetchFunc(func(context.Context, string, config.Config) ([]byte, error) {
		t.Fatal("unexpected fetch")
		return nil, nil
	}))
	runner.View = readFunc(func(context.Context, bool) ([]byte, error) {
		count++
		return nil, errors.New("view disconnected")
	})
	if err := runner.Reconcile(context.Background()); err == nil {
		t.Fatal("expected configuration read error")
	}
	if count != 4 {
		t.Fatalf("read count = %d, want 4", count)
	}
}

func TestLogsDoNotExposeCredentialInFilenameOrError(t *testing.T) {
	store := testStore(t, t.TempDir())
	document := `{"sources":[{"source":"https://example.com/a","output":"fake-secret","version":1}],
		"credentials":[{"origin":"https://example.com","method":"bearer","secret":"fake-secret"}]}`
	runner := testReconciler(store, document, fetchFunc(func(context.Context, string, config.Config) ([]byte, error) {
		return nil, errors.New("problem with fake-secret")
	}))
	var logs bytes.Buffer
	runner.Logger = log.New(&logs, "", 0)
	if err := runner.Reconcile(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	if strings.Contains(logs.String(), "fake-secret") {
		t.Fatalf("credential leaked in logs: %s", logs.String())
	}
}

func TestReconcileRejectsDuplicateOutputsBeforeAnyChanges(t *testing.T) {
	common := t.TempDir()
	store := testStore(t, common)
	if err := store.Publish(0, "existing.yaml", []byte("last good")); err != nil {
		t.Fatal(err)
	}
	document := `{"sources":[
		{"source":"https://example.com/a","output":"same.yaml","version":1},
		{"source":"https://example.com/b","output":"other.yaml","version":1},
		{"source":"https://example.com/c","output":"same.yaml","version":1}]}`
	calls := 0
	runner := testReconciler(store, document, fetchFunc(func(context.Context, string, config.Config) ([]byte, error) {
		calls++
		return []byte("downloaded"), nil
	}))
	err := runner.Reconcile(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sources 1 and 3") {
		t.Fatalf("expected duplicate-output error, got %v", err)
	}
	if calls != 0 {
		t.Fatalf("invalid configuration caused %d downloads", calls)
	}
	assertFile(t, filepath.Join(common, "params", "existing.yaml"), "last good")
	if _, err := os.Stat(filepath.Join(common, "params", "same.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate output was published: %v", err)
	}
}

func TestRefreshSourceSizeLimit(t *testing.T) {
	common := t.TempDir()
	store := testStore(t, common)
	data := make([]byte, maxOutputBytes+1)
	size := len(data)
	runner := testReconciler(store, `{}`, fetchFunc(func(context.Context, string, config.Config) ([]byte, error) {
		return data[:size], nil
	}))
	source := config.Source{URL: "https://example.com/a", Output: "params.yaml"}
	err := runner.refreshSource(context.Background(), config.Config{}, 0, source)
	if err == nil || !strings.Contains(err.Error(), "per-output limit") {
		t.Fatalf("expected size-limit error, got %v", err)
	}
	path := filepath.Join(common, "params", source.Output)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized source was published: %v", err)
	}

	size = maxOutputBytes
	if err := runner.refreshSource(context.Background(), config.Config{}, 0, source); err != nil {
		t.Fatalf("source at size limit was rejected: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != maxOutputBytes {
		t.Fatalf("published size at limit = %v, err = %v", info, err)
	}
}

func testStore(t *testing.T, common string) *state.Store {
	t.Helper()
	store, err := state.Open(common)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testReconciler(store *state.Store, document string, fetcher SourceFetcher) *Reconciler {
	return &Reconciler{
		Store: store,
		View: readFunc(func(context.Context, bool) ([]byte, error) {
			return []byte(document), nil
		}),
		Fetcher:     fetcher,
		Logger:      log.New(io.Discard, "", 0),
		RetryDelays: []time.Duration{0, 0, 0},
	}
}

func assertFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s = %q, err = %v, want %q", path, data, err, want)
	}
}
