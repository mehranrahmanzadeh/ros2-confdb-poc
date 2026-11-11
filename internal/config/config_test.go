package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDefaultPolicy(t *testing.T) {
	cfg, err := Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Policy.AllowHTTP || !cfg.Policy.AllowHTTPS || cfg.Policy.AllowlistEnabled ||
		cfg.Policy.Timeout != 60*time.Second || cfg.Policy.MaxSourceBytes != 10*1024*1024 ||
		cfg.Policy.MaxRedirects != 5 {
		t.Fatalf("unexpected defaults: %#v", cfg.Policy)
	}
	for _, source := range []string{"http://example.com/a", "https://example.com/a"} {
		if _, err := cfg.Policy.CheckURL(source); err != nil {
			t.Errorf("%s unexpectedly blocked: %v", source, err)
		}
	}
}

func TestWildcardAllowlist(t *testing.T) {
	cfg, err := Parse([]byte(`{"policy":{"allowlist-enabled":true,"allowed-hosts":["*.Example.COM","127.0.0.1"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]bool{
		"https://a.example.com/a":     true,
		"http://b.a.example.com:81/a": true,
		"https://example.com/a":       false,
		"https://example.com.evil/a":  false,
		"http://127.0.0.1:8080/a":     true,
		"http://127.0.0.2/a":          false,
	}
	for source, want := range tests {
		_, err := cfg.Policy.CheckURL(source)
		if (err == nil) != want {
			t.Errorf("CheckURL(%q) error = %v, allowed = %v", source, err, want)
		}
	}
}

func TestEmptyEnabledAllowlistBlocksEveryHost(t *testing.T) {
	cfg, err := Parse([]byte(`{"policy":{"allowlist-enabled":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Policy.CheckURL("https://example.com/"); err == nil {
		t.Fatal("enabled empty allowlist must block the source")
	}
}

func TestOriginsAreNormalized(t *testing.T) {
	tests := [][2]string{
		{"https://EXAMPLE.com", "https://example.com:443"},
		{"http://example.com.:080", "http://example.com:80"},
		{"https://bücher.example", "https://xn--bcher-kva.example:443"},
		{"http://[::1]:8080", "http://[::1]:8080"},
	}
	for _, test := range tests {
		origin, err := ParseOrigin(test[0])
		if err != nil {
			t.Fatalf("ParseOrigin(%q): %v", test[0], err)
		}
		if origin.String() != test[1] {
			t.Errorf("ParseOrigin(%q) = %q, want %q", test[0], origin.String(), test[1])
		}
	}
}

func TestCredentialLookupIsPerOrigin(t *testing.T) {
	cfg, err := Parse([]byte(`{"credentials":[{"origin":"https://EXAMPLE.com","method":"bearer","secret":"fake-token"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		url  string
		want bool
	}{
		{"https://example.com:443/a", true},
		{"https://example.com:444/a", false},
		{"http://example.com/a", false},
		{"https://sub.example.com/a", false},
	} {
		_, origin, err := ParseURL(test.url)
		if err != nil {
			t.Fatal(err)
		}
		credential, err := cfg.CredentialFor(origin)
		if err != nil {
			t.Fatal(err)
		}
		if got := credential != nil; got != test.want {
			t.Errorf("credential for %s = %v, want %v", test.url, got, test.want)
		}
	}
}

func TestRejectsInvalidConfiguration(t *testing.T) {
	tests := map[string]string{
		"missing version":  `{"sources":[{"source":"https://example.com/a","output":"x.yaml"}]}`,
		"output path":      `{"sources":[{"source":"https://example.com/a","output":"../x","version":1}]}`,
		"userinfo":         `{"sources":[{"source":"https://user:pass@example.com/a","output":"x","version":1}]}`,
		"unsupported URL":  `{"sources":[{"source":"file:///x","output":"x","version":1}]}`,
		"invalid port":     `{"sources":[{"source":"http://example.com:65536/a","output":"x","version":1}]}`,
		"unknown field":    `{"policy":{"unknown":true}}`,
		"timeout":          `{"policy":{"timeout-seconds":0}}`,
		"source limit":     `{"policy":{"max-source-mib":0}}`,
		"redirect limit":   `{"policy":{"max-redirects":-1}}`,
		"host with port":   `{"policy":{"allowed-hosts":["example.com:443"]}}`,
		"partial wildcard": `{"policy":{"allowed-hosts":["foo*.example.com"]}}`,
		"duplicate origin": `{"credentials":[{"origin":"https://example.com","method":"bearer","secret":"a"},{"origin":"https://EXAMPLE.com:443","method":"bearer","secret":"b"}]}`,
		"credential path":  `{"credentials":[{"origin":"https://example.com/path","method":"bearer","secret":"a"}]}`,
		"header injection": `{"credentials":[{"origin":"https://example.com","method":"api-key","header":"X-Key","secret":"a\nb"}]}`,
		"reserved header":  `{"credentials":[{"origin":"https://example.com","method":"api-key","header":"Host","secret":"a"}]}`,
		"null root":        `null`,
		"empty document":   ``,
		"null policy":      `{"policy":null}`,
		"null list":        `{"sources":null}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPolicyCanBlockPreviouslyConfiguredSource(t *testing.T) {
	input := `{"sources":[{"source":"http://example.com/a","output":"x.yaml","version":1}],"policy":{"allow-http":false}}`
	cfg, err := Parse([]byte(input))
	if err != nil {
		t.Fatalf("policy change must remain representable: %v", err)
	}
	if err := cfg.ValidateSourcesAgainstPolicy(); err == nil {
		t.Fatal("blocked source should fail when checked for a new fetch")
	}
}

func TestValidateUniqueOutputs(t *testing.T) {
	tests := []struct {
		name    string
		outputs []string
		wantErr string
	}{
		{name: "empty list"},
		{name: "distinct outputs", outputs: []string{"a.yaml", "b.yaml"}},
		{name: "case sensitive", outputs: []string{"a.yaml", "A.yaml"}},
		{name: "adjacent duplicates", outputs: []string{"a.yaml", "a.yaml"}, wantErr: "sources 1 and 2"},
		{name: "non-adjacent duplicates", outputs: []string{"a.yaml", "b.yaml", "a.yaml"}, wantErr: "sources 1 and 3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var cfg Config
			for _, output := range test.outputs {
				cfg.Sources = append(cfg.Sources, Source{Output: output})
			}
			err := cfg.ValidateUniqueOutputs()
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected validation error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("validation error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestSecretsAreNotIncludedInValidationErrors(t *testing.T) {
	const secret = "fake-secret-that-must-not-be-logged"
	input := fmt.Sprintf(`{"credentials":[{"origin":"https://example.com","method":"api-key","header":"Host","secret":%q}]}`, secret)
	_, err := Parse([]byte(input))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected validation error: %v", err)
	}
}
