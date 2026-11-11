package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/config"
)

func TestAuthenticationMethods(t *testing.T) {
	tests := []struct {
		method   string
		username string
		header   string
		want     string
	}{
		{method: "basic", username: "robot", want: "Basic cm9ib3Q6ZmFrZS1zZWNyZXQ="},
		{method: "bearer", want: "Bearer fake-secret"},
		{method: "api-key", header: "X-Api-Key", want: "fake-secret"},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				header := "Authorization"
				if test.header != "" {
					header = test.header
				}
				got = r.Header.Get(header)
				fmt.Fprint(w, "node: {ros__parameters: {value: 1}}\n")
			}))
			defer server.Close()
			cfg := testConfig(t, map[string]any{
				"credentials": []map[string]any{{
					"origin": server.URL, "method": test.method, "username": test.username,
					"header": test.header, "secret": "fake-secret",
				}},
			})
			if _, err := (Downloader{}).Fetch(context.Background(), server.URL+"/params", cfg); err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("authorization header = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRedirectDoesNotForwardCredentialsToAnotherOrigin(t *testing.T) {
	for _, method := range []string{"bearer", "api-key"} {
		t.Run(method, func(t *testing.T) {
			var receivedAuth, receivedKey string
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				receivedAuth = r.Header.Get("Authorization")
				receivedKey = r.Header.Get("X-Api-Key")
				fmt.Fprint(w, "ok")
			}))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/params", http.StatusFound)
			}))
			defer source.Close()
			credential := map[string]any{"origin": source.URL, "method": method, "secret": "fake-secret"}
			if method == "api-key" {
				credential["header"] = "X-Api-Key"
			}
			cfg := testConfig(t, map[string]any{"credentials": []map[string]any{credential}})
			if _, err := (Downloader{}).Fetch(context.Background(), source.URL, cfg); err != nil {
				t.Fatal(err)
			}
			if receivedAuth != "" || receivedKey != "" {
				t.Fatalf("credentials leaked across origin: Authorization=%q X-Api-Key=%q", receivedAuth, receivedKey)
			}
		})
	}
}

func TestRedirectAppliesTargetOriginCredential(t *testing.T) {
	var authorization string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		fmt.Fprint(w, "ok")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer source.Close()
	cfg := testConfig(t, map[string]any{"credentials": []map[string]any{
		{"origin": source.URL, "method": "bearer", "secret": "first-token"},
		{"origin": target.URL, "method": "bearer", "secret": "second-token"},
	}})
	if _, err := (Downloader{}).Fetch(context.Background(), source.URL, cfg); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer second-token" {
		t.Fatalf("target authorization = %q", authorization)
	}
}

func TestRedirectPolicyAndLimit(t *testing.T) {
	targetReached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetReached = true
		fmt.Fprint(w, "ok")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetURL := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
		http.Redirect(w, r, targetURL, http.StatusFound)
	}))
	defer source.Close()

	cfg := testConfig(t, map[string]any{
		"policy": map[string]any{"allowlist-enabled": true, "allowed-hosts": []string{"127.0.0.1"}},
	})
	if _, err := (Downloader{}).Fetch(context.Background(), source.URL, cfg); err == nil ||
		!strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("expected redirect policy error, got %v", err)
	}
	if targetReached {
		t.Fatal("blocked redirect reached the target")
	}

	cfg = testConfig(t, map[string]any{"policy": map[string]any{"max-redirects": 0}})
	if _, err := (Downloader{}).Fetch(context.Background(), source.URL, cfg); err == nil ||
		!strings.Contains(err.Error(), "redirect limit") {
		t.Fatalf("expected redirect limit error, got %v", err)
	}
}

func TestSizeLimitAppliesAfterDecompression(t *testing.T) {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(bytes.Repeat([]byte("x"), 1024*1024+1)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}))
	defer server.Close()
	cfg := testConfig(t, map[string]any{"policy": map[string]any{"max-source-mib": 1}})
	if _, err := (Downloader{}).Fetch(context.Background(), server.URL, cfg); err == nil ||
		!strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected decoded size limit error, got %v", err)
	}
}

func TestSourceSizeLimitAtBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		size := 1024 * 1024
		if r.URL.Path == "/over" {
			size++
		}
		_, _ = w.Write(bytes.Repeat([]byte("x"), size))
	}))
	defer server.Close()
	cfg := testConfig(t, map[string]any{"policy": map[string]any{"max-source-mib": 1}})
	data, err := (Downloader{}).Fetch(context.Background(), server.URL+"/exact", cfg)
	if err != nil || len(data) != 1024*1024 {
		t.Fatalf("exact limit returned %d bytes, error %v", len(data), err)
	}
	if _, err := (Downloader{}).Fetch(context.Background(), server.URL+"/over", cfg); err == nil ||
		!strings.Contains(err.Error(), "size limit") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func TestFailureDoesNotExposeURLQueryOrCredential(t *testing.T) {
	const secret = "fake-secret-value"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/next?token="+secret, http.StatusFound)
	}))
	defer server.Close()
	cfg := testConfig(t, map[string]any{
		"policy": map[string]any{"max-redirects": 0},
		"credentials": []map[string]any{{
			"origin": server.URL, "method": "bearer", "secret": secret,
		}},
	})
	_, err := (Downloader{}).Fetch(context.Background(), server.URL+"?token="+secret, cfg)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "token=") {
		t.Fatalf("unexpected or sensitive error: %v", err)
	}
}

func TestMalformedRedirectDoesNotExposeLocation(t *testing.T) {
	const secret = "query-only-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/%zz?token="+secret)
		w.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	cfg := testConfig(t, map[string]any{})
	_, err := (Downloader{}).Fetch(context.Background(), server.URL, cfg)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "token=") {
		t.Fatalf("unexpected or sensitive redirect error: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type failingBody struct{}

func (failingBody) Read([]byte) (int, error) {
	return 0, errors.New("read https://example.com/file?token=query-only-secret failed")
}

func (failingBody) Close() error {
	return nil
}

func TestResponseReadFailureDoesNotExposeURL(t *testing.T) {
	cfg := testConfig(t, map[string]any{})
	downloader := Downloader{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: failingBody{}}, nil
	})}
	_, err := downloader.Fetch(context.Background(), "https://example.com/params", cfg)
	if err == nil || strings.Contains(err.Error(), "query-only-secret") || strings.Contains(err.Error(), "token=") {
		t.Fatalf("unexpected or sensitive response error: %v", err)
	}
}

func TestRejectsUnsupportedContentEncoding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		fmt.Fprint(w, "unhandled encoding")
	}))
	defer server.Close()
	cfg := testConfig(t, map[string]any{})
	if _, err := (Downloader{}).Fetch(context.Background(), server.URL, cfg); err == nil ||
		!strings.Contains(err.Error(), "unsupported content encoding") {
		t.Fatalf("expected unsupported content encoding error, got %v", err)
	}
}

func TestConfiguredTimeoutCancelsDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg := testConfig(t, map[string]any{"policy": map[string]any{"timeout-seconds": 1}})
	started := time.Now()
	_, err := (Downloader{}).Fetch(context.Background(), server.URL, cfg)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("one-second timeout elapsed after %v", elapsed)
	}
}

func TestRedirectLimitAtBoundary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			http.Redirect(w, r, "/middle", http.StatusFound)
		case "/middle":
			http.Redirect(w, r, "/end", http.StatusFound)
		case "/end":
			fmt.Fprint(w, "ok")
		}
	}))
	defer server.Close()
	cfg := testConfig(t, map[string]any{"policy": map[string]any{"max-redirects": 2}})
	if _, err := (Downloader{}).Fetch(context.Background(), server.URL+"/start", cfg); err != nil {
		t.Fatalf("two allowed redirects failed: %v", err)
	}
	cfg = testConfig(t, map[string]any{"policy": map[string]any{"max-redirects": 1}})
	if _, err := (Downloader{}).Fetch(context.Background(), server.URL+"/start", cfg); err == nil ||
		!strings.Contains(err.Error(), "redirect limit") {
		t.Fatalf("expected redirect limit after one redirect, got %v", err)
	}
}

func testConfig(t *testing.T, data map[string]any) config.Config {
	t.Helper()
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
