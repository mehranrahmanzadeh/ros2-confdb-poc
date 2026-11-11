package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/config"
)

func TestSnapctlViewUsesExplicitDefaultsForEmptyFields(t *testing.T) {
	var calls [][]string
	view := SnapctlView{
		Plug: "ros2-config",
		run: func(_ context.Context, args ...string) ([]byte, error) {
			calls = append(calls, append([]string(nil), args...))
			field := args[len(args)-1]
			value := []byte(`[]`)
			if field == "policy" {
				value = []byte(`{}`)
			}
			return json.Marshal(map[string]json.RawMessage{field: value})
		},
	}
	data, err := view.Read(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Parse(data); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"get", "--view", "--previous", "--default", "[]", "-d", ":ros2-config", "sources"},
		{"get", "--view", "--previous", "--default", "{}", "-d", ":ros2-config", "policy"},
		{"get", "--view", "--previous", "--default", "[]", "-d", ":ros2-config", "credentials"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("snapctl calls = %#v, want %#v", calls, want)
	}
}

func TestSnapctlViewRejectsMalformedOutput(t *testing.T) {
	view := SnapctlView{
		Plug: "ros2-config",
		run: func(context.Context, ...string) ([]byte, error) {
			return []byte(`{}`), nil
		},
	}
	if _, err := view.Read(context.Background(), false); err == nil {
		t.Fatal("expected invalid view document error")
	}
}

func TestSnapctlViewExecutesSnapctl(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
set -eu
case "$*" in
  *"--view"*":ros2-agent"*) ;;
  *) exit 2 ;;
esac
for arg in "$@"; do field=$arg; done
case "$field" in
  sources) printf '%s\n' '{"sources":[{"source":"https://example.com/a","output":"a.yaml","version":1}]}' ;;
  policy) printf '%s\n' '{"policy":{}}' ;;
  credentials) printf '%s\n' '{"credentials":[]}' ;;
  *) exit 3 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "snapctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	data, err := (SnapctlView{Plug: "ros2-agent"}).Read(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 1 || !strings.HasSuffix(cfg.Sources[0].URL, "/a") {
		t.Fatalf("unexpected configuration: %#v", cfg.Sources)
	}
}
