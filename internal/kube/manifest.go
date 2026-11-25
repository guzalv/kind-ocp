package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// maxManifestBytes caps how much of a manifest response is read. The
// pinned manifests are a few hundred kilobytes; anything tens of
// megabytes is a misresolved URL, not a manifest.
const maxManifestBytes = 64 << 20

// maxRedirects bounds the redirect chain of one manifest fetch.
// Go's default policy allows the same ten; naming the bound keeps it a
// decision of this package rather than an accident of the zero value.
const maxRedirects = 10

// FetchManifest GETs one manifest file from rawURL and returns its
// bytes, undecoded — callers that verify a digest or cache the file
// need the exact bytes that were fetched; pair it with DecodeManifests
// to get objects.
//
// The URL must be absolute https, and so must every URL the fetch is
// redirected to: kind-ocp's manifests are pinned upstream artifacts
// fetched over TLS, and this is the one place a configured value
// becomes a network request, so a non-TLS or relative URL is refused
// rather than silently honored, and a redirect to a non-https URL is
// refused before that request is issued. What comes back here ends up
// applied with cluster-admin credentials, so no hop may downgrade the
// transport to unauthenticated plain text. Redirects to other https
// hosts are allowed — GitHub serves its release assets from
// release-assets.githubusercontent.com — and the chain is bounded by
// maxRedirects.
//
// The fetcher owns the http.Client it uses, so the redirect policy
// above is not a caller decision: no caller can re-scope or weaken it
// by handing in a client of its own. Callers inject only what the
// fetch itself needs — the transport (nil means http.DefaultTransport;
// tests replay canned responses through a fake one, keeping them
// network-free) and the timeout bounding the whole fetch, redirects
// and body read included. The timeout must be positive: a zero timeout
// would silently make the fetch unbounded.
func FetchManifest(ctx context.Context, transport http.RoundTripper, timeout time.Duration, rawURL string) ([]byte, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("manifest fetch timeout must be positive, got %s", timeout)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("manifest URL %q: %w", rawURL, err)
	}
	if u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("manifest URL %q: must be an absolute https URL", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("building request for %s: %w", rawURL, err)
	}
	client := &http.Client{
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: httpsOnlyRedirect,
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching manifest %s: unexpected status %s%s",
			rawURL, resp.Status, bodyExcerpt(resp.Body))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("fetching manifest %s: reading body: %w", rawURL, err)
	}
	if len(body) > maxManifestBytes {
		return nil, fmt.Errorf("fetching manifest %s: body exceeds %d bytes", rawURL, maxManifestBytes)
	}
	return body, nil
}

// httpsOnlyRedirect is the redirect policy of the client FetchManifest
// builds for itself. It refuses a redirect to a non-https URL —
// net/http calls the policy before issuing the redirected request, so
// a refused hop never reaches the transport — and bounds the chain at
// maxRedirects. Because that client never leaves FetchManifest, the
// policy cannot be composed with, overridden by, or delegated to
// anything a caller controls.
func httpsOnlyRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-https URL %s", req.URL)
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return nil
}

// DecodeManifests decodes a multi-document YAML (or JSON) manifest into
// unstructured objects in document order. Order matters for apply:
// objects can only be created after the objects they reference (the
// OLM manifest puts its namespaces before the deployments in them), so
// the decoded order is the apply order. Empty documents (comment-only
// or separator-only) are skipped. A document that is not an object,
// and a stream with no objects at all, are errors: both mean the
// source is not the manifest the caller pinned.
func DecodeManifests(r io.Reader) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	var objs []*unstructured.Unstructured
	for {
		var obj unstructured.Unstructured
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decoding document %d: %w", len(objs)+1, err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		objs = append(objs, &obj)
	}
	if len(objs) == 0 {
		return nil, errors.New("no objects in manifest")
	}
	return objs, nil
}

// bodyExcerpt renders a short, single-line excerpt of a non-200 response
// body. The common failure is a 404 from a bad release version, and the
// excerpt names the mistake without dumping an HTML page at the user.
func bodyExcerpt(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil || len(b) == 0 {
		return ""
	}
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return " (body: " + s + ")"
}
