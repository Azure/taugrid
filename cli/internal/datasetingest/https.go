package datasetingest

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// HTTPSSource reads public dataset bytes from a strict HTTPS source root. It
// intentionally has no credential or token support: public HTTP(S) URLs with
// query strings, userinfo, or fragments are rejected.
type HTTPSSource struct {
	base   *url.URL
	client *http.Client
}

// NewHTTPSSource constructs a public HTTPS source. client may be nil, in which
// case http.DefaultClient is used.
func NewHTTPSSource(root string, client *http.Client) (*HTTPSSource, error) {
	if err := ValidateAzureURL(root); err != nil {
		return nil, err
	}
	base, err := url.Parse(root)
	if err != nil {
		return nil, fmt.Errorf("parse HTTPS source root: %w", err)
	}
	if !strings.EqualFold(base.Scheme, "https") {
		return nil, fmt.Errorf("HTTPS source root must use https://, got %q", base.Scheme)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPSSource{base: base, client: client}, nil
}

func (s *HTTPSSource) Describe() string { return s.base.String() }

// Open resolves a manifest-relative path below the configured root, performs a
// context-aware GET, and accepts only successful HTTPS responses.
func (s *HTTPSSource) Open(ctx context.Context, relPath string) (io.ReadCloser, int64, error) {
	if err := validateHTTPSRelativePath(relPath); err != nil {
		return nil, 0, err
	}
	target := *s.base
	target.Path = strings.TrimSuffix(s.base.Path, "/") + "/" + relPath
	target.RawPath = ""
	if err := ValidateAzureURL(target.String()); err != nil {
		return nil, 0, fmt.Errorf("resolve HTTPS source path: %w", err)
	}

	client := *s.client
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := ValidateAzureURL(req.URL.String()); err != nil || !strings.EqualFold(req.URL.Scheme, "https") {
			if err == nil {
				err = fmt.Errorf("redirect must use HTTPS")
			}
			return fmt.Errorf("reject redirect to %q: %w", req.URL.String(), err)
		}
		if previousRedirect != nil {
			return previousRedirect(req, via)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create HTTPS source request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s: %w", target.String(), err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("GET %s: unexpected HTTP status %s", target.String(), resp.Status)
	}
	if !strings.EqualFold(resp.Request.URL.Scheme, "https") {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("GET %s: redirect resolved to non-HTTPS URL", target.String())
	}
	return resp.Body, resp.ContentLength, nil
}

func validateHTTPSRelativePath(path string) error {
	if path == "" || strings.HasPrefix(path, "/") {
		return fmt.Errorf("HTTPS source path %q must be relative", path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return fmt.Errorf("HTTPS source path %q contains '..' traversal component", path)
		}
	}
	return nil
}
