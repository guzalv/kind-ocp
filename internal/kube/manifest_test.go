package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDecodeManifests(t *testing.T) {
	t.Run("multi-document YAML preserves order", func(t *testing.T) {
		objs, err := DecodeManifests(strings.NewReader(`
apiVersion: v1
kind: Namespace
metadata:
  name: olm
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: olm-operator
  namespace: olm
`))
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) != 2 {
			t.Fatalf("decoded %d objects, want 2", len(objs))
		}
		if objs[0].GetKind() != "Namespace" || objs[0].GetName() != "olm" {
			t.Errorf("first object = %s/%s, want Namespace/olm", objs[0].GetKind(), objs[0].GetName())
		}
		if objs[1].GetKind() != "Deployment" || objs[1].GetName() != "olm-operator" || objs[1].GetNamespace() != "olm" {
			t.Errorf("second object = %s %s/%s, want Deployment olm/olm-operator",
				objs[1].GetKind(), objs[1].GetNamespace(), objs[1].GetName())
		}
	})

	t.Run("comment-only and separator-only documents are skipped", func(t *testing.T) {
		objs, err := DecodeManifests(strings.NewReader(`
# leading comment
---
# just a comment, no object
---
apiVersion: v1
kind: Namespace
metadata:
  name: olm
---
---

`))
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) != 1 || objs[0].GetName() != "olm" {
			t.Fatalf("decoded %v, want only the namespace", objs)
		}
	})

	t.Run("single JSON object", func(t *testing.T) {
		objs, err := DecodeManifests(strings.NewReader(
			`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"olm"}}`))
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) != 1 || objs[0].GetName() != "olm" {
			t.Fatalf("decoded %v, want the namespace", objs)
		}
	})

	t.Run("non-object document is an error", func(t *testing.T) {
		_, err := DecodeManifests(strings.NewReader("- just\n- a list\n"))
		if err == nil || !strings.Contains(err.Error(), "decoding document 1") {
			t.Fatalf("DecodeManifests(list) = %v, want a document error", err)
		}
	})

	t.Run("malformed document is an error", func(t *testing.T) {
		_, err := DecodeManifests(strings.NewReader("apiVersion: [unclosed\n"))
		if err == nil {
			t.Fatal("DecodeManifests(malformed) must fail")
		}
	})

	t.Run("no objects is an error", func(t *testing.T) {
		for name, body := range map[string]string{
			"empty":           "",
			"comments only":   "# nothing here\n",
			"empty documents": "---\n---\n",
		} {
			if _, err := DecodeManifests(strings.NewReader(body)); err == nil ||
				!strings.Contains(err.Error(), "no objects") {
				t.Errorf("DecodeManifests(%s) = %v, want a no-objects error", name, err)
			}
		}
	})
}

// replayTransport answers every request with one canned response and
// records the requests it saw, so manifest fetching is tested without
// network: it is the Transport of an otherwise ordinary http.Client.
type replayTransport struct {
	status int
	body   io.Reader
	err    error

	requests []*http.Request
}

func (t *replayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, req)
	if t.err != nil {
		return nil, t.err
	}
	body := t.body
	if body == nil {
		body = strings.NewReader("")
	}
	return &http.Response{
		StatusCode: t.status,
		Status:     fmt.Sprintf("%d %s", t.status, http.StatusText(t.status)),
		Header:     http.Header{"Content-Type": []string{"application/yaml"}},
		Body:       io.NopCloser(body),
		Request:    req,
	}, nil
}

// namespaceManifest is a minimal decodable manifest body.
const namespaceManifest = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: olm\n"

// testFetchTimeout bounds unit-test fetches. No unit test stalls long
// enough to hit it; only the timeout subtest passes a real bound.
const testFetchTimeout = 30 * time.Second

func TestFetchManifest(t *testing.T) {
	const url = "https://example.test/release/crds.yaml"

	t.Run("fetches the body bytes", func(t *testing.T) {
		tr := &replayTransport{status: http.StatusOK, body: strings.NewReader(namespaceManifest)}
		body, err := FetchManifest(context.Background(), tr, testFetchTimeout, url)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != namespaceManifest {
			t.Fatalf("body = %q, want the response body verbatim", body)
		}
		if len(tr.requests) != 1 {
			t.Fatalf("requests = %d, want 1", len(tr.requests))
		}
		req := tr.requests[0]
		if req.Method != http.MethodGet || req.URL.String() != url {
			t.Errorf("request = %s %s, want GET %s", req.Method, req.URL, url)
		}
	})

	t.Run("non-200 status names the body", func(t *testing.T) {
		tr := &replayTransport{
			status: http.StatusNotFound,
			body:   strings.NewReader("Not Found\n"),
		}
		_, err := FetchManifest(context.Background(), tr, testFetchTimeout, url)
		if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "Not Found") {
			t.Fatalf("FetchManifest(404) = %v, want the status and body excerpt", err)
		}
	})

	t.Run("transport error is wrapped", func(t *testing.T) {
		tr := &replayTransport{err: errors.New("connection refused")}
		_, err := FetchManifest(context.Background(), tr, testFetchTimeout, url)
		if err == nil || !strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("FetchManifest(transport error) = %v, want the wrapped error", err)
		}
	})

	t.Run("non-https URL is refused without a request", func(t *testing.T) {
		tr := &replayTransport{status: http.StatusOK}
		for _, bad := range []string{
			"http://example.test/crds.yaml",
			"example.test/crds.yaml",
			"ftp://example.test/crds.yaml",
		} {
			if _, err := FetchManifest(context.Background(), tr, testFetchTimeout, bad); err == nil ||
				!strings.Contains(err.Error(), "https") {
				t.Errorf("FetchManifest(%q) = %v, want an https rejection", bad, err)
			}
		}
		if len(tr.requests) != 0 {
			t.Errorf("%d requests were issued for refused URLs, want 0", len(tr.requests))
		}
	})

	t.Run("oversized body is refused", func(t *testing.T) {
		tr := &replayTransport{status: http.StatusOK, body: &endlessReader{}}
		_, err := FetchManifest(context.Background(), tr, testFetchTimeout, url)
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("FetchManifest(oversized) = %v, want a size-cap error", err)
		}
	})

	t.Run("a non-positive timeout is refused without a request", func(t *testing.T) {
		tr := &replayTransport{status: http.StatusOK}
		for _, timeout := range []time.Duration{0, -time.Second} {
			_, err := FetchManifest(context.Background(), tr, timeout, url)
			if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
				t.Errorf("FetchManifest(timeout %s) = %v, want a timeout rejection", timeout, err)
			}
		}
		if len(tr.requests) != 0 {
			t.Errorf("%d requests were issued for refused timeouts, want 0", len(tr.requests))
		}
	})

	t.Run("the timeout bounds a stalled fetch", func(t *testing.T) {
		// A transport that never answers must not outlive the fetch's
		// timeout: the fetcher wires Client.Timeout into the request,
		// so the stall ends at the deadline, not at the transport's
		// leisure.
		tr := stallingTransport{stall: 3 * time.Second}
		start := time.Now()
		_, err := FetchManifest(context.Background(), tr, 50*time.Millisecond, url)
		if err == nil {
			t.Fatal("a stalled fetch must fail, not hang")
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("the stalled fetch took %s; the timeout did not bound it", elapsed)
		}
	})
}

// cannedResponse is one scripted transport answer: a status, an
// optional redirect Location, and a body.
type cannedResponse struct {
	status   int
	location string
	body     string
}

// scriptedTransport answers request i with responses[i-1] and repeats
// the last response beyond the script, recording every request it
// saw, so redirect handling is tested without network. The repeated
// response doubles as a poison pill: a redirect the policy should
// have refused still gets an answer, and the request log shows it.
type scriptedTransport struct {
	responses []cannedResponse
	requests  []*http.Request
}

func (t *scriptedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, req)
	i := len(t.requests) - 1
	if i >= len(t.responses) {
		i = len(t.responses) - 1
	}
	r := t.responses[i]
	header := http.Header{"Content-Type": []string{"application/yaml"}}
	if r.location != "" {
		header.Set("Location", r.location)
	}
	return &http.Response{
		StatusCode: r.status,
		Status:     fmt.Sprintf("%d %s", r.status, http.StatusText(r.status)),
		Header:     header,
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

const releaseURL = "https://github.example.test/olm/releases/download/v0.46.0/crds.yaml"

// The fetch must validate https on every hop, not only the initial
// URL: net/http's default redirect
// policy counts hops but never checks their scheme — so one
// HTTP Location header away, unauthenticated content could reach the
// cluster-admin apply path. The redirect policy must refuse a non-https
// hop before the request is issued, on every hop (the second-hop case
// below pins the per-hop invariant), follow https hops across hosts
// (GitHub serves release assets from release-assets.githubusercontent.com),
// and keep the chain bounded.
func TestFetchManifestRedirects(t *testing.T) {
	t.Run("rejects a redirect to http before it reaches the transport", func(t *testing.T) {
		tr := &scriptedTransport{responses: []cannedResponse{
			{status: http.StatusFound, location: "http://release-assets.example.test/crds.yaml"},
			{status: http.StatusOK, body: namespaceManifest}, // poison: must never be served
		}}
		_, err := FetchManifest(context.Background(), tr, testFetchTimeout, releaseURL)
		if err == nil || !strings.Contains(err.Error(), "non-https") {
			t.Fatalf("FetchManifest(http redirect) = %v, want a non-https refusal", err)
		}
		if len(tr.requests) != 1 {
			t.Fatalf("%d requests reached the transport, want only the initial https request",
				len(tr.requests))
		}
		if got := tr.requests[0].URL.Scheme; got != "https" {
			t.Errorf("the one issued request is %s, not https", got)
		}
	})

	t.Run("follows a cross-host https redirect", func(t *testing.T) {
		tr := &scriptedTransport{responses: []cannedResponse{
			{status: http.StatusFound, location: "https://release-assets.example.test/asset/crds.yaml?sig=abc"},
			{status: http.StatusOK, body: namespaceManifest},
		}}
		body, err := FetchManifest(context.Background(), tr, testFetchTimeout, releaseURL)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != namespaceManifest {
			t.Fatalf("body = %q, want the redirect target's body", body)
		}
		if len(tr.requests) != 2 {
			t.Fatalf("%d requests, want the initial one plus the redirect target", len(tr.requests))
		}
		second := tr.requests[1].URL
		if second.Scheme != "https" || second.Host != "release-assets.example.test" {
			t.Errorf("redirect target = %s, want https on release-assets.example.test", second)
		}
	})

	t.Run("refuses an http hop after a followed https hop", func(t *testing.T) {
		// The per-hop invariant, not just a first-hop one: the first
		// redirect is a legal https hop and must be followed before the
		// second, http hop is refused.
		tr := &scriptedTransport{responses: []cannedResponse{
			{status: http.StatusFound, location: "https://cdn.example.test/a"},
			{status: http.StatusFound, location: "http://release-assets.example.test/b"},
			{status: http.StatusOK, body: namespaceManifest}, // poison: must never be served
		}}
		_, err := FetchManifest(context.Background(), tr, testFetchTimeout, releaseURL)
		if err == nil || !strings.Contains(err.Error(), "non-https") {
			t.Fatalf("FetchManifest(second-hop http redirect) = %v, want a non-https refusal", err)
		}
		if len(tr.requests) != 2 {
			t.Fatalf("%d requests reached the transport, want the initial one plus the https hop only",
				len(tr.requests))
		}
		for i, req := range tr.requests {
			if req.URL.Scheme != "https" {
				t.Errorf("request %d was issued with scheme %q, want https", i, req.URL.Scheme)
			}
		}
	})

	t.Run("refuses a redirect to another non-https scheme", func(t *testing.T) {
		tr := &scriptedTransport{responses: []cannedResponse{
			{status: http.StatusFound, location: "ftp://release-assets.example.test/crds.yaml"},
		}}
		_, err := FetchManifest(context.Background(), tr, testFetchTimeout, releaseURL)
		if err == nil || !strings.Contains(err.Error(), "non-https") {
			t.Fatalf("FetchManifest(ftp redirect) = %v, want a non-https refusal", err)
		}
		if len(tr.requests) != 1 {
			t.Fatalf("%d requests reached the transport, want 1", len(tr.requests))
		}
	})

	t.Run("resolves a protocol-relative Location to https", func(t *testing.T) {
		// "//host/path" inherits the request's scheme, resolving to
		// cross-host https — allowed by design. It must not be parsed
		// naively as an http URL (a classic downgrade confusion).
		tr := &scriptedTransport{responses: []cannedResponse{
			{status: http.StatusFound, location: "//release-assets.example.test/asset/crds.yaml"},
			{status: http.StatusOK, body: namespaceManifest},
		}}
		body, err := FetchManifest(context.Background(), tr, testFetchTimeout, releaseURL)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != namespaceManifest {
			t.Fatalf("body = %q, want the redirect target's body", body)
		}
		if len(tr.requests) != 2 {
			t.Fatalf("%d requests, want the initial one plus the redirect target", len(tr.requests))
		}
		second := tr.requests[1].URL
		if second.Scheme != "https" || second.Host != "release-assets.example.test" {
			t.Errorf("redirect target = %s, want https on release-assets.example.test", second)
		}
	})

	t.Run("resolves a relative Location against the request URL", func(t *testing.T) {
		tr := &scriptedTransport{responses: []cannedResponse{
			{status: http.StatusMovedPermanently, location: "/other/path/crds.yaml"},
			{status: http.StatusOK, body: namespaceManifest},
		}}
		_, err := FetchManifest(context.Background(), tr, testFetchTimeout, releaseURL)
		if err != nil {
			t.Fatal(err)
		}
		if len(tr.requests) != 2 || tr.requests[1].URL.String() != "https://github.example.test/other/path/crds.yaml" {
			t.Fatalf("requests = %v, want the relative Location resolved against the request URL",
				reqURLs(tr.requests))
		}
	})

	t.Run("bounds the redirect chain", func(t *testing.T) {
		tr := &scriptedTransport{responses: []cannedResponse{
			{status: http.StatusFound, location: "https://example.test/loop"},
		}}
		_, err := FetchManifest(context.Background(), tr, testFetchTimeout, releaseURL)
		if err == nil || !strings.Contains(err.Error(), "stopped after") {
			t.Fatalf("FetchManifest(redirect loop) = %v, want a redirect-limit error", err)
		}
		if len(tr.requests) != maxRedirects {
			t.Fatalf("%d requests, want the chain bounded at %d", len(tr.requests), maxRedirects)
		}
	})
}

// reqURLs renders a request log for failure messages.
func reqURLs(reqs []*http.Request) []string {
	urls := make([]string, 0, len(reqs))
	for _, r := range reqs {
		urls = append(urls, r.URL.String())
	}
	return urls
}

// stallingTransport never answers: it holds the request until its
// context is cancelled (or a long stall elapses), to prove the fetch
// timeout bounds a transport that would otherwise hang forever.
type stallingTransport struct{ stall time.Duration }

func (t stallingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case <-time.After(t.stall):
		return nil, errors.New("the stall elapsed; the fetch should have timed out before this")
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

// endlessReader produces an unbounded body of 'x' bytes without
// allocating them, to exercise the size cap.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// The decoded objects must be independent of the response body buffer:
// a caller mutating one object must not corrupt another.
func TestDecodedObjectsAreIndependent(t *testing.T) {
	tr := &replayTransport{
		status: http.StatusOK,
		body: strings.NewReader(`
apiVersion: v1
kind: Namespace
metadata:
  name: a
---
apiVersion: v1
kind: Namespace
metadata:
  name: b
`),
	}
	body, err := FetchManifest(context.Background(), tr, testFetchTimeout, "https://example.test/release/olm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	objs, err := DecodeManifests(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	objs[0].Object["metadata"].(map[string]any)["name"] = "mutated"
	if objs[1].GetName() != "b" {
		t.Errorf("mutating the first object changed the second (%q)", objs[1].GetName())
	}
}
