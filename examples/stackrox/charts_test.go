package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTransport answers every request with a canned response and records
// the URLs it saw, so the fetch tests assert what was (and was not)
// requested without any network.
type fakeTransport struct {
	status     int
	statusText string
	body       []byte
	urls       []string
}

func (t *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.urls = append(t.urls, req.URL.String())
	return &http.Response{
		StatusCode: t.status,
		Status:     t.statusText,
		Body:       nopCloser{bytes.NewReader(t.body)},
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

type nopCloser struct{ r *bytes.Reader }

func (c nopCloser) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c nopCloser) Close() error               { return nil }

func clientWith(tr http.RoundTripper) *http.Client {
	return &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return requireHTTPS(req.URL.String())
	}}
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestFetchVerifiedReturnsPinnedBytes(t *testing.T) {
	body := []byte("a chart archive")
	tr := &fakeTransport{status: http.StatusOK, body: body}
	got, err := fetchVerified(context.Background(), clientWith(tr), "https://charts.example/chart.tgz", digestOf(body))
	if err != nil {
		t.Fatalf("fetchVerified = %v, want the pinned bytes", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("fetchVerified returned %q, want %q", got, body)
	}
}

func TestFetchVerifiedRefusesBytesThatDoNotHashToThePin(t *testing.T) {
	tr := &fakeTransport{status: http.StatusOK, body: []byte("attacker bytes")}
	_, err := fetchVerified(context.Background(), clientWith(tr), "https://charts.example/chart.tgz", digestOf([]byte("pinned bytes")))
	if err == nil || !strings.Contains(err.Error(), "does not match its pin") {
		t.Fatalf("fetchVerified with wrong bytes = %v, want a digest mismatch naming both hashes", err)
	}
	if !strings.Contains(err.Error(), digestOf([]byte("pinned bytes"))) {
		t.Errorf("the mismatch error does not name the wanted digest: %v", err)
	}
}

func TestFetchVerifiedRefusesNonHTTPSBeforeRequesting(t *testing.T) {
	tr := &fakeTransport{status: http.StatusOK, body: []byte("x")}
	_, err := fetchVerified(context.Background(), clientWith(tr), "http://charts.example/chart.tgz", digestOf([]byte("x")))
	if err == nil || !strings.Contains(err.Error(), "non-https") {
		t.Fatalf("fetchVerified over http = %v, want a pre-request refusal", err)
	}
	if len(tr.urls) != 0 {
		t.Errorf("the http URL was requested anyway: %v", tr.urls)
	}
}

func TestFetchVerifiedSurfacesHTTPStatus(t *testing.T) {
	tr := &fakeTransport{status: http.StatusNotFound, statusText: "404 Not Found", body: nil}
	_, err := fetchVerified(context.Background(), clientWith(tr), "https://charts.example/chart.tgz", digestOf(nil))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("fetchVerified on a 404 = %v, want the status in the error", err)
	}
}

// chartArchive builds a small gzipped tar with the given entries, the
// shape a Helm chart ships in.
func chartArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractChartExtractsTheChartTree(t *testing.T) {
	dir := t.TempDir()
	archive := chartArchive(t, map[string]string{
		"stackrox-test/Chart.yaml":  "name: stackrox-test\n",
		"stackrox-test/values.yaml": "# values\n",
	})
	if err := extractChart(dir, archive); err != nil {
		t.Fatalf("extractChart = %v", err)
	}
	for name, content := range map[string]string{
		"stackrox-test/Chart.yaml":  "name: stackrox-test\n",
		"stackrox-test/values.yaml": "# values\n",
	} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading the extracted %s: %v", name, err)
		}
		if string(b) != content {
			t.Errorf("extracted %s = %q, want %q", name, b, content)
		}
	}
}

func TestExtractChartRefusesEntriesThatEscape(t *testing.T) {
	for name, entry := range map[string]string{
		"parent path":   "../evil",
		"nested parent": "good/../../evil",
		"absolute":      "/etc/passwd",
		"cleaned to ..": "good/..",
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			gzw := gzip.NewWriter(&out)
			tw := tar.NewWriter(gzw)
			if err := tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o644, Size: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gzw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractChart(t.TempDir(), out.Bytes()); err == nil {
				t.Fatalf("extractChart accepted the escaping entry %q", entry)
			}
		})
	}
}

func TestExtractChartRefusesNonRegularEntries(t *testing.T) {
	for _, typeflag := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		var out bytes.Buffer
		gzw := gzip.NewWriter(&out)
		tw := tar.NewWriter(gzw)
		if err := tw.WriteHeader(&tar.Header{Name: "evil", Mode: 0o777, Typeflag: typeflag, Linkname: "/etc/passwd"}); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gzw.Close(); err != nil {
			t.Fatal(err)
		}
		err := extractChart(t.TempDir(), out.Bytes())
		if err == nil || !strings.Contains(err.Error(), "unsupported type") {
			t.Fatalf("extractChart accepted entry type %q", string(rune(typeflag)))
		}
	}
}

func TestWriteFileBoundedRefusesAStreamThatContradictsTheHeader(t *testing.T) {
	dir := t.TempDir()
	// Fewer bytes than the header declares: the entry is truncated.
	if err := writeFileBounded(filepath.Join(dir, "lies"), strings.NewReader("short"), 10); err == nil ||
		!strings.Contains(err.Error(), "declared") {
		t.Fatalf("writeFileBounded on a short stream = %v, want a size mismatch", err)
	}
	// More bytes than the header declares: the entry over-runs.
	if err := writeFileBounded(filepath.Join(dir, "overruns"), strings.NewReader("0123456789X"), 10); err == nil ||
		!strings.Contains(err.Error(), "declared") {
		t.Fatalf("writeFileBounded on an over-running stream = %v, want a size mismatch", err)
	}
}

func TestFetchChartFetchesThePinnedVersionAndExtractsIt(t *testing.T) {
	archive := chartArchive(t, map[string]string{"stackrox-test/Chart.yaml": "name: stackrox-test\n"})
	tr := &fakeTransport{status: http.StatusOK, body: archive}
	pin, err := fetchChart(context.Background(), clientWith(tr), t.TempDir(), "stackrox-test", "1.2.3", digestOf(archive))
	if err != nil {
		t.Fatalf("fetchChart = %v", err)
	}
	if len(tr.urls) != 1 || !strings.HasSuffix(tr.urls[0], "stackrox-test-1.2.3.tgz") {
		t.Errorf("the chart was fetched from %v, want exactly the pinned version URL", tr.urls)
	}
	if _, err := os.Stat(filepath.Join(pin.dir, "Chart.yaml")); err != nil {
		t.Errorf("the chart's root is not helm-ready at %s: %v", pin.dir, err)
	}
}

func TestFetchChartRefusesAnArchiveThatIsNotOneChart(t *testing.T) {
	// Files at the archive root: not the one-top-level-directory layout
	// helm packages ship.
	archive := chartArchive(t, map[string]string{"Chart.yaml": "name: loose\n", "values.yaml": "# values\n"})
	tr := &fakeTransport{status: http.StatusOK, body: archive}
	_, err := fetchChart(context.Background(), clientWith(tr), t.TempDir(), "stackrox-test", "1.2.3", digestOf(archive))
	if err == nil || !strings.Contains(err.Error(), "exactly one top-level directory") {
		t.Fatalf("fetchChart on a loose archive = %v, want a layout refusal", err)
	}
}

func TestFetchChartReplacesAnEarlierExtraction(t *testing.T) {
	newArchive := chartArchive(t, map[string]string{"stackrox-test/Chart.yaml": "name: new\n"})
	dir := t.TempDir()
	old := filepath.Join(dir, "stackrox-test", "stale.txt")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := &fakeTransport{status: http.StatusOK, body: newArchive}
	if _, err := fetchChart(context.Background(), clientWith(tr), dir, "stackrox-test", "1.2.3", digestOf(newArchive)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a stale file from an earlier extraction survived: %v", err)
	}
}

func TestChartPinsAreRecorded(t *testing.T) {
	// The pins are the example's determinism knob; a placeholder that
	// never shipped would silently float the deployment.
	for name, digest := range map[string]string{
		"stackrox-central-services":         centralChartSHA256,
		"stackrox-secured-cluster-services": securedChartSHA256,
	} {
		if len(digest) != 64 {
			t.Errorf("chart %s has no recorded SHA-256 digest", name)
		}
	}
	if centralChartVersion != securedChartVersion {
		t.Errorf("the two charts are pinned to different versions (%s vs %s); StackRox releases them together",
			centralChartVersion, securedChartVersion)
	}
}
