// Command pin-openshift-api regenerates the openshift/api pin the
// openshift-api-crds layer fetches its CRDs from
// (internal/layers/openshiftapicrds/pin_gen.go):
//
//   - the version is whatever this repo's go.mod requires, read with
//     `go list -m` — go.mod stays the single source of truth;
//   - the commit is the full 40-hex SHA that version resolves to on
//     GitHub, looked up through the GitHub REST API. A pseudo-version
//     carries only a 12-hex commit prefix; the full SHA is recorded so
//     runtime fetches address an immutable, unambiguous locator instead
//     of handing the choice of commit to GitHub's resolver;
//   - the expected digest of every one of the layer's 18 CRD manifests
//     is the SHA-256 of that file's bytes in the pinned module, read
//     from the local module cache. The runtime validates every cache
//     read and every download against these digests, so the bytes that
//     reach a cluster are bound to the module go.mod pins — not to
//     anything the cache or the network says about itself;
//   - the size of the largest pinned manifest bounds every cache read:
//     a cache file larger than every entry the pin describes cannot be
//     one, so the runtime stops reading there instead of allocating
//     for a caller-owned file of any planted size.
//
// The table of CRDs the digests cover is the layer's own CRDSources
// (imported, not duplicated, so the two can never drift). The commit
// lookup needs network access, and the digests need the module in the
// local cache (it is, if the build ran); builds and tests do not need
// either — the generated file is committed. Run it via go generate
// (see pin.go) or `make generate`, and commit the result together with
// the go.mod bump.
//
// The generated file must exist for this tool to build — the layer
// package references the names it declares — and it always does in a
// checkout: the file is committed, and regenerating rewrites it in
// place rather than deleting it first.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/stackrox/kind-ocp/internal/layers/openshiftapicrds"
)

// modulePath is the pinned module this tool resolves.
const modulePath = "github.com/openshift/api"

// pinPackageDir is the package the generated file lives in, relative to
// the module root.
const pinPackageDir = "internal/layers/openshiftapicrds"

// pinFile is the generated file's name.
const pinFile = "pin_gen.go"

// pseudoVersionPattern matches a Go pseudo-version
// (vX.Y.Z-yyyymmddhhmmss-abcdef123456); its last segment is a 12-hex
// commit prefix.
var pseudoVersionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?-\d{14}-([0-9a-f]{12})$`)

// commitPattern matches a full 40-hex-character commit SHA.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// apiTimeout bounds the one GitHub API request the commit lookup makes.
const apiTimeout = 30 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pin-openshift-api: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	version, err := requiredVersion(root)
	if err != nil {
		return err
	}
	commit, err := resolveCommit(version)
	if err != nil {
		return err
	}
	digests, limit, err := moduleDigests(root)
	if err != nil {
		return err
	}
	out := filepath.Join(root, pinPackageDir, pinFile)
	changed, err := writePin(out, version, commit, digests, limit)
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("pin-openshift-api: wrote %s: %s at commit %s with %d expected digests (largest manifest %d bytes)\n",
			out, version, commit, len(digests), limit)
	} else {
		fmt.Printf("pin-openshift-api: %s already pins %s at commit %s\n", out, version, commit)
	}
	return nil
}

// moduleRoot returns this module's root directory, wherever inside the
// module the tool runs from, via `go env GOMOD`.
func moduleRoot() (string, error) {
	gomod, err := goOutput("", "env", "GOMOD")
	if err != nil {
		return "", err
	}
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not running inside a Go module (GOMOD=%q)", gomod)
	}
	return filepath.Dir(gomod), nil
}

// requiredVersion returns the version go.mod requires of the pinned
// module, refusing anything with a replace directive or a missing
// requirement — both mean the pin is not what this tool exists to
// record.
func requiredVersion(root string) (string, error) {
	replace, err := goOutput(root, "list", "-m", "-f", "{{with .Replace}}{{.Path}}{{end}}", modulePath)
	if err != nil {
		return "", fmt.Errorf("reading %s from go.mod (is it required?): %w", modulePath, err)
	}
	if replace != "" {
		return "", fmt.Errorf("%s is replaced in go.mod (%s); a replaced module has no upstream commit to pin — drop the replace to pin upstream content", modulePath, replace)
	}
	version, err := goOutput(root, "list", "-m", "-f", "{{.Version}}", modulePath)
	if err != nil {
		return "", fmt.Errorf("reading %s from go.mod: %w", modulePath, err)
	}
	if version == "" {
		return "", fmt.Errorf("go.mod does not record a version for %s", modulePath)
	}
	return version, nil
}

// resolveCommit returns the full commit SHA the given module version
// resolves to. A pseudo-version's 12-hex suffix and a plain tag alike
// are resolved through the GitHub commits API, which accepts any
// unambiguous ref; the result must be a full SHA and, for a
// pseudo-version, must extend that version's own commit prefix.
func resolveCommit(version string) (string, error) {
	ref := version
	if m := pseudoVersionPattern.FindStringSubmatch(version); m != nil {
		ref = m[1]
	}
	url := fmt.Sprintf("https://api.github.com/repos/openshift/api/commits/%s", ref)
	client := &http.Client{Timeout: apiTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("resolving %s to a commit via %s: %w", version, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("resolving %s to a commit: reading response: %w", version, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolving %s to a commit via %s: unexpected status %s (body: %s)",
			version, url, resp.Status, excerpt(body))
	}
	var parsed struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("resolving %s to a commit: parsing response: %w", version, err)
	}
	if !commitPattern.MatchString(parsed.SHA) {
		return "", fmt.Errorf("resolving %s to a commit: %s returned %q, not a full SHA", version, url, parsed.SHA)
	}
	if prefix := pseudoVersionPrefix(version); prefix != "" && !strings.HasPrefix(parsed.SHA, prefix) {
		return "", fmt.Errorf("resolving %s to a commit: %s returned %s, which does not extend the version's own commit prefix %s",
			version, url, parsed.SHA, prefix)
	}
	return parsed.SHA, nil
}

// moduleDigests hashes every manifest the layer's CRDSources table
// names, as it exists in the pinned module in the local module cache,
// and returns the CRD-name → SHA-256 map the runtime validates cache
// reads and downloads against, plus the byte size of the largest of
// those manifests — the bound cache reads stop at.
func moduleDigests(root string) (digests map[string]string, maxManifest int, err error) {
	dir, err := goOutput(root, "list", "-m", "-f", "{{.Dir}}", modulePath)
	if err != nil {
		return nil, 0, fmt.Errorf("resolving %s's module directory (is the module cache warm? it is, if the build ran): %w", modulePath, err)
	}
	if dir == "" {
		return nil, 0, fmt.Errorf("go list -m recorded no directory for %s; the module is not in the local cache", modulePath)
	}
	digests = make(map[string]string, len(openshiftapicrds.CRDSources))
	for _, src := range openshiftapicrds.CRDSources {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(src.Path)))
		if err != nil {
			return nil, 0, fmt.Errorf("reading %s in the pinned module: %w (module layout changed? update the layer's CRDSources table)", src.Path, err)
		}
		if len(b) > maxManifest {
			maxManifest = len(b)
		}
		sum := sha256.Sum256(b)
		digests[src.Name] = hex.EncodeToString(sum[:])
	}
	return digests, maxManifest, nil
}

// pseudoVersionPrefix returns a pseudo-version's 12-hex commit prefix,
// or "" for a version that is not a pseudo-version.
func pseudoVersionPrefix(version string) string {
	if m := pseudoVersionPattern.FindStringSubmatch(version); m != nil {
		return m[1]
	}
	return ""
}

// writePin renders the generated pin file, gofmt-formatted, and writes
// it only when its content changes — regenerating an in-sync pin is a
// no-op for the working tree. changed reports whether the file was
// written.
func writePin(path, version, commit string, digests map[string]string, maxManifest int) (changed bool, err error) {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "// Code generated by tools/pin-openshift-api; DO NOT EDIT.\n\n")
	fmt.Fprintf(&buf, "package %s\n\n", filepath.Base(pinPackageDir))
	fmt.Fprintf(&buf, "// ModuleVersion is the pinned %s version go.mod requires —\n", modulePath)
	fmt.Fprintf(&buf, "// the single source of truth for which CRDs the %s layer fetches.\n", filepath.Base(pinPackageDir))
	fmt.Fprintf(&buf, "// Bump it in go.mod, then regenerate this file (go generate ./%s).\n", pinPackageDir)
	fmt.Fprintf(&buf, "const ModuleVersion = %q\n\n", version)
	fmt.Fprintf(&buf, "// ModuleCommit is the full commit SHA ModuleVersion resolves to; every\n")
	fmt.Fprintf(&buf, "// CRD manifest is fetched from exactly this commit.\n")
	fmt.Fprintf(&buf, "const ModuleCommit = %q\n\n", commit)
	fmt.Fprintf(&buf, "// ModuleDigests maps each CRD's metadata.name to the SHA-256 of its\n")
	fmt.Fprintf(&buf, "// manifest bytes in the pinned module. Every cache read and every\n")
	fmt.Fprintf(&buf, "// download is validated against these digests, binding the bytes that\n")
	fmt.Fprintf(&buf, "// reach a cluster to the module go.mod pins.\n")
	fmt.Fprintf(&buf, "var ModuleDigests = map[string]string{\n")
	names := make([]string, 0, len(digests))
	for name := range digests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&buf, "\t%q: %q,\n", name, digests[name])
	}
	fmt.Fprintf(&buf, "}\n\n")
	fmt.Fprintf(&buf, "// ModuleMaxManifestBytes is the byte size of the largest CRD manifest\n")
	fmt.Fprintf(&buf, "// in the pinned module. Cache reads stop there: an entry larger than\n")
	fmt.Fprintf(&buf, "// every pinned manifest cannot be one, so it is treated as corrupt\n")
	fmt.Fprintf(&buf, "// and replaced, never read whole into memory.\n")
	fmt.Fprintf(&buf, "const ModuleMaxManifestBytes = %d\n", maxManifest)
	src, err := format.Source(buf.Bytes())
	if err != nil {
		return false, fmt.Errorf("formatting %s: %w\n\nsource:\n%s", path, err, buf.String())
	}
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, src) {
		return false, nil
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, nil
}

// goOutput runs the go command with dir as the working directory (or
// the current one when empty) and returns its trimmed stdout.
func goOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, excerpt(stderr.Bytes()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// excerpt renders a short, single-line excerpt of b for error messages.
func excerpt(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
