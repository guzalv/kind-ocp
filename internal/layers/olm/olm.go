// Package olm implements the olm layer: it installs upstream OLM
// (operator-framework) from a pinned release onto the cluster.
//
// Ported from the PoC's install_olm (stackrox PR #23841): server-side
// apply for the CRDs — the ClusterServiceVersion CRD exceeds the
// client-side last-applied-configuration annotation limit — a CRD
// Established wait before any object of the new kinds is applied, and
// rollout-complete waits (kubectl rollout status semantics) for the
// olm-operator and catalog-operator Deployments.
//
// The release fetch is pinned twice. The version selects the release
// (AC-4.1), and the default release's two manifest files are verified
// against reviewed SHA-256 digests before anything is decoded or
// applied: GitHub release assets are mutable — an asset can be
// replaced without touching its tag — and these are the bytes that get
// applied with cluster-admin credentials. An overridden version has no
// reviewed digest; whoever pins it trusts the release publisher, with
// the fetch still TLS-authenticated and https-only across redirects
// (see DefaultVersion).
//
// The release manifest ships two floating image references. Apply
// closes both (AC-4.2), handling exactly these two known fields rather
// than rewriting the manifest generically:
//
//   - the catalog-operator's --configmapServerImage argument names the
//     image ConfigMap-based CatalogSources are served from at :latest;
//     Apply replaces it with the pinned configmapRegistryImage setting.
//   - the operatorhubio-catalog CatalogSource points the cluster at
//     the public community catalog at :latest and re-polls it every 60
//     minutes. Nothing kind-ocp targets consumes it (the Compliance
//     Operator proof installs its own digest-pinned CatalogSource in
//     the olm namespace), so Apply drops it rather than pinning it. A
//     cluster that wants the public catalog applies its own
//     CatalogSource; kind-ocp never removes one already present.
//
// Settings (config file, `layers.olm` section; both optional, both
// pins overridable per AC-4.3):
//
//	version: v0.46.0                  # the pinned OLM release to install
//	configmapRegistryImage: <ref>     # pin for --configmapServerImage
//
// Waits are bounded by fixed budgets, not settings: AC-3.1 requires
// bounded waits, and no documented use case overrides a budget. The
// CRD Established wait is bounded by crdEstablishedTimeout; each
// Deployment rollout wait and each Verify read by rolloutTimeout.
package olm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/guzalv/kind-ocp/internal/kube"
	"github.com/guzalv/kind-ocp/layers"
)

// Name is the layer's stable identifier, as referenced by profiles and
// the config file.
const Name = "olm"

// The pinned release and the manifests that make it up. The version is
// the pin (AC-4.1); the manifest names are fixed by the release layout.
const (
	// DefaultVersion is the OLM release kind-ocp installs by default,
	// overridable through the layer's config section. The default's two
	// manifest files are verified against reviewed SHA-256 digests
	// (reviewedReleaseDigests). An override has no reviewed digest, so
	// whoever sets it trusts the release publisher's bytes: the fetch
	// stays TLS-authenticated github.com with https-only redirects, but
	// nothing checks the content against a review.
	DefaultVersion = "v0.46.0"

	// DefaultConfigmapRegistryImage is the pin for the one image
	// reference the OLM release leaves floating: the catalog-operator's
	// --configmapServerImage, which the release manifest sets to
	// quay.io/operator-framework/configmap-operator-registry:latest.
	// The tag is the operator-registry version this OLM release pins in
	// its go.mod (v1.73.0 for v0.46.0); the digest is that tag's
	// manifest on quay.io. Overridable through the layer's
	// configmapRegistryImage setting (AC-4.3), e.g. for a mirror.
	//
	// Upstream publishes no arm64 build of this image at any tag, so
	// ConfigMap-based CatalogSources cannot run on arm64 clusters
	// regardless of this pin; image-backed (grpc) CatalogSources —
	// which everything kind-ocp targets uses — never pull it.
	DefaultConfigmapRegistryImage = "quay.io/operator-framework/configmap-operator-registry:v1.73.0@sha256:c59709eaadcb33651b7753399532cc636788394023383b54dffc166631627f54"

	crdsManifest = "crds.yaml"
	olmManifest  = "olm.yaml"
)

// defaultCatalogSource is the release's public community catalog, the
// CatalogSource Apply drops (see the package doc).
const defaultCatalogSource = "operatorhubio-catalog"

// releaseBaseURL is the GitHub release-asset prefix every OLM manifest
// is fetched from.
const releaseBaseURL = "https://github.com/operator-framework/operator-lifecycle-manager/releases/download"

// The release's operator Deployments, in the olm namespace: the rollouts
// Apply waits for and Verify asserts. Facts of the release, not
// configuration.
const (
	operatorNamespace     = "olm"
	olmOperatorDeployment = "olm-operator"
	catalogDeployment     = "catalog-operator"
)

// operatorsAPI is the API group/version installing OLM provides; Verify
// checks the server serves it.
const operatorsAPI = "operators.coreos.com/v1alpha1"

// fetchTimeout bounds one manifest download.
const fetchTimeout = 2 * time.Minute

// Wait budgets. Fixed constants, not settings: AC-3.1 requires the
// waits to be bounded, and no documented use case overrides a budget.
const (
	// crdEstablishedTimeout bounds the wait for the release's CRDs to
	// reach Established after they are applied.
	crdEstablishedTimeout = 2 * time.Minute

	// rolloutTimeout bounds each operator Deployment's rollout wait in
	// Apply and, with the same budget, each of Verify's reads. The
	// PoC's TIMEOUT_ROLLOUT (600 s) covered image pulls on CI runners;
	// keep that headroom.
	rolloutTimeout = 10 * time.Minute
)

// versionPattern matches an OLM release tag: v-prefixed semver with an
// optional pre-release suffix. Anything else — a floating "latest", an
// unprefixed "0.46.0", a path-injecting "../x" — is rejected when the
// URL is built, instead of surfacing as a 404 (or a URL pointing
// somewhere else) at fetch time.
var versionPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// settings is the olm layer's resolved configuration: the two pins
// AC-4.3 makes overridable. DecodeSettings overlays the config file
// onto defaultSettings, so only explicitly set keys override the
// defaults.
type settings struct {
	Version                string `yaml:"version"`
	ConfigmapRegistryImage string `yaml:"configmapRegistryImage"`
}

func defaultSettings() settings {
	return settings{
		Version:                DefaultVersion,
		ConfigmapRegistryImage: DefaultConfigmapRegistryImage,
	}
}

func decodeSettings(env layers.Env) (settings, error) {
	s := defaultSettings()
	if err := env.DecodeSettings(Name, &s); err != nil {
		return settings{}, err
	}
	if _, err := releaseManifestURL(s.Version, crdsManifest); err != nil {
		return settings{}, err
	}
	if s.ConfigmapRegistryImage == "" {
		return settings{}, errors.New("configmapRegistryImage must be a non-empty image reference")
	}
	return s, nil
}

// releaseManifestURL builds the download URL for one file of an OLM
// release and validates the version (see versionPattern).
func releaseManifestURL(version, file string) (string, error) {
	if !versionPattern.MatchString(version) {
		return "", fmt.Errorf("invalid OLM version %q: must be a release tag like %q", version, DefaultVersion)
	}
	return fmt.Sprintf("%s/%s/%s", releaseBaseURL, version, file), nil
}

// Layer installs upstream OLM. It is stateless; construct once.
type Layer struct{}

// Name returns "olm".
func (Layer) Name() string { return Name }

// Apply converges the cluster to the pinned OLM release: fetch both
// release manifests, close the release's floating image references
// (pinReleaseImages), server-side apply the CRDs, wait for them to be
// Established, apply the remaining objects, then wait for both operator
// Deployments' rollouts to complete. Re-running is a converge:
// server-side apply of the same content under the same field manager is
// a no-op, so a retry after a partial failure completes what is
// missing. This drops the PoC's "already installed, skip" shortcut —
// the shortcut could not recover a partial install, and the converge
// does not need it.
func (l Layer) Apply(ctx context.Context, env layers.Env) error {
	s, err := decodeSettings(env)
	if err != nil {
		return err
	}
	env.Log.Info("installing OLM", "version", s.Version)

	client, err := kube.NewClient(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	objs, err := fetchRelease(ctx, http.DefaultTransport, s.Version)
	if err != nil {
		return err
	}
	objs, err = pinReleaseImages(objs, s.ConfigmapRegistryImage)
	if err != nil {
		return fmt.Errorf("closing the OLM release's floating image references: %w", err)
	}
	crds, rest := partitionCRDs(objs)
	manager := kube.LayerFieldManager(Name)

	if err := client.Apply(ctx, manager, crds); err != nil {
		return fmt.Errorf("applying OLM CRDs: %w", err)
	}
	// No object of the CRDs' kinds may be applied before this passes:
	// the apiserver rejects such objects until the CRD is established.
	if err := kube.WaitForCRDsEstablished(ctx, client.Dynamic, crdEstablishedTimeout, crdNames(crds)); err != nil {
		return fmt.Errorf("waiting for OLM CRDs: %w", err)
	}
	if err := client.Apply(ctx, manager, rest); err != nil {
		return fmt.Errorf("applying OLM resources: %w", err)
	}
	for _, deployment := range []string{olmOperatorDeployment, catalogDeployment} {
		if err := kube.WaitForDeploymentRolledOut(ctx, client.Clientset, operatorNamespace, deployment, rolloutTimeout); err != nil {
			return err
		}
	}
	return nil
}

// Verify checks the layer's postconditions and mutates nothing: both
// operator Deployments Available, and the operators.coreos.com/v1alpha1
// API served. Each read is bounded by rolloutTimeout.
func (l Layer) Verify(ctx context.Context, env layers.Env) error {
	return l.verify(ctx, env, rolloutTimeout)
}

// verify is Verify with an explicit read budget. Verify passes the
// production bound (rolloutTimeout); the bounded-read regression test
// drives this helper with a short budget, which is why the budget is a
// parameter and not a setting.
func (l Layer) verify(ctx context.Context, env layers.Env, readTimeout time.Duration) error {
	// Validate the same settings Apply does, so a configuration
	// mistake fails Verify with the mistake, not a cluster error.
	if _, err := decodeSettings(env); err != nil {
		return err
	}
	client, err := kube.NewClient(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	for _, deployment := range []string{olmOperatorDeployment, catalogDeployment} {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		d, err := client.Clientset.AppsV1().Deployments(operatorNamespace).Get(readCtx, deployment, metav1.GetOptions{})
		cancel()
		if err != nil {
			return fmt.Errorf("reading deployment %s/%s: %w", operatorNamespace, deployment, err)
		}
		if available, why := kube.DeploymentAvailable(d); !available {
			return fmt.Errorf("deployment %s/%s is not Available: %s", operatorNamespace, deployment, why)
		}
	}
	// The discovery read gets the same bound as the deployment reads
	// above: an apiserver that wedges discovery must not outlive the
	// timeout (the caller's context can be unbounded — the CLI passes
	// context.Background).
	readCtx, cancel := context.WithTimeout(ctx, readTimeout)
	served, err := kube.GroupVersionServed(readCtx, client.Discovery, operatorsAPI)
	cancel()
	if err != nil {
		return err
	}
	if !served {
		return fmt.Errorf("API %s is not served (the OLM CRDs are missing or not established)", operatorsAPI)
	}
	return nil
}

// fetchRelease downloads both manifests of the pinned release,
// verifies them against the reviewed digests before decoding (see
// reviewedReleaseDigests), and decodes them into objects, CRD manifest
// first. The transport is a parameter so the fetch, its verification,
// and the decode are unit-testable against a replaying transport; the
// fetcher (kube.FetchManifest) owns the HTTP client, its https-only
// redirect policy, and the download bound (fetchTimeout).
func fetchRelease(ctx context.Context, transport http.RoundTripper, version string) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	for _, file := range []string{crdsManifest, olmManifest} {
		url, err := releaseManifestURL(version, file)
		if err != nil {
			return nil, err
		}
		body, err := kube.FetchManifest(ctx, transport, fetchTimeout, url)
		if err != nil {
			return nil, err
		}
		if err := verifyReviewedDigest(reviewedReleaseDigests, version, file, body); err != nil {
			return nil, err
		}
		fileObjs, err := kube.DecodeManifests(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("manifest %s: %w", url, err)
		}
		objs = append(objs, fileObjs...)
	}
	return objs, nil
}

// reviewedReleaseDigests maps the default release's manifest files to
// their reviewed SHA-256 digests. GitHub release assets are mutable —
// a maintainer can replace an asset without touching its tag — so the
// version pin alone does not bind the bytes, and these are the bytes
// that get applied with cluster-admin credentials. Only the default
// release is reviewed; an overridden version is trusted as pinned (see
// DefaultVersion). Bump these together with DefaultVersion. The table
// is fixed package data: nothing writes it, at runtime or in tests.
var reviewedReleaseDigests = map[string]string{
	crdsManifest: "2ecd51a33dfa00a5abcae7bf47bcc4eea6c6d6b4212bb36d6b887aa6ef6d63c5",
	olmManifest:  "3a6cd5caea67bedc11464ad443fa0826d3777d7f1b4f2dfa00766a739f2ab44c",
}

// verifyReviewedDigest refuses manifest bytes that are not the reviewed
// ones. It runs only for reviewed releases — the default; an
// overridden version is trusted as pinned and skips the check. A
// default-release file with no recorded digest fails closed: a
// changed release layout must not silently skip verification. The pin
// table is a parameter, so the comparison is tested with the caller's
// own table instead of swapping the production pins, which stay fixed
// package data.
func verifyReviewedDigest(digests map[string]string, version, file string, body []byte) error {
	if version != DefaultVersion {
		return nil
	}
	want, ok := digests[file]
	if !ok {
		return fmt.Errorf("OLM %s has no reviewed digest for %s; the release layout changed — revisit the olm layer's release pins",
			version, file)
	}
	if got := sha256Hex(body); got != want {
		return fmt.Errorf("OLM %s %s does not match its reviewed digest: got sha256:%s, want sha256:%s — the release's bytes are not what was reviewed",
			version, file, got, want)
	}
	return nil
}

// sha256Hex returns the hex-encoded SHA-256 of b.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// partitionCRDs splits objects into CustomResourceDefinitions and
// everything else, preserving order in both. The CRDs must be applied
// and Established before any object of their kinds is applied: the olm
// manifest contains CatalogSource, OperatorGroup, ClusterServiceVersion,
// and OLMConfig objects alongside the core kinds.
func partitionCRDs(objs []*unstructured.Unstructured) (crds, rest []*unstructured.Unstructured) {
	for _, obj := range objs {
		if obj.GetKind() == "CustomResourceDefinition" {
			crds = append(crds, obj)
		} else {
			rest = append(rest, obj)
		}
	}
	return crds, rest
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

// configmapServerImageArg is the catalog-operator flag whose value the
// OLM release leaves floating at :latest.
const configmapServerImageArg = "--configmapServerImage="

// pinReleaseImages closes the two floating image references the OLM
// release manifest ships (see the package doc), touching nothing else:
// it drops the operatorhubio-catalog CatalogSource and replaces the
// catalog-operator's --configmapServerImage argument with
// configmapRegistryImage, preserving argument order and neighbors.
//
// Dropping the catalog is idempotent: a release without it needs no
// change. The argument rewrite fails closed — a release whose
// catalog-operator Deployment is missing, or carries no
// --configmapServerImage argument, is an error rather than a silent
// pass-through, because the layer would otherwise configure an image
// reference it never pinned. Only an explicit version bump reaches a
// changed release layout, and that bump should hit this error, not a
// floating default.
func pinReleaseImages(objs []*unstructured.Unstructured, configmapRegistryImage string) ([]*unstructured.Unstructured, error) {
	pinned := make([]*unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		if obj.GetKind() == "CatalogSource" && obj.GetName() == defaultCatalogSource {
			continue
		}
		pinned = append(pinned, obj)
	}
	found := false
	for _, obj := range pinned {
		if obj.GetKind() != "Deployment" || obj.GetName() != catalogDeployment || obj.GetNamespace() != operatorNamespace {
			continue
		}
		found = true
		// NestedFieldNoCopy, not the Nested* accessors: those deep-copy,
		// and the rewritten arguments must land in the object that gets
		// applied.
		rawContainers, _, err := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return nil, fmt.Errorf("reading the %s Deployment's containers: %w", catalogDeployment, err)
		}
		containers, ok := rawContainers.([]any)
		if !ok {
			return nil, fmt.Errorf("reading the %s Deployment's containers: not a list", catalogDeployment)
		}
		replaced := false
		for _, c := range containers {
			container, ok := c.(map[string]any)
			if !ok {
				continue
			}
			rawArgs, _, err := unstructured.NestedFieldNoCopy(container, "args")
			if err != nil {
				return nil, fmt.Errorf("reading the %s container's args: %w", catalogDeployment, err)
			}
			args, ok := rawArgs.([]any)
			if !ok {
				continue
			}
			for i, a := range args {
				if arg, ok := a.(string); ok && strings.HasPrefix(arg, configmapServerImageArg) {
					args[i] = configmapServerImageArg + configmapRegistryImage
					replaced = true
				}
			}
		}
		if !replaced {
			return nil, fmt.Errorf("the %s/%s Deployment carries no %s argument; the OLM release layout changed — revisit the olm layer's image pins",
				operatorNamespace, catalogDeployment, configmapServerImageArg)
		}
	}
	if !found {
		return nil, fmt.Errorf("the release manifest contains no %s/%s Deployment; the OLM release layout changed — revisit the olm layer's image pins",
			operatorNamespace, catalogDeployment)
	}
	return pinned, nil
}
