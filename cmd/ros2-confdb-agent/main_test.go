package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/state"
)

func TestChangeHookRejectsDuplicateOutputsViaSnapctl(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
set -eu
case "$1" in
  fail) printf '%s\n' "$2" > "$ROS2_TEST_SNAPCTL_FAILURE"; exit 0 ;;
  get) ;;
  *) exit 2 ;;
esac
previous=false
field=
for arg in "$@"; do
  case "$arg" in --previous) previous=true ;; esac
  field=$arg
done
case "$field/$previous" in
  sources/false) printf '%s\n' '{"sources":[{"source":"https://example.com/a","output":"same.yaml","version":1},{"source":"https://example.com/b","output":"same.yaml","version":1}]}' ;;
  sources/true) printf '%s\n' '{"sources":[]}' ;;
  policy/*) printf '%s\n' '{"policy":{}}' ;;
  credentials/*) printf '%s\n' '{"credentials":[]}' ;;
  *) exit 3 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "snapctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	common := t.TempDir()
	failure := filepath.Join(dir, "failure")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SNAP_COMMON", common)
	t.Setenv("ROS2_TEST_SNAPCTL_FAILURE", failure)

	if err := run([]string{"change-hook"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(failure)
	if err != nil || !strings.Contains(string(data), "sources 1 and 2") {
		t.Fatalf("snapctl fail was not called with the duplicate error: %q, %v", data, err)
	}
	store, err := state.Open(common)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := store.Generation()
	if err != nil || generation != 0 {
		t.Fatalf("rejected change invalidated generation: %d, %v", generation, err)
	}
}
