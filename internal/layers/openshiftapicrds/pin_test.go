package openshiftapicrds

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

// modulePath is the pinned module these tests resolve, mirroring the
// generator's constant.
const modulePath = "github.com/openshift/api"

// TestModulePin pins the generated pin file against the module graph the
// test binary was built from: the version go.mod requires must be the
// one embedded, and the embedded commit must be a full SHA that extends
// the version's own pseudo-version commit prefix. A go.mod bump without
// a regeneration fails here, at build time, instead of fetching one
// commit's CRDs under another commit's name at create time.
func TestModulePin(t *testing.T) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("the test binary carries no build info")
	}
	var dep *debug.Module
	for _, d := range bi.Deps {
		if d.Path == modulePath {
			dep = d
		}
	}
	if dep == nil {
		t.Fatalf("%s is not in the test binary's module graph; the anchor import that keeps the go.mod requirement (see pin.go) is missing", modulePath)
	}
	if dep.Replace != nil {
		t.Fatalf("%s is replaced with %s; the pin must address upstream content, and pin_gen.go has no way to stay honest about a replaced module", modulePath, dep.Replace.Path)
	}
	if dep.Version != ModuleVersion {
		t.Errorf("the build resolved %s %s, but pin_gen.go pins %s — bump the requirement and regenerate (go generate ./%s)", modulePath, dep.Version, ModuleVersion, "internal/layers/openshiftapicrds")
	}
	if !commitPattern.MatchString(ModuleCommit) {
		t.Errorf("ModuleCommit = %q, want a full 40-hex commit SHA", ModuleCommit)
	}
	if prefix := pseudoCommitPrefix(dep.Version); prefix != "" && !strings.HasPrefix(ModuleCommit, prefix) {
		t.Errorf("ModuleCommit = %q does not extend commit prefix %q of the resolved version %s", ModuleCommit, prefix, dep.Version)
	}
}

// pseudoCommitPrefix returns a version's 12-hex pseudo-version commit
// prefix, or "" when the version is not a pseudo-version.
func pseudoCommitPrefix(version string) string {
	parts := strings.Split(version, "-")
	if len(parts) < 3 {
		return ""
	}
	prefix := parts[len(parts)-1]
	if len(prefix) != 12 || !isHex(prefix) {
		return ""
	}
	return prefix
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// TestCRDSourcesAgainstModule pins the CRD path map against the pinned
// openshift/api module in the local module cache: every entry's path
// must exist there, every file must be exactly the CRD its entry
// claims — one object, a CustomResourceDefinition, with the expected
// metadata.name — and every file's SHA-256 must be the one
// ModuleDigests records (a go.mod bump without a regeneration fails
// here, at build time, not as a create-run digest refusal). Every
// manifest must also pass realCRDSchema, Verify's realness check — the
// check's structural assumption (root properties, no root
// preserve-unknown) holds for every real manifest and fails for every
// config-openshift placeholder, and a module bump that broke it would
// otherwise turn every real cluster's verify FAIL. And no manifest may
// exceed ModuleMaxManifestBytes, the bound cache reads stop at. The
// module and the repository at the pinned commit carry the same bytes,
// so this validates the runtime fetch's paths and expectations without
// the network. A module bump that reshapes the
// zz_generated.crd-manifests layout fails here, at build time, instead
// of as a create-run fetch error.
func TestCRDSourcesAgainstModule(t *testing.T) {
	if len(CRDSources) != 18 {
		t.Errorf("CRDSources has %d entries, want the 18 platform CRDs", len(CRDSources))
	}
	if len(ModuleDigests) != len(CRDSources) {
		t.Errorf("ModuleDigests has %d entries, want one per CRD source (%d) — regenerate the pin file", len(ModuleDigests), len(CRDSources))
	}
	dir := moduleDir(t)
	seenName := map[string]bool{}
	seenPath := map[string]bool{}
	maxSeen := 0
	for _, src := range CRDSources {
		if seenName[src.Name] {
			t.Errorf("duplicate CRD name %q in CRDSources", src.Name)
		}
		if seenPath[src.Path] {
			t.Errorf("duplicate manifest path %q in CRDSources", src.Path)
		}
		seenName[src.Name] = true
		seenPath[src.Path] = true

		// The entry must also build the exact URL the runtime fetch
		// uses: an unbuildable path is a per-entry error, not a
		// create-run surprise.
		if _, err := crdURL(ModuleCommit, src.Path); err != nil {
			t.Errorf("CRD %s: %v", src.Name, err)
		}

		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(src.Path)))
		if err != nil {
			t.Errorf("CRD %s: reading %s in the pinned module: %v (module layout changed? update CRDSources)", src.Name, src.Path, err)
			continue
		}
		if len(b) > maxSeen {
			maxSeen = len(b)
		}
		obj, err := decodeCRD(b, src.Name)
		if err != nil {
			t.Errorf("CRD %s: %s in the pinned module is not the manifest it claims to be: %v (module layout changed? update CRDSources)", src.Name, src.Path, err)
			continue
		}
		if got := sha256Hex(b); got != ModuleDigests[src.Name] {
			t.Errorf("CRD %s: ModuleDigests records sha256:%s, but the pinned module's %s hashes to sha256:%s — regenerate the pin file (go generate ./internal/layers/openshiftapicrds)",
				src.Name, ModuleDigests[src.Name], src.Path, got)
		}
		// The traits Verify's realness check reads, pinned by running
		// the check itself over the module's manifests: a real CRD
		// that failed its own layer's check would FAIL every honest
		// verify.
		if real, why := realCRDSchema(obj); !real {
			t.Errorf("CRD %s: the pinned module's manifest fails Verify's realness check (%s) — the check's traits no longer fit the module; revisit them with the check", src.Name, why)
		}
	}
	// The read bound cache reads stop at: it must cover every pinned
	// manifest (a manifest over it would look corrupt on every cache
	// read) and it is regenerated with the digests, so it must be
	// exactly the largest one.
	if ModuleMaxManifestBytes != maxSeen {
		t.Errorf("ModuleMaxManifestBytes = %d, but the pinned module's largest manifest is %d bytes — regenerate the pin file (go generate ./internal/layers/openshiftapicrds)",
			ModuleMaxManifestBytes, maxSeen)
	}
	// No digest for a CRD the table does not list: a stale map entry
	// would look like coverage it does not have.
	for name := range ModuleDigests {
		if !seenName[name] {
			t.Errorf("ModuleDigests has an entry for %q, which CRDSources does not list — regenerate the pin file", name)
		}
	}
}

// moduleDir resolves the pinned module's directory in the local module
// cache with `go list -m`, the same resolution the generator and the
// PoC's staging step use. It runs from this package's directory, inside
// the module, and needs no network: the module is in the build graph
// (the anchor import in pin.go), so it is already downloaded.
func moduleDir(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", modulePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("resolving %s's module directory: %v: %s (is the module cache warm? it is, if the build ran)", modulePath, err, strings.TrimSpace(string(out)))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatalf("go list -m recorded no directory for %s; the module is not in the local cache", modulePath)
	}
	return dir
}
