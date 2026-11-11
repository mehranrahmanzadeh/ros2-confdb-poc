package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/idna"
)

const (
	defaultTimeoutSeconds = 60
	defaultMaxSourceMiB   = 10
	defaultMaxRedirects   = 5
)

type Source struct {
	URL     string `json:"source"`
	Output  string `json:"output"`
	Version *int64 `json:"version"`
}

type Credential struct {
	Origin   string `json:"origin"`
	Method   string `json:"method"`
	Username string `json:"username,omitempty"`
	Secret   string `json:"secret"`
	Header   string `json:"header,omitempty"`
}

type rawPolicy struct {
	AllowHTTP        *bool    `json:"allow-http,omitempty"`
	AllowHTTPS       *bool    `json:"allow-https,omitempty"`
	AllowlistEnabled *bool    `json:"allowlist-enabled,omitempty"`
	AllowedHosts     []string `json:"allowed-hosts,omitempty"`
	TimeoutSeconds   *int64   `json:"timeout-seconds,omitempty"`
	MaxSourceMiB     *int64   `json:"max-source-mib,omitempty"`
	MaxRedirects     *int     `json:"max-redirects,omitempty"`
}

type rawConfig struct {
	Sources     []Source     `json:"sources,omitempty"`
	Policy      *rawPolicy   `json:"policy,omitempty"`
	Credentials []Credential `json:"credentials,omitempty"`
}

type Policy struct {
	AllowHTTP        bool
	AllowHTTPS       bool
	AllowlistEnabled bool
	AllowedHosts     []string
	Timeout          time.Duration
	MaxSourceBytes   int64
	MaxRedirects     int
}

type Config struct {
	Sources     []Source
	Policy      Policy
	Credentials []Credential
}

// Parse fills in policy defaults and validates the stored shape. Output
// uniqueness and whether sources satisfy the policy are checked separately.
func Parse(data []byte) (Config, error) {
	if trimmed := bytes.TrimSpace(data); len(trimmed) == 0 || trimmed[0] != '{' {
		return Config{}, fmt.Errorf("confdb configuration must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var raw rawConfig
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("invalid confdb configuration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Config{}, fmt.Errorf("invalid trailing confdb data: %w", err)
		}
		return Config{}, fmt.Errorf("multiple confdb configuration documents")
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return Config{}, fmt.Errorf("invalid confdb configuration: %w", err)
	}
	if containsNull(document) {
		return Config{}, fmt.Errorf("confdb configuration must not contain null")
	}

	policy, err := normalizePolicy(raw.Policy)
	if err != nil {
		return Config{}, err
	}
	for i, source := range raw.Sources {
		if source.Version == nil {
			return Config{}, fmt.Errorf("source %d is missing version", i+1)
		}
		if err := ValidateOutput(source.Output); err != nil {
			return Config{}, fmt.Errorf("source %d: %w", i+1, err)
		}
		if _, _, err := ParseURL(source.URL); err != nil {
			return Config{}, fmt.Errorf("source %d: %w", i+1, err)
		}
	}

	seenOrigins := make(map[string]bool)
	for i, credential := range raw.Credentials {
		origin, err := ParseOrigin(credential.Origin)
		if err != nil {
			return Config{}, fmt.Errorf("credential %d: %w", i+1, err)
		}
		key := origin.String()
		if seenOrigins[key] {
			return Config{}, fmt.Errorf("credential %d duplicates an origin", i+1)
		}
		seenOrigins[key] = true
		if err := validateCredential(credential); err != nil {
			return Config{}, fmt.Errorf("credential %d: %w", i+1, err)
		}
	}
	return Config{Sources: raw.Sources, Policy: policy, Credentials: raw.Credentials}, nil
}

func containsNull(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case []any:
		for _, child := range value {
			if containsNull(child) {
				return true
			}
		}
	case map[string]any:
		for _, child := range value {
			if containsNull(child) {
				return true
			}
		}
	}
	return false
}

func normalizePolicy(raw *rawPolicy) (Policy, error) {
	policy := Policy{
		AllowHTTP:      true,
		AllowHTTPS:     true,
		Timeout:        defaultTimeoutSeconds * time.Second,
		MaxSourceBytes: defaultMaxSourceMiB * 1024 * 1024,
		MaxRedirects:   defaultMaxRedirects,
	}
	if raw == nil {
		return policy, nil
	}
	if raw.AllowHTTP != nil {
		policy.AllowHTTP = *raw.AllowHTTP
	}
	if raw.AllowHTTPS != nil {
		policy.AllowHTTPS = *raw.AllowHTTPS
	}
	if raw.AllowlistEnabled != nil {
		policy.AllowlistEnabled = *raw.AllowlistEnabled
	}
	for i, entry := range raw.AllowedHosts {
		normalized, err := normalizeAllowedHost(entry)
		if err != nil {
			return Policy{}, fmt.Errorf("allowlist entry %d: %w", i+1, err)
		}
		policy.AllowedHosts = append(policy.AllowedHosts, normalized)
	}
	if raw.TimeoutSeconds != nil {
		if *raw.TimeoutSeconds <= 0 || *raw.TimeoutSeconds > math.MaxInt64/int64(time.Second) {
			return Policy{}, fmt.Errorf("timeout-seconds must be a positive duration")
		}
		policy.Timeout = time.Duration(*raw.TimeoutSeconds) * time.Second
	}
	if raw.MaxSourceMiB != nil {
		if *raw.MaxSourceMiB <= 0 || *raw.MaxSourceMiB > (math.MaxInt64-1)/(1024*1024) {
			return Policy{}, fmt.Errorf("max-source-mib must be a positive size")
		}
		policy.MaxSourceBytes = *raw.MaxSourceMiB * 1024 * 1024
	}
	if raw.MaxRedirects != nil {
		if *raw.MaxRedirects < 0 {
			return Policy{}, fmt.Errorf("max-redirects must not be negative")
		}
		policy.MaxRedirects = *raw.MaxRedirects
	}
	return policy, nil
}

func ValidateOutput(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 ||
		filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("output must be a filename, not a path")
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return fmt.Errorf("output filename contains a control character")
		}
	}
	return nil
}

func validateCredential(credential Credential) error {
	if credential.Secret == "" {
		return fmt.Errorf("secret must not be empty")
	}
	if strings.ContainsAny(credential.Secret, "\r\n") {
		return fmt.Errorf("secret must not contain line breaks")
	}
	switch credential.Method {
	case "basic":
		if credential.Username == "" || strings.ContainsAny(credential.Username, ":\r\n") {
			return fmt.Errorf("basic authentication requires a valid username")
		}
		if credential.Header != "" {
			return fmt.Errorf("basic authentication does not use a custom header")
		}
	case "bearer":
		if credential.Username != "" || credential.Header != "" {
			return fmt.Errorf("bearer authentication does not use username or header")
		}
		if !validHeaderValue(credential.Secret) {
			return fmt.Errorf("bearer token contains invalid header characters")
		}
	case "api-key":
		if credential.Username != "" {
			return fmt.Errorf("API-key authentication does not use a username")
		}
		if !validHeaderName(credential.Header) || reservedHeader(credential.Header) {
			return fmt.Errorf("API-key authentication requires a permitted HTTP header name")
		}
		if !validHeaderValue(credential.Secret) {
			return fmt.Errorf("API key contains invalid header characters")
		}
	default:
		return fmt.Errorf("authentication method must be basic, bearer, or api-key")
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z') &&
			!(c >= 'A' && c <= 'Z') &&
			!(c >= '0' && c <= '9') &&
			!strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			return false
		}
	}
	return true
}

func validHeaderValue(value string) bool {
	for _, c := range value {
		if (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

func reservedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "content-length", "cookie", "host", "proxy-authorization",
		"proxy-connection", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

type Origin struct {
	Scheme string
	Host   string
	Port   string
}

func (origin Origin) String() string {
	return origin.Scheme + "://" + net.JoinHostPort(origin.Host, origin.Port)
}

// ParseURL validates an HTTP(S) URL and returns its normalized origin.
func ParseURL(raw string) (*url.URL, Origin, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil {
		return nil, Origin{}, fmt.Errorf("source must be an HTTP(S) URL without embedded credentials")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, Origin{}, fmt.Errorf("source URL scheme must be HTTP or HTTPS")
	}
	host, err := normalizeHost(u.Hostname())
	if err != nil {
		return nil, Origin{}, fmt.Errorf("source URL has an invalid hostname")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	} else {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return nil, Origin{}, fmt.Errorf("source URL has an invalid port")
		}
		port = strconv.Itoa(number)
	}
	return u, Origin{Scheme: u.Scheme, Host: host, Port: port}, nil
}

func ParseOrigin(raw string) (Origin, error) {
	u, origin, err := ParseURL(raw)
	if err != nil {
		return Origin{}, fmt.Errorf("origin must be an HTTP(S) scheme, host, and optional port")
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Origin{}, fmt.Errorf("origin must not contain a path, query, or fragment")
	}
	return origin, nil
}

func normalizeHost(host string) (string, error) {
	if host == "" {
		return "", fmt.Errorf("empty hostname")
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return address.String(), nil
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > 253 {
		return "", fmt.Errorf("invalid hostname")
	}
	for _, label := range strings.Split(ascii, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("invalid hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
				return "", fmt.Errorf("invalid hostname")
			}
		}
	}
	return ascii, nil
}

func normalizeAllowedHost(entry string) (string, error) {
	wildcard := strings.HasPrefix(entry, "*.")
	if wildcard {
		entry = strings.TrimPrefix(entry, "*.")
	}
	host, err := normalizeHost(entry)
	if err != nil {
		return "", fmt.Errorf("expected a hostname or IP address without a port")
	}
	if wildcard {
		if _, err := netip.ParseAddr(host); err == nil {
			return "", fmt.Errorf("wildcards cannot be used with IP addresses")
		}
		return "*." + host, nil
	}
	return host, nil
}

func (policy Policy) CheckURL(raw string) (Origin, error) {
	_, origin, err := ParseURL(raw)
	if err != nil {
		return Origin{}, err
	}
	if origin.Scheme == "http" && !policy.AllowHTTP {
		return Origin{}, fmt.Errorf("HTTP is disabled by URL policy")
	}
	if origin.Scheme == "https" && !policy.AllowHTTPS {
		return Origin{}, fmt.Errorf("HTTPS is disabled by URL policy")
	}
	if policy.AllowlistEnabled {
		allowed := false
		for _, entry := range policy.AllowedHosts {
			if entry == origin.Host ||
				(strings.HasPrefix(entry, "*.") &&
					strings.HasSuffix(origin.Host, "."+strings.TrimPrefix(entry, "*."))) {
				allowed = true
				break
			}
		}
		if !allowed {
			return Origin{}, fmt.Errorf("source hostname is not permitted by URL policy")
		}
	}
	return origin, nil
}

func (config Config) ValidateSourcesAgainstPolicy() error {
	for i, source := range config.Sources {
		if _, err := config.Policy.CheckURL(source.URL); err != nil {
			return fmt.Errorf("source %d: %w", i+1, err)
		}
	}
	return nil
}

func (config Config) ValidateUniqueOutputs() error {
	seen := make(map[string]int, len(config.Sources))
	for i, source := range config.Sources {
		if previous, ok := seen[source.Output]; ok {
			return fmt.Errorf("sources %d and %d have the same output", previous+1, i+1)
		}
		seen[source.Output] = i
	}
	return nil
}

func SourcesEqual(left, right []Source) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].URL != right[i].URL || left[i].Output != right[i].Output ||
			left[i].Version == nil || right[i].Version == nil ||
			*left[i].Version != *right[i].Version {
			return false
		}
	}
	return true
}

func (config Config) CredentialFor(origin Origin) (*Credential, error) {
	for i := range config.Credentials {
		candidate, err := ParseOrigin(config.Credentials[i].Origin)
		if err != nil {
			return nil, fmt.Errorf("credential %d has an invalid origin: %w", i+1, err)
		}
		if candidate == origin {
			return &config.Credentials[i], nil
		}
	}
	return nil, nil
}

func RedactSecrets(message string, credentials []Credential) string {
	for _, credential := range credentials {
		if credential.Secret != "" {
			message = strings.ReplaceAll(message, credential.Secret, "[redacted]")
			escaped := strconv.Quote(credential.Secret)
			escaped = escaped[1 : len(escaped)-1]
			if escaped != credential.Secret {
				message = strings.ReplaceAll(message, escaped, "[redacted]")
			}
		}
	}
	return message
}
