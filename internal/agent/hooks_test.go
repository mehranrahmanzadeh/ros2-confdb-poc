package agent

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
)

func TestChangeValidatesChangedSourcesButAllowsPolicyOnlyRestriction(t *testing.T) {
	const old = `{"sources":[{"source":"https://example.com/a","output":"a.yaml","version":1}]}`
	tests := []struct {
		name        string
		proposed    string
		wantFailure bool
	}{
		{
			name: "policy only blocks existing source",
			proposed: `{"sources":[{"source":"https://example.com/a","output":"a.yaml","version":1}],
				"policy":{"allowlist-enabled":true,"allowed-hosts":["other.example"]}}`,
		},
		{
			name: "changed source is blocked",
			proposed: `{"sources":[{"source":"https://example.com/b","output":"a.yaml","version":2}],
				"policy":{"allowlist-enabled":true,"allowed-hosts":["other.example"]}}`,
			wantFailure: true,
		},
		{
			name:        "changed source is allowed",
			proposed:    `{"sources":[{"source":"https://example.com/b","output":"a.yaml","version":2}]}`,
			wantFailure: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t, t.TempDir())
			hooks := Hooks{
				Store:  store,
				Logger: log.New(io.Discard, "", 0),
				View: readFunc(func(_ context.Context, previous bool) ([]byte, error) {
					if previous {
						return []byte(old), nil
					}
					return []byte(test.proposed), nil
				}),
			}
			err := hooks.Change(context.Background())
			if (err != nil) != test.wantFailure {
				t.Fatalf("change error = %v, want failure = %v", err, test.wantFailure)
			}
			generation, err := store.Generation()
			if err != nil {
				t.Fatal(err)
			}
			if test.wantFailure && generation != 0 || !test.wantFailure && generation != 1 {
				t.Fatalf("generation = %d", generation)
			}
		})
	}
}

func TestChangeRejectsInvalidConfigurationWithoutLeakingSecret(t *testing.T) {
	store := testStore(t, t.TempDir())
	hooks := Hooks{
		Store:  store,
		Logger: log.New(io.Discard, "", 0),
		View: readFunc(func(_ context.Context, previous bool) ([]byte, error) {
			if previous {
				return []byte(`{}`), nil
			}
			return []byte(`{"credentials":[{"origin":"https://example.com","method":"bearer","secret":"fake-secret\nbad"}]}`), nil
		}),
	}
	err := hooks.Change(context.Background())
	if err == nil || strings.Contains(err.Error(), "fake-secret") {
		t.Fatalf("unexpected or sensitive error: %v", err)
	}
}

func TestChangeRejectsDuplicateOutputsBeforeInvalidation(t *testing.T) {
	const proposed = `{"sources":[
		{"source":"https://example.com/a","output":"fake-secret","version":1},
		{"source":"https://example.com/b","output":"other.yaml","version":1},
		{"source":"https://example.com/c","output":"fake-secret","version":1}],
		"credentials":[{"origin":"https://example.com","method":"bearer","secret":"fake-secret"}]}`
	store := testStore(t, t.TempDir())
	var logs bytes.Buffer
	hooks := Hooks{
		Store:  store,
		Logger: log.New(&logs, "", 0),
		View: readFunc(func(_ context.Context, previous bool) ([]byte, error) {
			if previous {
				return []byte(`{}`), nil
			}
			return []byte(proposed), nil
		}),
	}
	err := hooks.Change(context.Background())
	if err == nil || !strings.Contains(err.Error(), "sources 1 and 3") {
		t.Fatalf("expected duplicate-output error identifying sources 1 and 3, got %v", err)
	}
	if strings.Contains(err.Error(), "fake-secret") || strings.Contains(logs.String(), "fake-secret") {
		t.Fatal("duplicate-output error exposed a credential")
	}
	generation, err := store.Generation()
	if err != nil || generation != 0 {
		t.Fatalf("rejected change invalidated generation: %d, %v", generation, err)
	}
	if logs.Len() != 0 {
		t.Fatalf("rejected change unexpectedly notified the service: %s", logs.String())
	}
}

func TestChangeRejectsUnchangedDuplicateOutputs(t *testing.T) {
	const sources = `[{"source":"https://example.com/a","output":"same.yaml","version":1},
		{"source":"https://example.com/b","output":"same.yaml","version":1}]`
	store := testStore(t, t.TempDir())
	hooks := Hooks{
		Store:  store,
		Logger: log.New(io.Discard, "", 0),
		View: readFunc(func(_ context.Context, previous bool) ([]byte, error) {
			if previous {
				return []byte(`{"sources":` + sources + `}`), nil
			}
			return []byte(`{"sources":` + sources + `,"policy":{"allow-http":false}}`), nil
		}),
	}
	if err := hooks.Change(context.Background()); err == nil {
		t.Fatal("expected a policy-only change with duplicate outputs to be rejected")
	}
	generation, err := store.Generation()
	if err != nil || generation != 0 {
		t.Fatalf("rejected change invalidated generation: %d, %v", generation, err)
	}
}

func TestChangeAllowsFixingPreviouslyDuplicatedOutputs(t *testing.T) {
	const previous = `{"sources":[
		{"source":"https://example.com/a","output":"same.yaml","version":1},
		{"source":"https://example.com/b","output":"same.yaml","version":1}]}`
	const proposed = `{"sources":[
		{"source":"https://example.com/a","output":"same.yaml","version":1},
		{"source":"https://example.com/b","output":"other.yaml","version":1}]}`
	store := testStore(t, t.TempDir())
	hooks := Hooks{
		Store:  store,
		Logger: log.New(io.Discard, "", 0),
		View: readFunc(func(_ context.Context, old bool) ([]byte, error) {
			if old {
				return []byte(previous), nil
			}
			return []byte(proposed), nil
		}),
	}
	if err := hooks.Change(context.Background()); err != nil {
		t.Fatalf("cannot repair previously duplicated outputs: %v", err)
	}
	generation, err := store.Generation()
	if err != nil || generation != 1 {
		t.Fatalf("accepted change generation = %d, %v", generation, err)
	}
}
