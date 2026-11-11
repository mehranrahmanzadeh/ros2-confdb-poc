package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/fetch"
)

func TestHTTPDownloadsPublishedUnchanged(t *testing.T) {
	const defaults = "# preserve comments and CRLF\r\n/robot/node:\r\n  ros__parameters:\r\n    speed: 1\r\n"
	const other = "not: [valid YAML\n\x00\xff"
	var defaultsRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/defaults":
			defaultsRequests.Add(1)
			fmt.Fprint(w, defaults)
		case "/other":
			fmt.Fprint(w, other)
		case "/empty":
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	common := t.TempDir()
	store := testStore(t, common)
	document := fmt.Sprintf(`{"sources":[
		{"source":%q,"output":"defaults.yaml","version":1},
		{"source":%q,"output":"other.yaml","version":1},
		{"source":%q,"output":"copy.yaml","version":1},
		{"source":%q,"output":"empty.yaml","version":1}
	]}`, server.URL+"/defaults", server.URL+"/other", server.URL+"/defaults", server.URL+"/empty")
	runner := testReconciler(store, document, fetch.Downloader{})
	if err := runner.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertFile(t, filepath.Join(common, "params", "defaults.yaml"), defaults)
	assertFile(t, filepath.Join(common, "params", "other.yaml"), other)
	assertFile(t, filepath.Join(common, "params", "copy.yaml"), defaults)
	assertFile(t, filepath.Join(common, "params", "empty.yaml"), "")
	if defaultsRequests.Load() != 2 {
		t.Fatalf("same URL with different outputs was requested %d times", defaultsRequests.Load())
	}
}

func TestRestrictivePolicyStopsFetchingAndKeepsLastGood(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "node: {ros__parameters: {value: 1}}\n")
	}))
	defer server.Close()
	common := t.TempDir()
	store := testStore(t, common)
	source := fmt.Sprintf(`{"source":%q,"output":"params.yaml","version":1}`, server.URL)
	initial := `{"sources":[` + source + `]}`
	runner := testReconciler(store, initial, fetch.Downloader{})
	if err := runner.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(common, "params", "params.yaml")
	lastGood, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bump(); err != nil {
		t.Fatal(err)
	}
	restricted := `{"sources":[` + source + `],"policy":{"allowlist-enabled":true,"allowed-hosts":["other.example"]}}`
	runner.View = readFunc(func(context.Context, bool) ([]byte, error) {
		return []byte(restricted), nil
	})
	if err := runner.Reconcile(context.Background()); err == nil {
		t.Fatal("expected blocked source error")
	}
	if requests.Load() != 1 {
		t.Fatalf("blocked refresh made %d total network requests, want 1", requests.Load())
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(lastGood) {
		t.Fatalf("last good output changed: %q, %v", after, err)
	}
	if !strings.Contains(string(after), "value: 1") {
		t.Fatalf("unexpected last good output: %q", after)
	}
}
