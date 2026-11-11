package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/config"
)

func TestServiceStartsRefreshesAndStops(t *testing.T) {
	common, err := os.MkdirTemp("", "ros2-agent-service-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(common)
	store := testStore(t, common)
	var mu sync.Mutex
	document := `{"sources":[{"source":"https://example.com/one","output":"params.yaml","version":1}]}`
	runner := testReconciler(store, document, fetchFunc(func(_ context.Context, source string, _ config.Config) ([]byte, error) {
		if strings.HasSuffix(source, "/one") {
			return []byte("node: {ros__parameters: {value: 1}}\n"), nil
		}
		return []byte("node: {ros__parameters: {value: 2}}\n"), nil
	}))
	runner.View = readFunc(func(context.Context, bool) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		return []byte(document), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Serve(ctx) }()
	path := filepath.Join(common, "params", "params.yaml")
	waitForFile(t, path, "value: 1")
	mu.Lock()
	document = `{"sources":[{"source":"https://example.com/two","output":"params.yaml","version":2}]}`
	mu.Unlock()
	if err := store.Bump(); err != nil {
		t.Fatal(err)
	}
	if err := Notify(store); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, path, "value: 2")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := Notify(store); err == nil {
		t.Fatal("notification succeeded with the service stopped")
	}
}

func TestSecondDaemonCannotReplaceLiveSocket(t *testing.T) {
	common, err := os.MkdirTemp("", "ros2-agent-lock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(common)
	store := testStore(t, common)
	lock, err := store.LockDaemon()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	runner := testReconciler(store, `{}`, fetchFunc(func(context.Context, string, config.Config) ([]byte, error) {
		return nil, nil
	}))
	if err := runner.Serve(context.Background()); err == nil {
		t.Fatal("second daemon acquired the lock")
	}
}

func waitForFile(t *testing.T, path, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), text) {
			return
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s", text, path)
}
