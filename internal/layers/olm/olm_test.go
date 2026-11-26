package olm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/guzalv/kind-ocp/internal/kube"
	"github.com/guzalv/kind-ocp/layers"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestReleaseManifestURL(t *testing.T) {
	t.Run("pinned release builds the download URLs", func(t *testing.T) {
		got, err := releaseManifestURL("v0.46.0", crdsManifest)
		if err != nil {
			t.Fatal(err)
		}
		want := "https://github.com/operator-framework/operator-lifecycle-manager/releases/download/v0.46.0/crds.yaml"
		if got != want {
			t.Errorf("URL = %q, want %q", got, want)
		}
	})

	for _, version := range []string{"", "latest", "0.46.0", "v0.46", "../evil", "v0.46.0/olm"} {
		t.Run("rejects "+version, func(t *testing.T) {
			_, err := releaseManifestURL(version, olmManifest)
			if err == nil || !strings.Contains(err.Error(), "invalid OLM version") {
				t.Fatalf("releaseManifestURL(%q) = %v, want a version rejection", version, err)
			}
		})
	}

	t.Run("accepts a pre-release tag", func(t *testing.T) {
		if _, err := releaseManifestURL("v0.47.0-rc.1", olmManifest); err != nil {
			t.Fatalf("pre-release tag rejected: %v", err)
		}
	})
}

// envWithSettings builds a layer Env whose settings section comes from
// raw YAML, the same shape config resolution produces.
func envWithSettings(t *testing.T, settingsYAML string) layers.Env {
	t.Helper()
	var m map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(settingsYAML), &m); err != nil {
		t.Fatal(err)
	}
	return layers.Env{Settings: m}
}

func TestDecodeSettings(t *testing.T) {
	t.Run("defaults with no settings section", func(t *testing.T) {
		s, err := decodeSettings(layers.Env{})
		if err != nil {
			t.Fatal(err)
		}
		if s.Version != DefaultVersion {
			t.Errorf("version = %q, want the default %q", s.Version, DefaultVersion)
		}
		if s.ConfigmapRegistryImage != DefaultConfigmapRegistryImage {
			t.Errorf("configmapRegistryImage = %q, want the default pin", s.ConfigmapRegistryImage)
		}
	})

	t.Run("explicit version overrides the default", func(t *testing.T) {
		s, err := decodeSettings(envWithSettings(t, "olm:\n  version: v0.47.0\n"))
		if err != nil {
			t.Fatal(err)
		}
		if s.Version != "v0.47.0" {
			t.Errorf("version = %q, want v0.47.0", s.Version)
		}
		if s.ConfigmapRegistryImage != DefaultConfigmapRegistryImage {
			t.Errorf("configmapRegistryImage = %q, want the default pin kept", s.ConfigmapRegistryImage)
		}
	})

	t.Run("unknown key is an error", func(t *testing.T) {
		_, err := decodeSettings(envWithSettings(t, "olm:\n  bogus: true\n"))
		if err == nil || !strings.Contains(err.Error(), "bogus") {
			t.Fatalf("decodeSettings(unknown key) = %v, want an error naming the key", err)
		}
	})

	t.Run("removed timeout settings are an unknown-key error", func(t *testing.T) {
		// The wait budgets are fixed constants, not settings; a config
		// file still carrying the old keys must fail loudly, not
		// silently ignore them.
		_, err := decodeSettings(envWithSettings(t, "olm:\n  timeouts:\n    rollout: 90s\n"))
		if err == nil || !strings.Contains(err.Error(), "timeouts") {
			t.Fatalf("decodeSettings(timeouts) = %v, want an unknown-key error", err)
		}
	})

	t.Run("invalid version is an error", func(t *testing.T) {
		_, err := decodeSettings(envWithSettings(t, "olm:\n  version: latest\n"))
		if err == nil || !strings.Contains(err.Error(), "invalid OLM version") {
			t.Fatalf("decodeSettings(latest) = %v, want a version rejection", err)
		}
	})

	t.Run("configmapRegistryImage is overridable", func(t *testing.T) {
		const mirror = "registry.example.com/mirror/configmap-operator-registry:v1.73.0@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		s, err := decodeSettings(envWithSettings(t, "olm:\n  configmapRegistryImage: "+mirror+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if s.ConfigmapRegistryImage != mirror {
			t.Errorf("configmapRegistryImage = %q, want the override %q", s.ConfigmapRegistryImage, mirror)
		}
	})

	t.Run("empty configmapRegistryImage is an error", func(t *testing.T) {
		_, err := decodeSettings(envWithSettings(t, "olm:\n  configmapRegistryImage: ''\n"))
		if err == nil || !strings.Contains(err.Error(), "configmapRegistryImage") {
			t.Fatalf("decodeSettings(empty pin) = %v, want a non-empty rejection", err)
		}
	})
}

// Apply validates its settings before it touches a cluster or the
// network: a bad version fails the create step with the configuration
// mistake, not a fetch error.
func TestApplyRejectsBadSettingsBeforeAnyWork(t *testing.T) {
	l := Layer{}
	if l.Name() != Name {
		t.Errorf("Name() = %q, want %q", l.Name(), Name)
	}
	err := l.Apply(context.Background(), envWithSettings(t, "olm:\n  version: latest\n"))
	if err == nil || !strings.Contains(err.Error(), "invalid OLM version") {
		t.Fatalf("Apply(bad version) = %v, want the version rejection", err)
	}
	if err := l.Verify(context.Background(), envWithSettings(t, "olm:\n  version: latest\n")); err == nil ||
		!strings.Contains(err.Error(), "invalid OLM version") {
		t.Fatalf("Verify(bad version) = %v, want the version rejection", err)
	}
}

func TestPartitionCRDs(t *testing.T) {
	// Decode through the real parse path: document order must survive
	// partitioning, and comment-only documents must not become objects.
	objs, err := kube.DecodeManifests(strings.NewReader(`
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: clusterserviceversions.operators.coreos.com
---
# a comment-only document
apiVersion: v1
kind: Namespace
metadata:
  name: olm
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: catalogsources.operators.coreos.com
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
	if len(objs) != 4 {
		t.Fatalf("decoded %d objects, want 4", len(objs))
	}

	crds, rest := partitionCRDs(objs)
	if len(crds) != 2 || crds[0].GetName() != "clusterserviceversions.operators.coreos.com" ||
		crds[1].GetName() != "catalogsources.operators.coreos.com" {
		t.Errorf("crds = %v, want the two CRDs in document order", objNames(crds))
	}
	if len(rest) != 2 || rest[0].GetName() != "olm" || rest[1].GetName() != "olm-operator" {
		t.Errorf("rest = %v, want namespace and deployment in document order", objNames(rest))
	}
	if got := crdNames(crds); len(got) != 2 || got[0] != "clusterserviceversions.operators.coreos.com" ||
		got[1] != "catalogsources.operators.coreos.com" {
		t.Errorf("crdNames = %v, want the CRD names in apply order", got)
	}
}

func objNames(objs []*unstructured.Unstructured) []string {
	names := make([]string, 0, len(objs))
	for _, obj := range objs {
		names = append(names, obj.GetName())
	}
	return names
}

// releaseExcerpt is the slice of the real v0.46.0 olm.yaml that
// pinReleaseImages touches: the catalog-operator Deployment with its
// verbatim argument list (the floating --configmapServerImage between
// two digest-pinned neighbors), the release's operatorhubio-catalog
// CatalogSource, and an unrelated CatalogSource that must survive.
const releaseExcerpt = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: catalog-operator
  namespace: olm
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: catalog-operator
        command:
        - /bin/catalog
        args:
        - '--namespace'
        - olm
        - --configmapServerImage=quay.io/operator-framework/configmap-operator-registry:latest
        - --opmImage=quay.io/operator-framework/opm@sha256:e5a6220603fb4504d58c6e3e488386b817e3695c906a62ee0370b5faedc3799a
        - --util-image
        - quay.io/operator-framework/olm@sha256:4404599eb7b7736bdbd0f36260a60599f32acb6edeaf7d0c54bb10b36619e5fe
        image: quay.io/operator-framework/olm@sha256:4404599eb7b7736bdbd0f36260a60599f32acb6edeaf7d0c54bb10b36619e5fe
---
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: operatorhubio-catalog
  namespace: olm
spec:
  sourceType: grpc
  image: quay.io/operatorhubio/catalog:latest
  updateStrategy:
    registryPoll:
      interval: 60m
---
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: compliance-operator-catalog
  namespace: olm
spec:
  sourceType: grpc
  image: ghcr.io/complianceascode/compliance-operator-catalog@sha256:b5ad7b09f3fa12d99821b7768d6af54eba02e9328feb71c5aee7624401d5a3e8
`

// releaseExcerptNoFlag is releaseExcerpt with the floating
// --configmapServerImage argument removed: the shape a future release
// would have if it dropped or renamed the flag.
const releaseExcerptNoFlag = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: catalog-operator
  namespace: olm
spec:
  replicas: 1
  template:
    spec:
      containers:
      - name: catalog-operator
        command:
        - /bin/catalog
        args:
        - '--namespace'
        - olm
        - --opmImage=quay.io/operator-framework/opm@sha256:e5a6220603fb4504d58c6e3e488386b817e3695c906a62ee0370b5faedc3799a
        - --util-image
        - quay.io/operator-framework/olm@sha256:4404599eb7b7736bdbd0f36260a60599f32acb6edeaf7d0c54bb10b36619e5fe
        image: quay.io/operator-framework/olm@sha256:4404599eb7b7736bdbd0f36260a60599f32acb6edeaf7d0c54bb10b36619e5fe
`

// catalogOperatorArgs returns the catalog-operator container's args
// from the decoded excerpt, failing the test if the object is missing.
func catalogOperatorArgs(t *testing.T, objs []*unstructured.Unstructured) []string {
	t.Helper()
	for _, obj := range objs {
		if obj.GetKind() != "Deployment" || obj.GetName() != catalogDeployment {
			continue
		}
		containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
		if err != nil || len(containers) != 1 {
			t.Fatalf("reading the %s Deployment's containers: %v (%d containers)", catalogDeployment, err, len(containers))
		}
		args, _, err := unstructured.NestedSlice(containers[0].(map[string]any), "args")
		if err != nil {
			t.Fatalf("reading the catalog-operator args: %v", err)
		}
		got := make([]string, 0, len(args))
		for _, a := range args {
			s, ok := a.(string)
			if !ok {
				t.Fatalf("non-string arg %v", a)
			}
			got = append(got, s)
		}
		return got
	}
	t.Fatalf("no %s Deployment in %v", catalogDeployment, objNames(objs))
	return nil
}

func decodeExcerpt(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	objs, err := kube.DecodeManifests(strings.NewReader(releaseExcerpt))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("decoded %d objects, want 3", len(objs))
	}
	return objs
}

// The release objects ship two floating image references by default:
// the operatorhubio-catalog CatalogSource at
// quay.io/operatorhubio/catalog:latest with a 60m poll, and the
// catalog-operator's --configmapServerImage at
// quay.io/operator-framework/configmap-operator-registry:latest.
// pinReleaseImages must close both, and only those.
func TestPinReleaseImages(t *testing.T) {
	t.Run("pins the configmap registry image and drops the public catalog", func(t *testing.T) {
		objs, err := pinReleaseImages(decodeExcerpt(t), DefaultConfigmapRegistryImage)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range objNames(objs) {
			if name == defaultCatalogSource {
				t.Errorf("the %s CatalogSource survived; want it dropped", defaultCatalogSource)
			}
		}
		if len(objs) != 2 {
			t.Fatalf("objects = %v, want the deployment and the other catalog", objNames(objs))
		}
		args := catalogOperatorArgs(t, objs)
		want := configmapServerImageArg + DefaultConfigmapRegistryImage
		found := false
		for _, a := range args {
			if a == want {
				found = true
			}
			if strings.HasPrefix(a, configmapServerImageArg) && a != want {
				t.Errorf("arg %q is a configmapServerImage that is not the pin", a)
			}
		}
		if !found {
			t.Errorf("args = %v, want %q", args, want)
		}
	})

	t.Run("leaves the pinned neighbors and the argument order intact", func(t *testing.T) {
		before := catalogOperatorArgs(t, decodeExcerpt(t))
		objs, err := pinReleaseImages(decodeExcerpt(t), DefaultConfigmapRegistryImage)
		if err != nil {
			t.Fatal(err)
		}
		after := catalogOperatorArgs(t, objs)
		if len(before) != len(after) {
			t.Fatalf("arg count changed: %v -> %v", before, after)
		}
		for i := range before {
			if before[i] == after[i] {
				continue
			}
			if !strings.HasPrefix(after[i], configmapServerImageArg) {
				t.Errorf("arg %d changed from %q to %q; only the pin may change", i, before[i], after[i])
			}
		}
		// The resulting registry reference must carry a digest: the
		// equality check above compares the production constant against
		// itself, so it would pass even if the constant drifted to
		// another floating tag.
		for _, a := range after {
			if strings.HasPrefix(a, configmapServerImageArg) && !strings.Contains(a, "@sha256:") {
				t.Errorf("arg %q is not digest-pinned", a)
			}
		}
	})

	t.Run("drops only the release's catalog, not other CatalogSources", func(t *testing.T) {
		objs, err := pinReleaseImages(decodeExcerpt(t), DefaultConfigmapRegistryImage)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range objNames(objs) {
			if name == "compliance-operator-catalog" {
				return
			}
		}
		t.Errorf("objects = %v, want the unrelated CatalogSource kept", objNames(objs))
	})

	t.Run("honors a pin override", func(t *testing.T) {
		const mirror = "registry.example.com/mirror/configmap-operator-registry:v1.73.0@sha256:0000000000000000000000000000000000000000000000000000000000000000"
		objs, err := pinReleaseImages(decodeExcerpt(t), mirror)
		if err != nil {
			t.Fatal(err)
		}
		args := catalogOperatorArgs(t, objs)
		for _, a := range args {
			if strings.HasPrefix(a, configmapServerImageArg) && a != configmapServerImageArg+mirror {
				t.Errorf("arg = %q, want the override %q", a, mirror)
			}
		}
	})

	t.Run("missing --configmapServerImage argument fails closed", func(t *testing.T) {
		objs, err := kube.DecodeManifests(strings.NewReader(releaseExcerptNoFlag))
		if err != nil {
			t.Fatal(err)
		}
		_, err = pinReleaseImages(objs, DefaultConfigmapRegistryImage)
		if err == nil || !strings.Contains(err.Error(), "carries no "+configmapServerImageArg) {
			t.Fatalf("pinReleaseImages(no flag) = %v, want a fail-closed error", err)
		}
	})

	t.Run("missing catalog-operator Deployment fails closed", func(t *testing.T) {
		objs := decodeExcerpt(t)
		var kept []*unstructured.Unstructured
		for _, obj := range objs {
			if obj.GetKind() != "Deployment" {
				kept = append(kept, obj)
			}
		}
		_, err := pinReleaseImages(kept, DefaultConfigmapRegistryImage)
		if err == nil || !strings.Contains(err.Error(), "no olm/catalog-operator Deployment") {
			t.Fatalf("pinReleaseImages(no deployment) = %v, want a fail-closed error", err)
		}
	})
}

// availableDeployment is the body the fake apiserver below serves for
// the operator Deployment reads: Available at its current generation,
// so the Deployment half of Verify passes immediately.
const availableDeployment = `{"kind":"Deployment","apiVersion":"apps/v1",` +
	`"metadata":{"name":"olm-operator","namespace":"olm","generation":1},` +
	`"spec":{"replicas":1},` +
	`"status":{"observedGeneration":1,"availableReplicas":1,` +
	`"conditions":[{"type":"Available","status":"True"}]}}`

// Verify's discovery read — the GroupVersionServed call — must be
// bounded: under the caller's unbounded context, an apiserver that
// wedged /api and /apis would hang
// `kind-ocp verify` indefinitely (the CLI hands Verify
// context.Background) while every neighboring read was bounded. The
// fake apiserver answers the operator Deployment reads immediately
// (Available) and stalls both discovery endpoints far beyond the read
// budget; the reads must fail within the bound, not after the stalls.
//
// The budget is exercised through the internal verify helper with a
// short budget: production waits are fixed constants (AC-3.1 requires
// bounded waits, not configurable ones), and Verify simply passes
// rolloutTimeout to this helper.
func TestVerifyDiscoveryReadIsBounded(t *testing.T) {
	const readBudget = 200 * time.Millisecond
	const stall = 3 * time.Second
	var mu sync.Mutex
	served := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api", "/apis":
			// Hold the response far beyond the rollout timeout, and
			// stop holding once the client cancels: a bounded read
			// cancels at its deadline, an unbounded one waits the stall
			// out (twice — /api then /apis).
			select {
			case <-time.After(stall):
				if r.URL.Path == "/api" {
					_, _ = w.Write([]byte(`{"kind":"APIVersions","apiVersion":"v1","versions":["v1"]}`))
				} else {
					_, _ = w.Write([]byte(`{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`))
				}
			case <-r.Context().Done():
			}
		default:
			mu.Lock()
			served[r.URL.Path]++
			mu.Unlock()
			_, _ = w.Write([]byte(availableDeployment))
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

	env := layers.Env{
		KubeconfigPath: kubeconfig,
		Log:            slog.New(slog.DiscardHandler),
	}

	start := time.Now()
	err := Layer{}.verify(context.Background(), env, readBudget)
	elapsed := time.Since(start)

	// The Deployment reads must have been served: they prove Verify got
	// past settings decoding and client building, so the failure comes
	// from the discovery read, not from test wiring.
	mu.Lock()
	for _, name := range []string{olmOperatorDeployment, catalogDeployment} {
		if served["/apis/apps/v1/namespaces/olm/deployments/"+name] == 0 {
			mu.Unlock()
			t.Fatalf("deployment %s was never read; Verify failed before it: %v", name, err)
		}
	}
	mu.Unlock()

	if err == nil {
		t.Fatal("Verify must fail: the apiserver serves no OLM API")
	}
	if !strings.Contains(err.Error(), "discovering the server's API groups") {
		t.Errorf("error = %v, want the bounded discovery read to fail it", err)
	}
	if elapsed > time.Second {
		t.Errorf("%s read budget took %s; the stalled discovery read was not cancelled at the deadline", readBudget, elapsed)
	}
}

// fileTransport serves canned manifest bodies keyed by the URL's last
// path element and records every request, so fetchRelease's two
// downloads are tested without network.
type fileTransport struct {
	bodies   map[string]string
	requests []string
}

func (t *fileTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, req.URL.String())
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/yaml"}},
		Body:       io.NopCloser(strings.NewReader(t.bodies[path.Base(req.URL.Path)])),
		Request:    req,
	}, nil
}

// The version pin does not bind the bytes —
// GitHub release assets are mutable under a tag — and the manifest
// bytes are applied with cluster-admin credentials. fetchRelease must
// verify the default release's files against their reviewed digests
// before decoding anything, and must trust an overridden version as
// pinned (the documented operator trust).
func TestFetchRelease(t *testing.T) {
	t.Run("default release bytes are verified before decoding", func(t *testing.T) {
		// Garbage bodies: even undecodable bytes must produce the
		// digest error, proving verification runs first, and the
		// fetch must stop at the first mismatching file.
		tr := &fileTransport{bodies: map[string]string{
			crdsManifest: "not the reviewed crds.yaml",
			olmManifest:  "not the reviewed olm.yaml",
		}}
		_, err := fetchRelease(context.Background(), tr, DefaultVersion)
		if err == nil || !strings.Contains(err.Error(), "digest") {
			t.Fatalf("fetchRelease(default, unreviewed bytes) = %v, want a digest mismatch", err)
		}
		if !strings.Contains(err.Error(), crdsManifest) {
			t.Errorf("error = %v, want it to name %s", err, crdsManifest)
		}
		if len(tr.requests) != 1 {
			t.Errorf("%d requests, want the fetch to stop at the first mismatching file", len(tr.requests))
		}
	})

	t.Run("overridden version is fetched without a digest check", func(t *testing.T) {
		tr := &fileTransport{bodies: map[string]string{
			crdsManifest: "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: clusterserviceversions.operators.coreos.com\n",
			olmManifest:  "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: olm\n",
		}}
		objs, err := fetchRelease(context.Background(), tr, "v0.47.0")
		if err != nil {
			t.Fatal(err)
		}
		if len(objs) != 2 || objs[0].GetKind() != "CustomResourceDefinition" || objs[1].GetName() != "olm" {
			t.Fatalf("objects = %v, want the CRD then the namespace in fetch order", objNames(objs))
		}
		want := []string{
			"https://github.com/operator-framework/operator-lifecycle-manager/releases/download/v0.47.0/crds.yaml",
			"https://github.com/operator-framework/operator-lifecycle-manager/releases/download/v0.47.0/olm.yaml",
		}
		if !slices.Equal(tr.requests, want) {
			t.Errorf("requests = %v, want %v", tr.requests, want)
		}
	})
}

func TestVerifyReviewedDigest(t *testing.T) {
	body := []byte(namespaceBody)

	// The production pins bind the real release bytes, which the unit
	// suite does not embed (the integration test fetches those). The
	// comparison is therefore tested against caller-built tables; the
	// production table stays fixed package data that no test touches.
	otherBody := []byte("some other manifest bytes\n")

	t.Run("matching digest passes", func(t *testing.T) {
		pins := map[string]string{crdsManifest: sha256Hex(body)}
		if err := verifyReviewedDigest(pins, DefaultVersion, crdsManifest, body); err != nil {
			t.Fatalf("verifyReviewedDigest(match) = %v, want nil", err)
		}
	})

	t.Run("mismatching digest names both digests", func(t *testing.T) {
		pins := map[string]string{olmManifest: sha256Hex(otherBody)}
		err := verifyReviewedDigest(pins, DefaultVersion, olmManifest, body)
		if err == nil || !strings.Contains(err.Error(), "sha256:") {
			t.Fatalf("verifyReviewedDigest(mismatch) = %v, want an error naming both digests", err)
		}
		if !strings.Contains(err.Error(), sha256Hex(body)) || !strings.Contains(err.Error(), sha256Hex(otherBody)) {
			t.Errorf("error = %v, want it to name the got and want digests", err)
		}
	})

	t.Run("overridden version is trusted as pinned", func(t *testing.T) {
		pins := map[string]string{crdsManifest: sha256Hex(otherBody)}
		if err := verifyReviewedDigest(pins, "v0.47.0", crdsManifest, body); err != nil {
			t.Fatalf("verifyReviewedDigest(override) = %v, want nil", err)
		}
	})

	t.Run("unknown default-release file fails closed", func(t *testing.T) {
		pins := map[string]string{crdsManifest: sha256Hex(body)}
		err := verifyReviewedDigest(pins, DefaultVersion, "rbac.yaml", body)
		if err == nil || !strings.Contains(err.Error(), "no reviewed digest") {
			t.Fatalf("verifyReviewedDigest(unknown file) = %v, want a fail-closed error", err)
		}
	})

	t.Run("reviewed digests are sha256 hex", func(t *testing.T) {
		// A mistyped constant must fail here, not at the first real
		// fetch on some cluster.
		for file, digest := range reviewedReleaseDigests {
			raw, err := hex.DecodeString(digest)
			if err != nil || len(raw) != sha256.Size {
				t.Errorf("digest of %s is not 64 hex characters: %q", file, digest)
			}
		}
	})
}

// namespaceBody is a minimal decodable manifest body.
const namespaceBody = "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: olm\n"
