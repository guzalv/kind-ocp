package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/guzalv/kind-ocp/internal/fscheck"
)

// The StackRox chart pins. The chart tarballs are the pinned unit: each is
// fetched from StackRox's public chart repository at a recorded version and
// verified against a reviewed SHA-256 digest before anything reads it (a
// chart repository, like a GitHub release asset, can re-publish the same
// version). The images the chart references are the released public tags of
// the appVersion (quay.io/stackrox-io/main:5.0.0, central-db:5.0.0, …):
// the chart owns them, so a chart bump is an image bump.
//
// Bump procedure: pick a released version, take its digest from the
// repository's index.yaml (https://charts.stackrox.io/index.yaml), record
// both here, and run the example once. The two charts are released together
// per StackRox version and should stay on the same appVersion.
const (
	chartRepository = "https://charts.stackrox.io"

	centralChartVersion = "500.0.0"
	centralChartSHA256  = "4a39146348a3419e7024f18841a81c50a20cbccb4aa3263fedda7936942484c2"

	securedChartVersion = "500.0.0"
	securedChartSHA256  = "db9a1f31dd48bd0f45610adadd74b3e541890df9111eaa4cbd619fac2254e788"

	// maxChartBytes bounds the archive download. The pinned charts are a
	// few hundred kilobytes; a gigabyte-sized answer is not a chart.
	maxChartBytes = 64 << 20
)

// chartPin names one pinned chart and where it was extracted.
type chartPin struct {
	name    string
	version string
	sha256  string

	// dir is the chart's root directory — the archive's single top-level
	// directory, the one carrying Chart.yaml — ready for helm.
	dir string
}

// fetchCharts fetches and verifies both pinned charts into the work
// directory and returns their extracted directories. Nothing is extracted
// unless the downloaded bytes hash to the recorded digest. The charts
// directory is verified against kind-ocp's boundary policy (the work
// directory is opened verified; this keeps the check on the entry the
// extraction actually writes through).
func fetchCharts(ctx context.Context, work string) (central, secured *chartPin, err error) {
	chartsDir := filepath.Join(work, "charts")
	if err := os.MkdirAll(chartsDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("creating the charts directory: %w", err)
	}
	if err := fscheck.CheckManagedDir(chartsDir); err != nil {
		return nil, nil, fmt.Errorf("the charts directory: %w", err)
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return requireHTTPS(req.URL.String())
	}}
	central, err = fetchChart(ctx, client, chartsDir, "stackrox-central-services", centralChartVersion, centralChartSHA256)
	if err != nil {
		return nil, nil, err
	}
	secured, err = fetchChart(ctx, client, chartsDir, "stackrox-secured-cluster-services", securedChartVersion, securedChartSHA256)
	if err != nil {
		return nil, nil, err
	}
	return central, secured, nil
}

// fetchChart downloads one pinned chart, verifies it, and extracts it under
// chartsDir/<name>. An existing extracted directory from an earlier run is
// replaced, so the extracted tree always matches the verified archive. The
// HTTP client is a parameter so tests can substitute its transport;
// production callers pass a plain client with the https-only redirect rule.
func fetchChart(ctx context.Context, client *http.Client, chartsDir, name, version, digest string) (*chartPin, error) {
	extractDir := filepath.Join(chartsDir, name)
	url := fmt.Sprintf("%s/%s-%s.tgz", chartRepository, name, version)
	archive, err := fetchVerified(ctx, client, url, digest)
	if err != nil {
		return nil, fmt.Errorf("fetching the %s chart: %w", name, err)
	}
	if err := os.RemoveAll(extractDir); err != nil {
		return nil, fmt.Errorf("clearing the previous %s chart extraction: %w", name, err)
	}
	if err := extractChart(extractDir, archive); err != nil {
		return nil, fmt.Errorf("extracting the %s chart: %w", name, err)
	}
	root, err := chartRootDir(extractDir)
	if err != nil {
		return nil, fmt.Errorf("validating the %s chart: %w", name, err)
	}
	return &chartPin{name: name, version: version, sha256: digest, dir: root}, nil
}

// chartRootDir resolves the directory helm must be pointed at. Helm chart
// archives carry the chart in exactly one top-level directory; anything
// else is not a chart package, and refusing it here beats a helm error
// later with less context.
func chartRootDir(extractDir string) (string, error) {
	entries, err := os.ReadDir(extractDir)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return "", fmt.Errorf("the extraction at %s does not hold exactly one top-level directory", extractDir)
	}
	root := filepath.Join(extractDir, entries[0].Name())
	if _, err := os.Stat(filepath.Join(root, "Chart.yaml")); err != nil {
		return "", fmt.Errorf("the chart directory %s carries no Chart.yaml", root)
	}
	return root, nil
}

// fetchVerified downloads url and returns its bytes only if they hash to
// the recorded SHA-256 digest. Only https URLs are requested, redirects
// must stay https, and the body is bounded. The client is a parameter so
// tests can substitute its transport; production callers pass a plain
// client with the https-only redirect rule.
func fetchVerified(ctx context.Context, client *http.Client, url, digest string) ([]byte, error) {
	if err := requireHTTPS(url); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChartBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	if len(body) > maxChartBytes {
		return nil, fmt.Errorf("%s exceeds the %d byte chart bound", url, maxChartBytes)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != digest {
		return nil, fmt.Errorf("%s does not match its pin: got sha256 %s, want %s", url, got, digest)
	}
	return body, nil
}

func requireHTTPS(url string) error {
	if len(url) < 8 || url[:8] != "https://" {
		return fmt.Errorf("refusing a non-https fetch: %s", url)
	}
	return nil
}

// extractChart unpacks a gzipped chart tarball into dir. Only plain files
// and directories are accepted — no symlinks, no devices, no hardlinks —
// and every entry must stay inside dir: a chart archive is upstream
// content, and this is the boundary between it and the filesystem.
func extractChart(dir string, archive []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(header.Name)
		if clean == "." || header.Name == "" {
			return fmt.Errorf("archive entry with an empty name")
		}
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes the extraction directory", header.Name)
		}
		target := filepath.Join(dir, clean)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if err := writeFileBounded(target, tr, header.Size); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q has unsupported type %q", header.Name, string(rune(header.Typeflag)))
		}
	}
}

func writeFileBounded(target string, r io.Reader, size int64) error {
	if size < 0 || size > maxChartBytes {
		return fmt.Errorf("archive entry %s has an out-of-bounds size %d", target, size)
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// io.Copy with a LimitReader of size+1: one byte past the declared
	// size means the archive lied, and the entry is refused.
	n, err := io.Copy(f, io.LimitReader(r, size+1))
	if err != nil {
		_ = f.Close()
		return err
	}
	if n != size {
		_ = f.Close()
		return fmt.Errorf("archive entry %s declared %d bytes but carried %d", target, size, n)
	}
	// Close is checked, not deferred: a short write surfaces here as a
	// close error, and a file whose contents were not flushed must not
	// count as extracted.
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}
