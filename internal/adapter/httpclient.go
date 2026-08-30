package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// maxBody caps how much of a response we will read. A service that answers a
// small status query with megabytes is misbehaving, and reading it all would
// let one bad endpoint stall the daemon.
const maxBody = 4 << 20 // 4 MiB

// httpClient is the shared transport for HTTP adapters. Timeouts are set per
// request from the service config rather than on the client, so one slow
// service cannot be given a longer budget than its config allows.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}
}

// httpError carries the status code so callers can tell a 404 from a refusal.
type httpError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *httpError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.URL, e.Status)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, e.Body)
}

// doJSON performs one request and decodes a JSON response into out. A nil body
// sends no request body; a nil out discards the response body.
//
// Every failure path is explicit: a non 2xx status, a body that is not JSON, a
// truncated body. None of them is allowed to look like success, because a
// silent decode failure here would present an empty model list, which reads as
// "this service is holding nothing".
func doJSON(ctx context.Context, client *http.Client, method, url string, body, out any, headers map[string]string) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request for %s: %w", url, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, url, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{Method: method, URL: url, Status: resp.StatusCode, Body: snippet(data)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: response is not the expected JSON: %w (body: %s)", method, url, err, snippet(data))
	}
	return nil
}

// doText performs one request and returns the body as a trimmed string. It is
// for endpoints that answer in plain text rather than JSON.
func doText(ctx context.Context, client *http.Client, method, url string, body any, headers map[string]string) (string, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("encode request for %s: %w", url, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return "", fmt.Errorf("build request for %s: %w", url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("%s %s: read body: %w", method, url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &httpError{Method: method, URL: url, Status: resp.StatusCode, Body: snippet(data)}
	}
	return string(bytes.TrimSpace(data)), nil
}

// snippet trims a body for an error message. Whole bodies in errors turn one
// misconfigured endpoint into pages of log noise.
func snippet(data []byte) string {
	const limit = 200
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > limit {
		return string(trimmed[:limit]) + "..."
	}
	return string(trimmed)
}
