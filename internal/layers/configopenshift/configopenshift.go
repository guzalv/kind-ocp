// Package configopenshift implements the config-openshift layer: the 13
// placeholder config.openshift.io CRDs, the three singletons, and the
// Infrastructure status that lets components probing for OpenShift —
// StackRox Sensor among them — detect the cluster as one.
//
// Ported from the config-shim part of the PoC's install_co (stackrox
// PR #23841). A plain kind cluster serves none of config.openshift.io;
// without it, an OpenShift-targeted operator either refuses to start or
// reports its platform checks NOT-APPLICABLE. The CRDs are deliberate
// placeholders — every field preserved, status subresource enabled —
// which is all the singletons need; the openshift-api-crds layer later
// supersedes them with the real openshift/api CRDs (hence this layer's
// place before it in the registration order).
//
// Because of that supersede, Apply bootstraps the placeholder CRDs with
// create-only semantics: each CRD is created when — and only when — no
// CRD of its name exists yet. An existing CRD (this layer's own
// placeholder from an earlier run, the real openshift/api CRD the
// openshift-api-crds layer installed, or a foreign one) is left exactly
// as it is. Server-side applying the placeholders instead would
// overwrite the real CRDs with the placeholder schemas — a re-create
// whose later fetch fails would downgrade a working cluster, the exact
// finding of the increment-4 review. The singletons and the
// Infrastructure status stay server-side applies: nothing supersedes
// those objects, and re-applying them is a converge, not a downgrade.
//
// The Infrastructure status is the layer's load-bearing detail, in two
// ways. It is written through the status subresource — the CRDs enable
// it, and a main-resource apply cannot set status (a ported PoC
// gotcha) — and its platformStatus is the exact input Sensor's
// OpenShift detection reads. The status content is fixed data
// describing the kind cluster's shape, ported verbatim from the PoC's
// `kubectl patch infrastructure cluster --subresource=status`.
//
// The PoC applied the singletons immediately after the CRDs without
// waiting for establishment and got away with it on a warm apiserver;
// this layer waits for Established first (the same gotcha the olm layer
// ports), so a cold apiserver cannot reject the singletons. The wait
// covers every placeholder CRD, created or pre-existing alike: the
// singletons follow it, whatever form of the CRDs is installed.
//
// The layer has no settings: everything it applies is embedded, and it
// consumes no external input there would be a reason to override
// (AC-4.3 covers pins; this layer has none). Waits and reads are
// bounded by fixed budgets: crdEstablishedTimeout for CRD
// establishment, readTimeout for Verify's read.
package configopenshift

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/stackrox/kind-ocp/assets"
	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/layers"
)

// Name is the layer's stable identifier, as referenced by profiles and
// the config file.
const Name = "config-openshift"

// The API the layer installs: the group every placeholder CRD defines
// and every singleton lives in, and the group/version the singletons
// carry.
const (
	configAPIGroup   = "config.openshift.io"
	configAPIVersion = "config.openshift.io/v1"
)

// infrastructureName is the singleton whose status Apply writes and
// Verify reads: OpenShift's cluster-wide Infrastructure object.
const infrastructureName = "cluster"

// infrastructureGVR is the Infrastructure singleton's resource path,
// fixed by the infrastructures.config.openshift.io CRD the layer
// applies. Verify reads through it directly — no discovery round —
// because the layer owns the CRD that defines the mapping.
var infrastructureGVR = schema.GroupVersionResource{
	Group:    configAPIGroup,
	Version:  "v1",
	Resource: "infrastructures",
}

// Wait and read budgets. Fixed constants, not settings: AC-3.1
// requires bounded waits, and no documented use case overrides a
// budget.
const (
	// crdEstablishedTimeout bounds the wait for the placeholder CRDs
	// to reach Established. Their schemas are trivial, but
	// establishment runs on the apiserver's CRD controller, which
	// shares time with a cluster that has just booted.
	crdEstablishedTimeout = 2 * time.Minute

	// readTimeout bounds Verify's read of the Infrastructure singleton.
	readTimeout = 2 * time.Minute
)

// Layer applies the config.openshift.io placeholder API. It is
// stateless; construct once.
type Layer struct{}

// Name returns "config-openshift".
func (Layer) Name() string { return Name }

// Apply converges the cluster to the layer's desired state: bootstrap
// the placeholder CRDs (created only where absent, never overwriting an
// existing CRD of the same name — see the package comment for why),
// wait for all of them to be Established, apply the three singletons,
// then write the Infrastructure status through the status subresource.
// Re-running is a converge: the bootstrap skips what exists, and the
// server-side applies of the same content under the same field manager
// are no-ops, on the singletons and the status subresource alike.
func (l Layer) Apply(ctx context.Context, env layers.Env) error {
	crds, singletons, err := loadAssets()
	if err != nil {
		return err
	}

	client, err := kube.NewClient(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	bootstrapped := 0
	for _, crd := range crds {
		created, err := client.CreateAbsentCRD(ctx, crd)
		if err != nil {
			return fmt.Errorf("bootstrapping the config.openshift.io CRDs: %w", err)
		}
		if created {
			bootstrapped++
		}
	}
	env.Log.Info("bootstrapping the config.openshift.io placeholder API",
		"crds", len(crds), "created", bootstrapped, "alreadyPresent", len(crds)-bootstrapped)
	// No singleton may be applied before this passes: the singletons
	// are objects of the CRDs' kinds, and the apiserver rejects those
	// until the CRDs are established — whichever form of the CRDs is
	// installed, the placeholder this run created or the real one a
	// later layer left in place.
	if err := kube.WaitForCRDsEstablished(ctx, client.Dynamic, crdEstablishedTimeout, crdNames(crds)); err != nil {
		return fmt.Errorf("waiting for the config.openshift.io CRDs: %w", err)
	}
	if err := client.Apply(ctx, kube.LayerFieldManager(Name), singletons); err != nil {
		return fmt.Errorf("applying the config.openshift.io singletons: %w", err)
	}
	if err := client.ApplyStatus(ctx, kube.LayerFieldManager(Name), infrastructureStatusObject()); err != nil {
		return fmt.Errorf("writing the Infrastructure status: %w", err)
	}
	return nil
}

// Verify checks the layer's postconditions and mutates nothing:
// Infrastructure/cluster is readable and carries status.platformStatus,
// the input Sensor's OpenShift detection reads. The read is bounded by
// readTimeout.
func (l Layer) Verify(ctx context.Context, env layers.Env) error {
	return l.verify(ctx, env, readTimeout)
}

// verify is Verify with an explicit read budget. Verify passes the
// production bound (readTimeout); the bounded-read regression test
// drives this helper with a short budget, which is why the budget is a
// parameter and not a setting.
func (l Layer) verify(ctx context.Context, env layers.Env, readBudget time.Duration) error {
	client, err := kube.NewClient(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	// The read runs under a deadline context: the caller's context can
	// be unbounded (the CLI passes context.Background), so an
	// apiserver that wedges the response must not outlive the budget.
	readCtx, cancel := context.WithTimeout(ctx, readBudget)
	infra, err := client.Dynamic.Resource(infrastructureGVR).Get(readCtx, infrastructureName, metav1.GetOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("reading Infrastructure/%s: %w (are the config-openshift CRDs applied?)",
			infrastructureName, err)
	}
	return checkInfrastructureStatus(infra)
}

// checkInfrastructureStatus reports whether the Infrastructure object
// carries a non-empty status.platformStatus.
func checkInfrastructureStatus(infra *unstructured.Unstructured) error {
	platformStatus, found, err := unstructured.NestedMap(infra.Object, "status", "platformStatus")
	if err != nil {
		return fmt.Errorf("reading Infrastructure/%s status.platformStatus: %w", infrastructureName, err)
	}
	if !found || len(platformStatus) == 0 {
		return fmt.Errorf("the Infrastructure/%s singleton carries no status.platformStatus: the status write did not happen or was lost — re-run create",
			infrastructureName)
	}
	return nil
}

// infrastructureStatusObject builds the object whose status Apply
// converges. The status is the PoC's patch payload, verbatim: it
// describes the kind cluster's shape — no cloud platform (None), a
// self-managed HighlyAvailable control plane, infrastructure name
// "kind". The object is status-only by design: the status subresource
// ignores every other field, and the singleton's spec belongs to the
// asset.
func infrastructureStatusObject() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": configAPIVersion,
		"kind":       "Infrastructure",
		"metadata":   map[string]any{"name": infrastructureName},
		"status": map[string]any{
			"infrastructureName":   "kind",
			"controlPlaneTopology": "HighlyAvailable",
			"platform":             "None",
			"platformStatus":       map[string]any{"type": "None"},
		},
	}}
}

// loadAssets decodes the layer's embedded manifests. Their structural
// contract — 13 config.openshift.io CRDs, the three singletons with
// Infrastructure/cluster among them, the CRDs' schema shape — is fixed
// by the embedded bytes, not runtime input, so TestLoadAssets pins it
// against the decoded output: an accidental asset change fails the
// build, not a create run.
func loadAssets() (crds, singletons []*unstructured.Unstructured, err error) {
	crds, err = kube.DecodeManifests(strings.NewReader(assets.ConfigOpenshiftCRDs))
	if err != nil {
		return nil, nil, fmt.Errorf("decoding the config-openshift CRD asset: %w", err)
	}
	singletons, err = kube.DecodeManifests(strings.NewReader(assets.ConfigOpenshiftObjects))
	if err != nil {
		return nil, nil, fmt.Errorf("decoding the config-openshift singleton asset: %w", err)
	}
	return crds, singletons, nil
}

// crdNames returns the CRD object names — what the Established wait
// polls, in the same order the CRDs were applied.
func crdNames(crds []*unstructured.Unstructured) []string {
	names := make([]string, 0, len(crds))
	for _, obj := range crds {
		names = append(names, obj.GetName())
	}
	return names
}
