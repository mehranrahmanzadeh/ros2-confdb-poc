package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/config"
)

type Downloader struct {
	Transport http.RoundTripper
}

func (downloader Downloader) Fetch(ctx context.Context, rawURL string, cfg config.Config) ([]byte, error) {
	if _, err := cfg.Policy.CheckURL(rawURL); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot create source request (%T)", withoutURL(err))
	}
	if err := applyCredential(request, cfg); err != nil {
		return nil, err
	}

	client := &http.Client{
		Transport: downloader.Transport,
		Timeout:   cfg.Policy.Timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > cfg.Policy.MaxRedirects {
				return redirectError{fmt.Errorf("redirect limit of %d exceeded", cfg.Policy.MaxRedirects)}
			}
			clearCredentials(request, cfg.Credentials)
			if err := applyCredential(request, cfg); err != nil {
				return redirectError{err}
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, requestError(err, cfg.Credentials)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("source returned HTTP status %d", response.StatusCode)
	}
	for _, encodings := range response.Header.Values("Content-Encoding") {
		for _, encoding := range strings.Split(encodings, ",") {
			if !strings.EqualFold(strings.TrimSpace(encoding), "identity") {
				return nil, fmt.Errorf("source uses an unsupported content encoding")
			}
		}
	}
	if cfg.Policy.MaxSourceBytes <= 0 {
		return nil, fmt.Errorf("source-size limit must be positive")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, cfg.Policy.MaxSourceBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("source response timed out")
		}
		if errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("source response canceled")
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("source response ended unexpectedly")
		}
		return nil, fmt.Errorf("cannot read source response (%T)", withoutURL(err))
	}
	if int64(len(body)) > cfg.Policy.MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds the %d-byte size limit", cfg.Policy.MaxSourceBytes)
	}
	return body, nil
}

func applyCredential(request *http.Request, cfg config.Config) error {
	origin, err := cfg.Policy.CheckURL(request.URL.String())
	if err != nil {
		return err
	}
	credential, err := cfg.CredentialFor(origin)
	if err != nil {
		return err
	}
	if credential == nil {
		return nil
	}
	switch credential.Method {
	case "basic":
		request.SetBasicAuth(credential.Username, credential.Secret)
	case "bearer":
		request.Header.Set("Authorization", "Bearer "+credential.Secret)
	case "api-key":
		request.Header.Set(credential.Header, credential.Secret)
	default:
		return fmt.Errorf("unsupported authentication method")
	}
	return nil
}

func clearCredentials(request *http.Request, credentials []config.Credential) {
	request.Header.Del("Authorization")
	for _, credential := range credentials {
		if credential.Method == "api-key" {
			request.Header.Del(credential.Header)
		}
	}
}

func withoutURL(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) {
		return urlError.Err
	}
	return err
}

type redirectError struct {
	err error
}

func (err redirectError) Error() string {
	return err.err.Error()
}

func (err redirectError) Unwrap() error {
	return err.err
}

func requestError(err error, credentials []config.Credential) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("source request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("source request timed out")
	}
	var redirect redirectError
	if errors.As(err, &redirect) {
		return fmt.Errorf("source redirect rejected: %s", config.RedactSecrets(redirect.Error(), credentials))
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return fmt.Errorf("source DNS resolution failed")
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return fmt.Errorf("source TLS certificate verification failed")
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return fmt.Errorf("source request timed out")
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("source connection was refused")
	}
	return fmt.Errorf("source request failed (%T)", withoutURL(err))
}
