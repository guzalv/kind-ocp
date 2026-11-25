//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stackrox/kind-ocp/assets"
	"github.com/stackrox/kind-ocp/config"
	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/internal/layers/openshiftapicrds"
	"github.com/stackrox/kind-ocp/kindocp"
)

// increment4Layers is everything the openshift-api-crds layer builds on,
// in registration order: the placeholder CRDs and singletons it
// supersedes, the node labels OLM's operators schedule on, OLM itself,
// and the new layer. The profile is not "full" — full grows as later
// increments register layers, and this test is only about increment 4.
var increment4Layers = []string{"config-openshift", "node-labels", "olm", "openshift-api-crds"}

// increment4CRDNames is the 18 CRDs this test expects the layer to have
// installed, written out independently of the layer's table so a table
// drift fails here, not only in the layer's own checks.
var increment4CRDNames = []string{
	"apiservers.config.openshift.io",
	"authentications.config.openshift.io",
	"clusteroperators.config.openshift.io",
	"clusterversions.config.openshift.io",
	"consoles.config.openshift.io",
	"featuregates.config.openshift.io",
	"images.config.openshift.io",
	"infrastructures.config.openshift.io",
	"ingresses.config.openshift.io",
	"networks.config.openshift.io",
	"oauths.config.openshift.io",
	"proxies.config.openshift.io",
	"schedulers.config.openshift.io",
	"ingresscontrollers.operator.openshift.io",
	"kubeapiservers.operator.openshift.io",
	"networks.operator.openshift.io",
	"openshiftapiservers.operator.openshift.io",
	"securitycontextconstraints.security.openshift.io",
}

// crdGVR addresses CustomResourceDefinition objects for direct reads.
var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// TestCreateWithOpenshiftAPICRDsLayer exercises increment 4's layer end
// to end on a real Docker daemon: create with the four-layer profile,
// fetch-and-stage the 18 real CRDs from the pinned openshift/api commit
// into the state root's cache, supersede the config-openshift
// placeholders under the same field manager, verify passes every layer
// check, the singletons survive the supersede, and a re-run of create
// reconciles. Delete then removes the cache with the rest of the
// prefix's state.
func TestCreateWithOpenshiftAPICRDsLayer(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"
	stateRoot := filepath.Join(t.TempDir(), "state")

	increment4 := func(cfg *config.Config) {
		cfg.Profiles = map[string]config.Profile{"increment-4": {Layers: increment4Layers}}
		cfg.Profile = "increment-4"
	}
	client, _ := testClientAt(t, prefix, stateRoot, increment4, nil)
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	// 1. Create with the four layers applied. This is the first run
	// against a cold cache: the 18 manifests are fetched from the
	// pinned commit over the network.
	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(records) != 1 || records[0].Name != cluster {
		t.Fatalf("records = %+v, want one record for %s", records, cluster)
	}
	if records[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("record status = %s, want complete", records[0].Status)
	}
	if !slices.Equal(records[0].Layers, increment4Layers) {
		t.Fatalf("record layers = %v, want %v in registration order", records[0].Layers, increment4Layers)
	}

	// 2. Verify passes, including every layer check.
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment4Layers, "after create")

	// 3. The layer's postconditions hold independently of its own
	// Verify, read through the exported kubeconfig.
	kcPath, err := client.Kubeconfig(ctx, cluster)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	kubeClient, err := kube.NewClient(kcPath)
	if err != nil {
		t.Fatalf("building kube client: %v", err)
	}

	// A short bound: the CRDs are already established (Apply waited for
	// them), so this is an immediate read through the public helper.
	if err := kube.WaitForCRDsEstablished(ctx, kubeClient.Dynamic, 30*time.Second, increment4CRDNames); err != nil {
		t.Errorf("openshift/api CRDs not established: %v", err)
	}

	// The supersede happened, not just a parallel install: the real
	// infrastructures CRD carries the openshift/api review annotations
	// the placeholder asset has none of.
	crd, err := kubeClient.Dynamic.Resource(crdGVR).Get(ctx, "infrastructures.config.openshift.io", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the infrastructures CRD: %v", err)
	}
	if _, ok := crd.GetAnnotations()["api-approved.openshift.io"]; !ok {
		t.Errorf("infrastructures CRD carries no api-approved.openshift.io annotation; the real openshift/api CRD did not supersede the placeholder")
	}

	// The singletons survived the supersede: the placeholder CRD's
	// objects are still readable through the real CRD, with the status
	// the config-openshift layer wrote — the exact input Sensor's
	// OpenShift detection reads.
	infra, err := kubeClient.Dynamic.Resource(configGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading Infrastructure/cluster through the real CRD: %v", err)
	}
	if platformStatus, found, _ := unstructured.NestedMap(infra.Object, "status", "platformStatus"); !found || len(platformStatus) == 0 {
		t.Errorf("Infrastructure/cluster status.platformStatus lost across the CRD supersede")
	}

	// 4. The cache holds what was fetched: the pinned commit's directory
	// with all 18 manifests, each carrying exactly the bytes the pin
	// expects, under the prefix's state root, inside kind-ocp's
	// verified state tree.
	cacheDir := filepath.Join(client.StateRoot(), "cache", openshiftapicrds.Name, openshiftapicrds.ModuleCommit)
	requireStagedCache(t, cacheDir)

	// 5. Re-running create reconciles: same cluster instance, the layer
	// re-applied from the now-warm cache, record complete.
	retryRecords, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if len(retryRecords) != 1 || retryRecords[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("reconcile records = %+v, want one complete record", retryRecords)
	}
	if retryRecords[0].InstanceID != records[0].InstanceID {
		t.Fatalf("reconcile changed the instance: %q -> %q", records[0].InstanceID, retryRecords[0].InstanceID)
	}
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after reconcile: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment4Layers, "after reconcile")

	// 6. The F-2 live regression, the increment-4 review's second
	// finding: a real CRD replaced by its placeholder — the exact
	// substitution the review observed config-openshift making — passes
	// an Established-only check but must FAIL this layer's Verify,
	// naming the CRD. A re-run of create then converges the real CRD
	// back and Verify passes again.
	downgraded := downgradeCRDToPlaceholder(t, ctx, kubeClient, "infrastructures.config.openshift.io")
	if downgraded.GetAnnotations()["api-approved.openshift.io"] != "" {
		t.Fatal("the downgrade fixture failed: the CRD still carries the real-CRD annotation after the placeholder was applied over it")
	}
	corruptResults, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after the CRD downgrade: %v", err)
	}
	var crdCheck *kindocp.CheckResult
	for i := range corruptResults {
		if corruptResults[i].Check == "layer:openshift-api-crds" {
			crdCheck = &corruptResults[i]
		}
	}
	if crdCheck == nil || crdCheck.Status != kindocp.StatusFail {
		t.Fatalf("verify after the CRD downgrade = %+v, want a FAIL for layer:openshift-api-crds", corruptResults)
	}
	if !strings.Contains(crdCheck.Detail, "infrastructures.config.openshift.io") ||
		!strings.Contains(crdCheck.Detail, "root schema preserves unknown fields") {
		t.Errorf("layer:openshift-api-crds FAIL detail = %q, want it to name the downgraded CRD and the placeholder schema it now serves", crdCheck.Detail)
	}

	// The re-run converges the real CRD back (server-side apply of the
	// pinned manifest over the placeholder) and Verify passes again.
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("reconcile create after the CRD downgrade: %v", err)
	}
	recovered, err := kubeClient.Dynamic.Resource(crdGVR).Get(ctx, "infrastructures.config.openshift.io", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading the infrastructures CRD: %v", err)
	}
	requireRealCRD(t, recovered, "infrastructures.config.openshift.io", "after the downgrade recovery")
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after the downgrade recovery: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment4Layers, "after the downgrade recovery")

	// 7. Delete removes the cache with the rest of the prefix's state:
	// a later create starts from zero, the PoC's stale-state lesson.
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("cache dir after delete: %v, want it removed with the state root", err)
	}
}

// downgradeCRDToPlaceholder reproduces the mechanism of the increment-4
// review's finding: the config-openshift placeholder CRD server-side
// applied over the real CRD under the layer's own field manager, which
// is exactly what the layer's pre-fix Apply did to a converged cluster.
// It returns the downgraded CRD as the apiserver now stores it.
func downgradeCRDToPlaceholder(t *testing.T, ctx context.Context, kubeClient *kube.Client, name string) *unstructured.Unstructured {
	t.Helper()
	objs, err := kube.DecodeManifests(strings.NewReader(assets.ConfigOpenshiftCRDs))
	if err != nil {
		t.Fatalf("decoding the placeholder CRD asset: %v", err)
	}
	var placeholder *unstructured.Unstructured
	for _, obj := range objs {
		if obj.GetName() == name {
			placeholder = obj
		}
	}
	if placeholder == nil {
		t.Fatalf("the placeholder asset has no CRD named %s", name)
	}
	if err := kubeClient.Apply(ctx, kube.LayerFieldManager(openshiftapicrds.Name), []*unstructured.Unstructured{placeholder}); err != nil {
		t.Fatalf("applying the placeholder CRD over the real one: %v", err)
	}
	// The downgraded CRD reports Established too — that is the second
	// half of the review's finding — so wait for that before asserting
	// the layer's check fails for the realness reason, not a race.
	if err := kube.WaitForCRDsEstablished(ctx, kubeClient.Dynamic, 30*time.Second, []string{name}); err != nil {
		t.Fatalf("waiting for the downgraded CRD to settle: %v", err)
	}
	crd, err := kubeClient.Dynamic.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the downgraded CRD: %v", err)
	}
	return crd
}

// requireRealCRD asserts a live CRD is the real openshift/api one, not
// a config-openshift placeholder under the same name: the real-CRD
// annotation is present, the root schema defines properties, and the
// placeholder's root-level x-kubernetes-preserve-unknown-fields marker
// is gone. These are the exact observations the increment-4 review made
// about a downgraded cluster, inverted.
func requireRealCRD(t *testing.T, crd *unstructured.Unstructured, name, when string) {
	t.Helper()
	if crd.GetAnnotations()["api-approved.openshift.io"] == "" {
		t.Errorf("%s: %s carries no api-approved.openshift.io annotation; the real openshift/api CRD did not survive %s", when, name, when)
	}
	versions, _, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil || len(versions) != 1 {
		t.Fatalf("%s: %s has no single version to inspect: %v", when, name, err)
	}
	version, ok := versions[0].(map[string]any)
	if !ok {
		t.Fatalf("%s: %s version entry is not a map", when, name)
	}
	schema, found, err := unstructured.NestedMap(version, "schema", "openAPIV3Schema")
	if err != nil || !found {
		t.Fatalf("%s: %s carries no root schema: %v", when, name, err)
	}
	if _, preserve := schema["x-kubernetes-preserve-unknown-fields"]; preserve {
		t.Errorf("%s: %s root schema preserves unknown fields; the placeholder's marker is on it %s", when, name, when)
	}
	props, found, _ := unstructured.NestedMap(schema, "properties")
	if !found || len(props) == 0 {
		t.Errorf("%s: %s root schema defines no properties; the real schema's fields are gone %s", when, name, when)
	}
}

// requireStagedCache asserts the cache directory holds a complete,
// pin-matching stage: one manifest file per CRD source, each carrying
// exactly the bytes the build-time pin expects for it.
func requireStagedCache(t *testing.T, dir string) {
	t.Helper()
	if len(openshiftapicrds.ModuleDigests) != len(increment4CRDNames) {
		t.Fatalf("ModuleDigests has %d entries, want one per CRD (%d)", len(openshiftapicrds.ModuleDigests), len(increment4CRDNames))
	}
	for name, wantDigest := range openshiftapicrds.ModuleDigests {
		path := filepath.Join(dir, name+".crd.yaml")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading cached %s: %v", path, err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != wantDigest {
			t.Errorf("cached %s hashes to sha256:%s, want the pinned sha256:%s", path, got, wantDigest)
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Errorf("cached file %s: %v", path, err)
		}
	}
	// Nothing else is staged: no index, no bookkeeping.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(increment4CRDNames) {
		t.Errorf("cache dir holds %d entries, want exactly the %d manifests", len(entries), len(increment4CRDNames))
	}
}
