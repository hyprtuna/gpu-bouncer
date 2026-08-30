package adapter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A redirect must never be followed. The config names one host per service,
// and following a 3xx would send a request to, and trust an answer from, a
// host the config never named.
func TestDoJSONRefusesRedirects(t *testing.T) {
	var secondHits atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		_, _ = io.WriteString(w, `{"done":true,"done_reason":"unload"}`)
	}))
	t.Cleanup(second.Close)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/x", http.StatusFound)
	}))
	t.Cleanup(first.Close)

	client := newHTTPClient()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var out map[string]any
		err := doJSON(context.Background(), client, method, first.URL+"/api/generate", map[string]int{"keep_alive": 0}, &out, nil)
		if err == nil {
			t.Fatalf("%s: a 302 was followed and its answer accepted", method)
		}
		for _, want := range []string{"HTTP 302", "redirect", second.URL + "/x", "refused"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error = %q, want it to contain %q", method, err, want)
			}
		}
	}
	if _, err := doText(context.Background(), client, http.MethodGet, first.URL+"/health", nil, nil); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("doText followed or accepted a redirect: %v", err)
	}
	if n := secondHits.Load(); n != 0 {
		t.Errorf("the redirect target received %d request(s), want 0", n)
	}
}

// A body over the cap is a failure, not a silently truncated success: the
// first 4 MiB of a larger answer can be valid JSON on its own.
func TestDoJSONRejectsOversizedBody(t *testing.T) {
	// Valid JSON followed by whitespace padding, which json.Unmarshal accepts,
	// so only the size check can tell the two cases apart.
	const head = `{"version":"9.9.9"}`
	for _, tt := range []struct {
		name  string
		total int
		ok    bool
	}{
		{"exactly the cap", maxBody, true},
		{"one byte over the cap", maxBody + 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := head + strings.Repeat(" ", tt.total-len(head))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(srv.Close)

			var out struct {
				Version string `json:"version"`
			}
			err := doJSON(context.Background(), newHTTPClient(), http.MethodGet, srv.URL+"/api/version", nil, &out, nil)
			if tt.ok {
				if err != nil || out.Version != "9.9.9" {
					t.Fatalf("a body at the cap failed: %v (version %q)", err, out.Version)
				}
				return
			}
			if err == nil {
				t.Fatalf("a body over the cap decoded as version %q", out.Version)
			}
			if !errors.Is(err, errBodyTooLarge) || !strings.Contains(err.Error(), "response larger than 4 MiB") {
				t.Errorf("error = %q, want response larger than 4 MiB", err)
			}
			if _, err := doText(context.Background(), newHTTPClient(), http.MethodGet, srv.URL+"/health", nil, nil); !errors.Is(err, errBodyTooLarge) {
				t.Errorf("doText error = %v, want errBodyTooLarge", err)
			}
		})
	}
}

// A password in an endpoint must never reach an error string, whichever path
// produced the error.
func TestErrorsRedactUserinfo(t *testing.T) {
	e := &httpError{Method: "GET", URL: "http://user:hunter2@127.0.0.1:1/api/version", Status: 500, Body: "nope"}
	if got := e.Error(); strings.Contains(got, "hunter2") || !strings.Contains(got, "user:xxxxx@") {
		t.Errorf("httpError = %q, want the password redacted", got)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	withCreds := strings.Replace(srv.URL, "http://", "http://user:hunter2@", 1)
	err := doJSON(context.Background(), newHTTPClient(), http.MethodGet, withCreds+"/api/version", nil, &struct{}{}, nil)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("doJSON error = %v, want a failure without the password", err)
	}
	srv.Close()
	err = doJSON(context.Background(), newHTTPClient(), http.MethodGet, withCreds+"/api/version", nil, &struct{}{}, nil)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("transport error = %v, want a failure without the password", err)
	}
}
