package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// maxBody caps how much of a response we will read. A service that answers a
// small status query with megabytes is misbehaving, and reading it all would
// let one bad endpoint stall the daemon.
const maxBody = 4 << 20 // 4 MiB

// errBodyTooLarge is the failure for a body over maxBody. A truncated body is
// never decoded: the first 4 MiB of a 6 MiB answer can be valid JSON, and
// decoding it would present part of a response as the whole of one.
var errBodyTooLarge = errors.New("response larger than 4 MiB")

// httpClient is the shared transport for HTTP adapters. Timeouts are set per
// request from the service config rather than on the client, so one slow
// service cannot be given a longer budget than its config allows.
//
// Redirects are never followed. The config names the one host each service
// lives on, and a 3xx would send the request, and trust the answer, of a
// host it does not name.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
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
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, redactURL(e.URL), e.Status)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, redactURL(e.URL), e.Status, e.Body)
}

// redactURL hides a password in a URL before it reaches an error string.
// Userinfo is refused at config time, so this is the second line: nothing
// that reaches status output, --json or the daemon log carries a secret.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Redacted()
}

// readBody reads at most maxBody bytes and fails if there were more.
func readBody(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, errBodyTooLarge
	}
	return data, nil
}

// refuseRedirect turns a 3xx into an error naming where the service tried to
// send us, so a misconfigured reverse proxy is diagnosable from the message.
func refuseRedirect(method, url string, resp *http.Response) error {
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return nil
	}
	location := resp.Header.Get("Location")
	if location == "" {
		location = "(no Location header)"
	}
	return fmt.Errorf("%s %s: HTTP %d redirect to %s refused: gpu-bouncer only talks to the endpoint the config names",
		method, url, resp.StatusCode, redactURL(location))
}

// doJSON performs one request and decodes a JSON response into out. A nil body
// sends no request body; a nil out discards the response body.
//
// Every failure path is explicit: a non 2xx status, a body that is not JSON, a
// truncated body. None of them is allowed to look like success, because a
// silent decode failure here would present an empty model list, which reads as
// "this service is holding nothing".
func doJSON(ctx context.Context, client *http.Client, method, rawURL string, body, out any, headers map[string]string) error {
	url := redactURL(rawURL)
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request for %s: %w", url, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
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
		return fmt.Errorf("%s %s: %w", method, url, redactErr(err))
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()

	if err := refuseRedirect(method, url, resp); err != nil {
		return err
	}
	data, err := readBody(resp)
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
func doText(ctx context.Context, client *http.Client, method, rawURL string, body any, headers map[string]string) (string, error) {
	url := redactURL(rawURL)
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("encode request for %s: %w", url, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
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
		return "", fmt.Errorf("%s %s: %w", method, url, redactErr(err))
	}
	defer func() { _ = resp.Body.Close() }()

	if err := refuseRedirect(method, url, resp); err != nil {
		return "", err
	}
	data, err := readBody(resp)
	if err != nil {
		return "", fmt.Errorf("%s %s: read body: %w", method, url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", &httpError{Method: method, URL: url, Status: resp.StatusCode, Body: snippet(data)}
	}
	return string(bytes.TrimSpace(data)), nil
}

// redactErr strips userinfo from the URL inside a transport error. The net/url
// package already redacts the password in its own message, but the wrapped
// error keeps the raw URL and %w would carry it into every caller's string.
func redactErr(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return &url.Error{Op: uerr.Op, URL: redactURL(uerr.URL), Err: uerr.Err}
	}
	return err
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
