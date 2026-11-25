package openshiftapicrds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stackrox/kind-ocp/internal/fscheck"
	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/layers"
)

// The CRD cache. Apply needs the same 18 manifests for every cluster it
// converges and for every re-create after a delete, so they are fetched
// once per state root into a directory the framework provides
// (layers.Env.CacheDir → <state root>/cache/<layer>/<commit>) instead of
// once per cluster. The directory name is the pin — the commit the
// manifests were fetched from — so a pin bump never reuses another
// commit's files, and delete removes the whole tree with the rest of
// the prefix's state.
//
// A staged cache is just the manifest files, named by CRD:
//
//	<dir>/<crd name>.crd.yaml
//
// There is no index and no bookkeeping of any kind. The expected
// SHA-256 of every file is generated from the pinned module at build
// time (ModuleDigests in pin_gen.go), so a cache entry verifies the
// same way the download it came from does: against the pin, not against
// anything the cache records about itself. A file that is missing, does
// not carry its expected digest, or is larger than every pinned
// manifest (ModuleMaxManifestBytes — no read allocates for more of a
// caller-owned file than the pin says an entry can be) is fetched and
// atomically replaced — only that file, never the whole cache. A file
// that is a symlink, or not a private caller-owned regular file, is
// refused: that is the state boundary's rule, and a refusal is never
// self-healed into a replacement. Two concurrent Applies of the same
// pin write identical bytes and are safe without locks, at worst
// duplicating work.

// crdFileSuffix extends a CRD's name into its cache file name, the
// PoC's staging convention.
const crdFileSuffix = ".crd.yaml"

// crdCache is one commit's cache directory.
type crdCache struct {
	dir string
}

// fileName is a CRD's cache file name.
func fileName(crdName string) string { return crdName + crdFileSuffix }

// loadCRDs returns the layer's 18 CRD manifests, from the cache when
// every entry verifies, and with only the missing or corrupt entries
// fetched and replaced otherwise. cached reports whether the cache
// served them without a fetch. The transport is a parameter so the
// fetch and the replacement are unit-testable against a replaying
// transport (production passes http.DefaultTransport; the fetcher,
// kube.FetchManifest, owns the HTTP client and its https-only redirect
// policy). The layer refuses to run without a cache directory provider
// in its environment: fetching without a verified place to stage would
// refetch per cluster and apply bytes no re-verification ever covers.
func loadCRDs(ctx context.Context, env layers.Env, transport http.RoundTripper) (objs []*unstructured.Unstructured, cached bool, err error) {
	if env.CacheDir == nil {
		return nil, false, errors.New("the layer environment provides no cache directory (kindocp supplies one); refusing to fetch the openshift/api CRDs without a verified place to cache them")
	}
	dir, err := env.CacheDir(ModuleCommit)
	if err != nil {
		return nil, false, fmt.Errorf("preparing the CRD cache directory: %w", err)
	}
	cache := crdCache{dir: dir}

	objs, missing, err := cache.load()
	if err != nil {
		return nil, false, err
	}
	if len(missing) == 0 {
		return objs, true, nil
	}
	env.Log.Warn("refetching openshift/api CRD cache entries that are missing or do not verify",
		"dir", dir, "entries", len(missing))
	// fetch validates every download against the pin and returns the
	// decoded objects for exactly the missing entries, in order. Those
	// objects are what Apply applies: cached and fetched bytes carry
	// the same pinned digest, so re-reading the files the fetch just
	// wrote would decode the same bytes again for nothing. The set is
	// complete when fetch succeeds — every gap is an index fetch
	// filled.
	fetched, err := cache.fetch(ctx, transport, missing)
	if err != nil {
		return nil, false, err
	}
	for i, idx := range missing {
		objs[idx] = fetched[i]
	}
	return objs, false, nil
}

// load reads and classifies every cache file, returning one slot per
// CRD source in table order — the decoded object where the entry
// verifies, nil where it is missing or does not — plus the indices of
// the nil slots. Corruption heals by replacement. A file that cannot
// be read safely — a symlink, a shared or foreign file, an unreadable
// directory entry — is an error, not a gap: the state boundary's
// refusals are never self-healed.
func (c crdCache) load() (objs []*unstructured.Unstructured, missing []int, err error) {
	objs = make([]*unstructured.Unstructured, len(CRDSources))
	for i, src := range CRDSources {
		want, ok := ModuleDigests[src.Name]
		if !ok {
			// The digest table is generated from this very table; a
			// missing entry means the pin file is stale, not that the
			// cache is wrong.
			return nil, nil, fmt.Errorf("the pin file records no expected digest for %s — regenerate it (go generate ./internal/layers/openshiftapicrds)", src.Name)
		}
		b, err := readCacheFile(c.file(src.Name))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errOversizedEntry) {
				missing = append(missing, i)
				continue
			}
			// A refusal or a read failure the boundary owns: never a
			// replacement trigger.
			return nil, nil, err
		}
		if got := sha256Hex(b); got != want {
			missing = append(missing, i)
			continue
		}
		obj, err := decodeCRD(b, src.Name)
		if err != nil {
			// The bytes carry the digest the pin generated from the
			// module, so a decode failure here means the pin file
			// disagrees with the module it was generated from —
			// fetching would return the same bytes.
			return nil, nil, fmt.Errorf("cached %s carries its expected digest but is not the CRD it claims to be: %w — regenerate the pin file (go generate ./internal/layers/openshiftapicrds)", c.file(src.Name), err)
		}
		objs[i] = obj
	}
	return objs, missing, nil
}

// fetch downloads the sources at the given CRDSources indices from the
// pinned commit, validates every download against the build-time
// expected digest and the CRD identity, atomically replaces each cache
// entry, and returns the decoded objects in index order. A download
// that does not match its expected digest is refused — the pin, not
// the response, decides which bytes may be cached and applied — and
// nothing is written for it. The transport is a parameter so tests can
// replay canned responses network-free; production passes
// http.DefaultTransport and the fetcher (kube.FetchManifest) owns the
// client, its https-only redirect policy, and the download bound.
func (c crdCache) fetch(ctx context.Context, transport http.RoundTripper, indices []int) ([]*unstructured.Unstructured, error) {
	objs := make([]*unstructured.Unstructured, 0, len(indices))
	for _, idx := range indices {
		src := CRDSources[idx]
		url, err := crdURL(ModuleCommit, src.Path)
		if err != nil {
			return nil, err
		}
		b, err := kube.FetchManifest(ctx, transport, fetchTimeout, url)
		if err != nil {
			return nil, err
		}
		obj, err := validateManifest(b, src)
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w (the pinned commit's content changed? bump the pin and revisit the layer's CRDSources table)", url, err)
		}
		if err := c.writeFile(fileName(src.Name), b); err != nil {
			return nil, err
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

// validateManifest checks bytes against the build-time expectation for
// src and returns them decoded as the CRD they claim to be: the digest
// ModuleDigests records, and the CRD identity. Bytes that pass neither
// the digest nor the identity check never reach the cache, a cluster,
// or the objects loadCRDs returns.
func validateManifest(b []byte, src CRDSource) (*unstructured.Unstructured, error) {
	want, ok := ModuleDigests[src.Name]
	if !ok {
		return nil, fmt.Errorf("the pin file records no expected digest for %s — regenerate it (go generate ./internal/layers/openshiftapicrds)", src.Name)
	}
	if got := sha256Hex(b); got != want {
		return nil, fmt.Errorf("bytes do not match the pinned digest: got sha256:%s, want sha256:%s", got, want)
	}
	// Belt and braces: the digest is generated from the module, whose
	// files are pinned to decode as the CRD they claim to be, so this
	// cannot fail for matching bytes — but refuse clearly if it does.
	return decodeCRD(b, src.Name)
}

func (c crdCache) file(crdName string) string { return filepath.Join(c.dir, fileName(crdName)) }

// writeFile atomically installs one cache file: the bytes go to a
// private temporary file in the cache directory that is renamed over
// the destination. An existing destination must verify as a private,
// caller-owned regular file first (fscheck.CheckWriteTarget) — a
// symlinked or foreign cache file is refused, never written through —
// and the rename is what makes replacing a corrupt entry converge.
func (c crdCache) writeFile(name string, b []byte) error {
	dst := filepath.Join(c.dir, name)
	if err := fscheck.CheckWriteTarget(dst); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("staging %s: %w", dst, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("staging %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("staging %s: %w", dst, err)
	}
	// CreateTemp already made the temporary file private (0600); the
	// rename keeps its mode.
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("staging %s: %w", dst, err)
	}
	return nil
}

// errOversizedEntry marks a cache file larger than every pinned
// manifest: it cannot be one, and the bounded read stopped at the pin's
// limit instead of its remaining bytes. load classifies it like a
// digest mismatch — fetched and replaced — never a refusal: the file
// itself verified as a private, caller-owned regular file, so replacing
// its directory entry is the same atomic rename a corrupt entry gets.
var errOversizedEntry = errors.New("cache entry is larger than every pinned manifest")

// readCacheFile reads one cache file after verifying it is a private,
// caller-owned regular file: a cache read through a symlinked or shared
// file is refused, not trusted. The read itself is bounded by
// ModuleMaxManifestBytes, the size of the largest manifest the pin
// describes: the file's length is caller-owned data, so no cache read
// allocates for more of it than the pin says an entry can be. A file
// past the bound is reported as errOversizedEntry — corruption to heal
// by replacement, not a whole file read before its digest could reject
// it.
func readCacheFile(path string) ([]byte, error) {
	if err := fscheck.CheckPrivateFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// One byte past the bound distinguishes "at the bound" from "over
	// it" without a stat call — a size read before the open is a bound
	// the file can change after, and the read is the honest one.
	b, err := io.ReadAll(io.LimitReader(f, ModuleMaxManifestBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > ModuleMaxManifestBytes {
		return nil, errOversizedEntry
	}
	return b, nil
}

// sha256Hex returns the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
