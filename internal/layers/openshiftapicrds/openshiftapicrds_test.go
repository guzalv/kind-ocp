package openshiftapicrds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guzalv/kind-ocp/layers"
)

func TestCRDURL(t *testing.T) {
	t.Run("pinned commit and module path build the fetch URL", func(t *testing.T) {
		got, err := crdURL(ModuleCommit, "config/v1/zz_generated.crd-manifests/0000_10_config-operator_01_apiservers-Default.crd.yaml")
		if err != nil {
			t.Fatal(err)
		}
		want := rawBaseURL + "/" + ModuleCommit + "/config/v1/zz_generated.crd-manifests/0000_10_config-operator_01_apiservers-Default.crd.yaml"
		if got != want {
			t.Errorf("URL = %q, want %q", got, want)
		}
	})

	for _, commit := range []string{"", "54a3998d81eb", "54a3998d81eb911e1abb2ffab8b57bfd0571a7350", "main", "HEAD"} {
		t.Run("rejects commit "+commit, func(t *testing.T) {
			_, err := crdURL(commit, "config/v1/zz_generated.crd-manifests/x.crd.yaml")
			if err == nil || !strings.Contains(err.Error(), "invalid openshift/api commit") {
				t.Fatalf("crdURL(%q) = %v, want a commit rejection", commit, err)
			}
		})
	}

	for _, path := range []string{
		"",
		"/config/v1/x.crd.yaml",
		"../escape.crd.yaml",
		"config/../../escape.crd.yaml",
		"config/v1/x.crd.yaml?oauth=1",
		"config/v1/x.crd.yaml#fragment",
		"config//v1/x.crd.yaml",
		"config/v1/../x.crd.yaml",
	} {
		t.Run("rejects path "+path, func(t *testing.T) {
			_, err := crdURL(ModuleCommit, path)
			if err == nil || !strings.Contains(err.Error(), "invalid openshift/api manifest path") {
				t.Fatalf("crdURL(path %q) = %v, want a path rejection", path, err)
			}
		})
	}
}

func TestDecodeCRD(t *testing.T) {
	const name = "infrastructures.config.openshift.io"
	body := crdManifestBody(name)

	t.Run("the manifest it claims to be", func(t *testing.T) {
		obj, err := decodeCRD(body, name)
		if err != nil {
			t.Fatal(err)
		}
		if obj.GetName() != name || obj.GetKind() != "CustomResourceDefinition" {
			t.Errorf("decoded %s %s, want CRD %s", obj.GetKind(), obj.GetName(), name)
		}
	})

	t.Run("wrong CRD name", func(t *testing.T) {
		_, err := decodeCRD(body, "other.config.openshift.io")
		if err == nil || !strings.Contains(err.Error(), "other.config.openshift.io") {
			t.Fatalf("decodeCRD(wrong name) = %v, want an error naming the expected CRD", err)
		}
	})

	t.Run("wrong kind", func(t *testing.T) {
		_, err := decodeCRD([]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n"), name)
		if err == nil || !strings.Contains(err.Error(), "CustomResourceDefinition") {
			t.Fatalf("decodeCRD(wrong kind) = %v, want a kind rejection", err)
		}
	})

	t.Run("two documents", func(t *testing.T) {
		_, err := decodeCRD([]byte(string(body)+"---\n"+string(crdManifestBody("x"))), name)
		if err == nil || !strings.Contains(err.Error(), "exactly one object") {
			t.Fatalf("decodeCRD(two documents) = %v, want a single-object rejection", err)
		}
	})

	t.Run("no objects", func(t *testing.T) {
		if _, err := decodeCRD([]byte("# only a comment\n"), name); err == nil {
			t.Fatal("decodeCRD(no objects) = nil, want an error")
		}
	})
}

// urlTransport serves canned bodies by full URL and records every
// request, so the cache tests replay fetches network-free and can
// assert exactly which URLs were fetched.
type urlTransport struct {
	bodies   map[string][]byte
	requests []string
}

func (t *urlTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, req.URL.String())
	body, ok := t.bodies[req.URL.String()]
	if !ok {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Status:     "404 Not Found",
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader("no such body")),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/yaml"}},
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Request:    req,
	}, nil
}

// moduleManifest returns the pinned module's bytes for one source,
// read from the local module cache — the exact bytes ModuleDigests were
// generated from, and the bytes raw.githubusercontent.com serves at the
// pinned commit.
func moduleManifest(t *testing.T, src CRDSource) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleDir(t), filepath.FromSlash(src.Path)))
	if err != nil {
		t.Fatalf("reading %s from the pinned module: %v", src.Path, err)
	}
	return b
}

// moduleTransport serves every CRD source with the pinned module's own
// bytes: the network-free stand-in for the pinned fetch.
func moduleTransport(t *testing.T) *urlTransport {
	t.Helper()
	bodies := map[string][]byte{}
	for _, src := range CRDSources {
		url, err := crdURL(ModuleCommit, src.Path)
		if err != nil {
			panic(err) // the table's paths are pinned by test; a bad one is a bug
		}
		bodies[url] = moduleManifest(t, src)
	}
	return &urlTransport{bodies: bodies}
}

// crdManifestBody renders a minimal single-CRD manifest named name —
// everything decodeCRD checks for, nothing it does not.
func crdManifestBody(name string) []byte {
	return []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + name + "\n")
}

// cacheEnv builds an Env whose CacheDir serves directories under a
// temporary root, mirroring what kindocp wires up in production.
func cacheEnv(t *testing.T) layers.Env {
	t.Helper()
	root := t.TempDir()
	return layers.Env{
		Log: slog.New(slog.DiscardHandler),
		CacheDir: func(pin string) (string, error) {
			dir := filepath.Join(root, pin)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
			return dir, nil
		},
	}
}

// cacheDir returns the cache directory cacheEnv serves for the pin.
func cacheDir(t *testing.T, env layers.Env) string {
	t.Helper()
	dir, err := env.CacheDir(ModuleCommit)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadCRDs(t *testing.T) {
	ctx := context.Background()

	t.Run("refuses to run without a cache directory", func(t *testing.T) {
		_, _, err := loadCRDs(ctx, layers.Env{Log: slog.New(slog.DiscardHandler)}, nil)
		if err == nil || !strings.Contains(err.Error(), "no cache directory") {
			t.Fatalf("loadCRDs(no CacheDir) = %v, want a refusal", err)
		}
	})

	t.Run("cold cache fetches, installs, and serves all 18", func(t *testing.T) {
		env := cacheEnv(t)
		tr := moduleTransport(t)
		objs, cached, err := loadCRDs(ctx, env, tr)
		if err != nil {
			t.Fatal(err)
		}
		if cached {
			t.Error("cached = true on a cold cache")
		}
		if len(objs) != len(CRDSources) {
			t.Fatalf("loaded %d objects, want %d", len(objs), len(CRDSources))
		}
		if len(tr.requests) != len(CRDSources) {
			t.Errorf("%d fetches, want one per CRD (%d)", len(tr.requests), len(CRDSources))
		}
	})

	t.Run("warm cache serves without fetching", func(t *testing.T) {
		env := cacheEnv(t)
		if _, _, err := loadCRDs(ctx, env, moduleTransport(t)); err != nil {
			t.Fatal(err)
		}
		// An empty-bodied transport fails every fetch: a warm cache must
		// not touch the network at all.
		tr := &urlTransport{bodies: map[string][]byte{}}
		objs, cached, err := loadCRDs(ctx, env, tr)
		if err != nil {
			t.Fatal(err)
		}
		if !cached {
			t.Error("cached = false on a warm cache")
		}
		if len(objs) != len(CRDSources) {
			t.Fatalf("loaded %d objects, want %d", len(objs), len(CRDSources))
		}
		if len(tr.requests) != 0 {
			t.Errorf("%d fetches on a warm cache, want 0", len(tr.requests))
		}
	})

	t.Run("a corrupted entry is replaced from the pin, and only that one", func(t *testing.T) {
		env := cacheEnv(t)
		if _, _, err := loadCRDs(ctx, env, moduleTransport(t)); err != nil {
			t.Fatal(err)
		}
		// Corrupt one cached byte — same shape, different bytes — the
		// tester's adversarial probe.
		victim := CRDSources[0]
		path := filepath.Join(cacheDir(t, env), fileName(victim.Name))
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		b[0] = '#' // a comment marker keeps it YAML-shaped but changes the bytes
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}

		tr := moduleTransport(t)
		objs, cached, err := loadCRDs(ctx, env, tr)
		if err != nil {
			t.Fatal(err)
		}
		if cached {
			t.Error("cached = true after a replacement")
		}
		if len(objs) != len(CRDSources) {
			t.Fatalf("loaded %d objects, want %d", len(objs), len(CRDSources))
		}
		// Exactly the corrupt entry was refetched — never the whole
		// cache.
		wantURL, err := crdURL(ModuleCommit, victim.Path)
		if err != nil {
			t.Fatal(err)
		}
		if len(tr.requests) != 1 || tr.requests[0] != wantURL {
			t.Errorf("refetched %v, want exactly [%s]", tr.requests, wantURL)
		}
		// The replacement carries the pin's bytes: a second load now
		// verifies without fetching.
		cold := &urlTransport{bodies: map[string][]byte{}}
		if _, cached, err := loadCRDs(ctx, env, cold); err != nil || !cached {
			t.Fatalf("load after replacement = (cached %v, %v), want a verifying warm cache", cached, err)
		}
	})

	t.Run("an oversized entry is replaced once, like a corrupt one", func(t *testing.T) {
		env := cacheEnv(t)
		if _, _, err := loadCRDs(ctx, env, moduleTransport(t)); err != nil {
			t.Fatal(err)
		}
		// Replace one cached file with a private, caller-owned regular
		// file well past the pin's bound: no legitimate entry is larger
		// than the largest pinned manifest, so this cannot be one, and
		// the bounded read must not allocate for the whole planted
		// file before rejecting it.
		victim := CRDSources[0]
		path := filepath.Join(cacheDir(t, env), fileName(victim.Name))
		junk := bytes.Repeat([]byte("# planted\n"), ModuleMaxManifestBytes/10+2)
		if len(junk) <= ModuleMaxManifestBytes {
			t.Fatalf("planted file is %d bytes, want more than the %d bound", len(junk), ModuleMaxManifestBytes)
		}
		if err := os.WriteFile(path, junk, 0o600); err != nil {
			t.Fatal(err)
		}

		tr := moduleTransport(t)
		objs, cached, err := loadCRDs(ctx, env, tr)
		if err != nil {
			t.Fatal(err)
		}
		if cached {
			t.Error("cached = true after a replacement")
		}
		if len(objs) != len(CRDSources) {
			t.Fatalf("loaded %d objects, want %d", len(objs), len(CRDSources))
		}
		// Exactly the oversized entry was refetched — one replacement,
		// never the whole cache.
		wantURL, err := crdURL(ModuleCommit, victim.Path)
		if err != nil {
			t.Fatal(err)
		}
		if len(tr.requests) != 1 || tr.requests[0] != wantURL {
			t.Errorf("refetched %v, want exactly [%s]", tr.requests, wantURL)
		}
		// The replacement carries the pin's bytes: a second load now
		// verifies without fetching.
		cold := &urlTransport{bodies: map[string][]byte{}}
		if _, cached, err := loadCRDs(ctx, env, cold); err != nil || !cached {
			t.Fatalf("load after replacement = (cached %v, %v), want a verifying warm cache", cached, err)
		}
	})

	t.Run("a missing entry is fetched, the rest served from cache", func(t *testing.T) {
		env := cacheEnv(t)
		if _, _, err := loadCRDs(ctx, env, moduleTransport(t)); err != nil {
			t.Fatal(err)
		}
		victim := CRDSources[5]
		if err := os.Remove(filepath.Join(cacheDir(t, env), fileName(victim.Name))); err != nil {
			t.Fatal(err)
		}

		tr := moduleTransport(t)
		if _, cached, err := loadCRDs(ctx, env, tr); err != nil {
			t.Fatal(err)
		} else if cached {
			t.Error("cached = true after fetching a missing entry")
		}
		wantURL, err := crdURL(ModuleCommit, victim.Path)
		if err != nil {
			t.Fatal(err)
		}
		if len(tr.requests) != 1 || tr.requests[0] != wantURL {
			t.Errorf("refetched %v, want exactly [%s]", tr.requests, wantURL)
		}
	})

	t.Run("a download that does not match the pin is refused and nothing is written", func(t *testing.T) {
		env := cacheEnv(t)
		victim := CRDSources[0]
		wantURL, err := crdURL(ModuleCommit, victim.Path)
		if err != nil {
			t.Fatal(err)
		}
		// Serve every source with the pinned bytes except the victim,
		// which gets a valid CRD of the right name but the wrong bytes:
		// the pin, not the response, decides what may be cached.
		tr := moduleTransport(t)
		tr.bodies[wantURL] = crdManifestBody(victim.Name)

		_, _, err = loadCRDs(ctx, env, tr)
		if err == nil || !strings.Contains(err.Error(), "do not match the pinned digest") {
			t.Fatalf("loadCRDs(wrong bytes served) = %v, want a digest refusal", err)
		}
		// Nothing was written for the refused download: the cache still
		// has no entry, and nothing half-installed looks cached.
		if _, err := os.Stat(filepath.Join(cacheDir(t, env), fileName(victim.Name))); !os.IsNotExist(err) {
			t.Errorf("a file exists for the refused download: %v", err)
		}
	})

	t.Run("a download that is another CRD is refused", func(t *testing.T) {
		env := cacheEnv(t)
		victim := CRDSources[0]
		wantURL, err := crdURL(ModuleCommit, victim.Path)
		if err != nil {
			t.Fatal(err)
		}
		// A different real manifest under the victim's URL: the right
		// digest shape for some other CRD, but not the pinned one.
		tr := moduleTransport(t)
		tr.bodies[wantURL] = moduleManifest(t, CRDSources[1])

		_, _, err = loadCRDs(ctx, env, tr)
		if err == nil || !strings.Contains(err.Error(), victim.Path) {
			t.Fatalf("loadCRDs(swapped manifest served) = %v, want an error naming the manifest", err)
		}
	})

	t.Run("fetch failure on a cold cache is the error", func(t *testing.T) {
		env := cacheEnv(t)
		tr := &urlTransport{bodies: map[string][]byte{}}
		_, _, err := loadCRDs(ctx, env, tr)
		if err == nil || !strings.Contains(err.Error(), "fetching manifest") {
			t.Fatalf("loadCRDs(cold, unreachable) = %v, want a fetch error", err)
		}
	})

	t.Run("a symlinked cache file is refused and never written through", func(t *testing.T) {
		env := cacheEnv(t)
		if _, _, err := loadCRDs(ctx, env, moduleTransport(t)); err != nil {
			t.Fatal(err)
		}
		// Replace one cached file with a symlink to a private,
		// caller-owned file outside the cache — the tester's adversarial
		// probe. The read must refuse it, and the replacement fetch must
		// not write through the link.
		victim := CRDSources[0]
		path := filepath.Join(cacheDir(t, env), fileName(victim.Name))
		outside := filepath.Join(t.TempDir(), "outside.yaml")
		if err := os.WriteFile(outside, moduleManifest(t, victim), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatal(err)
		}

		_, _, err := loadCRDs(ctx, env, moduleTransport(t))
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("loadCRDs(symlinked cache file) = %v, want a symlink refusal", err)
		}
		// And the file behind the link is untouched: the refusal never
		// wrote through it.
		got, err := os.ReadFile(outside)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(moduleManifest(t, victim)) {
			t.Error("the file behind the symlink changed; a refusal must never write through it")
		}
	})

	t.Run("a cache with no files at all fetches everything (nothing half-staged looks cached)", func(t *testing.T) {
		env := cacheEnv(t)
		// The directory exists (CacheDir created it) but holds nothing.
		tr := moduleTransport(t)
		objs, cached, err := loadCRDs(ctx, env, tr)
		if err != nil {
			t.Fatal(err)
		}
		if cached || len(objs) != len(CRDSources) || len(tr.requests) != len(CRDSources) {
			t.Fatalf("loadCRDs(empty dir) = (%d objs, cached %v, %d fetches), want a full cold fetch",
				len(objs), cached, len(tr.requests))
		}
	})
}

// The load classifications the replacement decision depends on,
// directly on the cache (no fetch involved).
func TestCacheLoadClassification(t *testing.T) {
	ctx := context.Background()

	// stage returns a warm cache (every entry fetched and installed
	// from the pinned module's bytes) over its own environment.
	stage := func(t *testing.T) crdCache {
		t.Helper()
		env := cacheEnv(t)
		if _, _, err := loadCRDs(ctx, env, moduleTransport(t)); err != nil {
			t.Fatal(err)
		}
		return crdCache{dir: cacheDir(t, env)}
	}

	t.Run("a present, digest-matching cache classifies as valid", func(t *testing.T) {
		cache := stage(t)
		objs, missing, err := cache.load()
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 0 {
			t.Fatalf("missing = %v, want none", missing)
		}
		for i, obj := range objs {
			if obj == nil {
				t.Fatalf("object %d is nil with no missing entry reported", i)
			}
		}
	})

	t.Run("a corrupted file classifies for refetch, not refusal", func(t *testing.T) {
		cache := stage(t)
		path := cache.file(CRDSources[0].Name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		b[0] = '#' // same shape, different bytes
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		objs, missing, err := cache.load()
		if err != nil {
			t.Fatalf("load(corrupted file) = %v, want a refetch classification, not an error", err)
		}
		if len(missing) != 1 || missing[0] != 0 || objs[0] != nil {
			t.Fatalf("load = (objs[0] %v, missing %v), want exactly entry 0 for refetch", objs[0], missing)
		}
	})

	t.Run("an oversized file classifies for refetch, not refusal, and the read stops at the bound", func(t *testing.T) {
		cache := stage(t)
		path := cache.file(CRDSources[0].Name)
		// A private, caller-owned regular file past the pin's bound:
		// no legitimate entry is larger than the largest pinned
		// manifest, so this cannot be one.
		junk := bytes.Repeat([]byte("# planted\n"), ModuleMaxManifestBytes/10+2)
		if len(junk) <= ModuleMaxManifestBytes {
			t.Fatalf("planted file is %d bytes, want more than the %d bound", len(junk), ModuleMaxManifestBytes)
		}
		if err := os.WriteFile(path, junk, 0o600); err != nil {
			t.Fatal(err)
		}
		// The bound engaged: the read stopped at the limit and reported
		// the sentinel, instead of reading the whole planted file and
		// rejecting it as a digest mismatch.
		if _, err := readCacheFile(path); !errors.Is(err, errOversizedEntry) {
			t.Fatalf("readCacheFile(oversized) = %v, want the oversized-entry bound", err)
		}
		objs, missing, err := cache.load()
		if err != nil {
			t.Fatalf("load(oversized file) = %v, want a refetch classification, not an error", err)
		}
		if len(missing) != 1 || missing[0] != 0 || objs[0] != nil {
			t.Fatalf("load = (objs[0] %v, missing %v), want exactly entry 0 for refetch", objs[0], missing)
		}
	})

	t.Run("a missing directory classifies every entry for refetch", func(t *testing.T) {
		cache := crdCache{dir: filepath.Join(t.TempDir(), "absent")}
		objs, missing, err := cache.load()
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != len(CRDSources) {
			t.Fatalf("missing = %d entries, want all %d", len(missing), len(CRDSources))
		}
		for i, obj := range objs {
			if obj != nil {
				t.Fatalf("object %d is not nil on a missing directory", i)
			}
		}
	})

	t.Run("a shared cache file is a refusal, not a refetch", func(t *testing.T) {
		cache := stage(t)
		path := cache.file(CRDSources[0].Name)
		if err := os.Chmod(path, 0o640); err != nil { // group-readable: outside the boundary
			t.Fatal(err)
		}
		_, _, err := cache.load()
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("load(shared file) = %v, want a boundary refusal", err)
		}
	})
}

// The downgrade regression at unit level: Established is not enough — a CRD
// replaced by its placeholder (established, with a schema that defines
// nothing and preserves everything) FAILs the check naming the CRD,
// while the real one passes. The fixtures carry live schemas because
// the schema is the check: the api-approved marker alone would pass a
// schema-less CRD.
func TestVerifyDistinguishesRealCRDsFromPlaceholders(t *testing.T) {
	// The two live root schemas the check distinguishes: the real
	// openshift/api one defines properties and preserves nothing; the
	// config-openshift placeholder preserves every field and defines
	// none.
	realSchema := `"schema":{"openAPIV3Schema":{"type":"object","properties":{"spec":{},"status":{}}}}`
	placeholderSchema := `"schema":{"openAPIV3Schema":{"type":"object","x-kubernetes-preserve-unknown-fields":true}}`

	// crdBody renders one live CRD: Established, carrying the given
	// root schema ("" renders a version with no schema at all) and,
	// when marker is set, the api-approved annotation a real CRD
	// carries.
	crdBody := func(name, schema string, marker bool) string {
		annotations := ""
		if marker {
			annotations = fmt.Sprintf(`,%q:{%q:"https://github.com/openshift/api/pull/470"}`,
				"annotations", realCRDAnnotation)
		}
		version := `{"name":"v1","served":true,"storage":true`
		if schema != "" {
			version += "," + schema
		}
		version += "}"
		return fmt.Sprintf(`{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition",
			"metadata":{"name":%q%s},"spec":{"versions":[%s]},
			"status":{"conditions":[{"type":"Established","status":"True"}]}}`,
			name, annotations, version)
	}

	// allRealExcept serves every CRD in the real shape — schema and
	// marker — except the names in overrides, which get the given
	// bodies verbatim.
	allRealExcept := func(overrides map[string]string) map[string]string {
		bodies := make(map[string]string, len(CRDSources))
		for _, src := range CRDSources {
			if body, ok := overrides[src.Name]; ok {
				bodies[src.Name] = body
				continue
			}
			bodies[src.Name] = crdBody(src.Name, realSchema, true)
		}
		return bodies
	}

	kubeconfigFor := func(t *testing.T, url string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "kubeconfig")
		content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
users:
- name: fake
  user: {}
`, url)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	envFor := func(t *testing.T, bodies map[string]string) layers.Env {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"
			if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) {
				http.NotFound(w, r)
				return
			}
			body, ok := bodies[strings.TrimPrefix(r.URL.Path, prefix)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return layers.Env{KubeconfigPath: kubeconfigFor(t, srv.URL), Log: slog.New(slog.DiscardHandler)}
	}

	t.Run("all 18 real and Established passes", func(t *testing.T) {
		if err := (Layer{}).Verify(context.Background(), envFor(t, allRealExcept(nil))); err != nil {
			t.Fatalf("Verify(all real) = %v, want nil", err)
		}
	})

	t.Run("a CRD replaced by its placeholder fails, naming the CRD and the served schema", func(t *testing.T) {
		env := envFor(t, allRealExcept(map[string]string{
			"infrastructures.config.openshift.io": crdBody("infrastructures.config.openshift.io", placeholderSchema, false),
		}))
		err := (Layer{}).Verify(context.Background(), env)
		if err == nil {
			t.Fatal("Verify passed with a placeholder CRD installed under a real CRD's name")
		}
		for _, want := range []string{
			"infrastructures.config.openshift.io",
			"root schema preserves unknown fields",
			"placeholder",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("every CRD still a placeholder fails", func(t *testing.T) {
		all := map[string]string{}
		for _, src := range CRDSources {
			all[src.Name] = crdBody(src.Name, placeholderSchema, false)
		}
		err := (Layer{}).Verify(context.Background(), envFor(t, all))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d of %d", len(CRDSources), len(CRDSources))) {
			t.Fatalf("Verify(all placeholders) = %v, want all %d CRDs reported", err, len(CRDSources))
		}
	})

	t.Run("a placeholder carrying the real-CRD marker still fails", func(t *testing.T) {
		// The marker is metadata
		// a schema-only change leaves untouched, so a placeholder that
		// carries it must fail exactly like one without it.
		env := envFor(t, allRealExcept(map[string]string{
			"infrastructures.config.openshift.io": crdBody("infrastructures.config.openshift.io", placeholderSchema, true),
		}))
		err := (Layer{}).Verify(context.Background(), env)
		if err == nil {
			t.Fatal("Verify passed on a placeholder carrying the real-CRD marker; the schema, not the marker, is the check")
		}
		for _, want := range []string{
			"infrastructures.config.openshift.io",
			"root schema preserves unknown fields",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("a schema-less CRD carrying the marker fails", func(t *testing.T) {
		// The pre-fix positive fixture's exact shape — spec with no
		// schema at all, marker present — passed the annotation check
		// while serving nothing. The schema check must reject it.
		env := envFor(t, allRealExcept(map[string]string{
			"infrastructures.config.openshift.io": crdBody("infrastructures.config.openshift.io", "", true),
		}))
		err := (Layer{}).Verify(context.Background(), env)
		if err == nil {
			t.Fatal("Verify passed on a schema-less CRD carrying the real-CRD marker")
		}
		if !strings.Contains(err.Error(), "no root schema") {
			t.Errorf("error = %v, want it to name the missing root schema", err)
		}
	})

	t.Run("a real schema without the marker passes", func(t *testing.T) {
		// The converse the structural check buys: the served schema is
		// real, so the CRD is real, whatever metadata it lost.
		env := envFor(t, allRealExcept(map[string]string{
			"infrastructures.config.openshift.io": crdBody("infrastructures.config.openshift.io", realSchema, false),
		}))
		if err := (Layer{}).Verify(context.Background(), env); err != nil {
			t.Fatalf("Verify(real schema, marker stripped) = %v, want nil", err)
		}
	})
}

// The same regression discipline the other layers' Verify reads follow:
// the CLI hands Verify an unbounded context (context.Background), so a
// wedged apiserver must not outlive the read budget. The fake apiserver
// answers nothing — every CRD GET stalls far beyond the budget — and
// the verify helper must return within the budget with an error, not
// after the stall.
func TestVerifyReadIsBounded(t *testing.T) {
	const readBudget = 200 * time.Millisecond
	const stall = 3 * time.Second

	var mu sync.Mutex
	served := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		mu.Unlock()
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: verify-bounded
  cluster:
    server: %s
contexts:
- name: verify-bounded
  context:
    cluster: verify-bounded
    user: verify-bounded
current-context: verify-bounded
users:
- name: verify-bounded
  user: {}
`, srv.URL)), 0o600); err != nil {
		t.Fatal(err)
	}

	env := layers.Env{KubeconfigPath: kubeconfig, Log: slog.New(slog.DiscardHandler)}
	start := time.Now()
	err := Layer{}.verify(context.Background(), env, readBudget)
	elapsed := time.Since(start)

	mu.Lock()
	got := served
	mu.Unlock()
	if got == 0 {
		t.Fatalf("the apiserver served no request; Verify failed before its reads: %v", err)
	}
	if err == nil {
		t.Fatal("Verify passed against an apiserver that answers nothing")
	}
	if elapsed >= stall {
		t.Errorf("Verify took %s to fail, want it bounded near the %s budget", elapsed, readBudget)
	}
}
