// Package assets embeds the manifest assets the builtin layers apply.
// Each layer's manifests live under a directory named for the layer and
// are exposed as package-level strings; the layer that owns an asset
// decodes and validates it (its package doc states the contract), so
// this package adds no logic of its own.
//
// Provenance and pin policy live with each asset: everything here is
// either hand-authored placeholder content ported from the PoC (stackrox
// PR #23841) or, from increment 5 on, upstream-derived content guarded by
// regeneration markers and SHA pins (tools/regenerate).
package assets

import _ "embed"

// ConfigOpenshiftCRDs is the config-openshift layer's placeholder CRD
// manifest: 13 schema-less config.openshift.io CustomResourceDefinitions
// with the status subresource enabled.
//
//go:embed config-openshift/crds.yaml
var ConfigOpenshiftCRDs string

// ConfigOpenshiftObjects is the config-openshift layer's singleton
// manifest: APIServer/cluster, Infrastructure/cluster, and
// ClusterVersion/version, spec-only.
//
//go:embed config-openshift/objects.yaml
var ConfigOpenshiftObjects string

// PlatformObjectsObjects is the platform-objects layer's object
// manifest: the namespaces, config.openshift.io and operator.openshift.io
// singletons, the misc objects some ocp4-cis rules probe, and the four
// control-plane ConfigMaps whose contents drive most ocp4-cis PASS/FAIL
// results. Two fields inside it — the openshift-kube-apiserver/config
// apiServerArguments and the openshift-kube-controller-manager/config
// extendedArguments — are upstream content between BEGIN/END UPSTREAM
// markers; tools/regenerate rewrites exactly those, from pinned upstream
// commits.
//
//go:embed platform-objects/objects.yaml
var PlatformObjectsObjects string

// PlatformObjectsSCCs is the platform-objects layer's
// SecurityContextConstraints manifest: the real restricted-v2 and
// privileged SCCs, verbatim from the pinned upstream commit recorded in
// its header. Entirely upstream content: tools/regenerate rewrites the
// whole file.
//
//go:embed platform-objects/sccs.yaml
var PlatformObjectsSCCs string

// SCCAdmissionWebhookMain is the scc-admission layer's webhook server,
// one of the two source files of the image the layer builds at create
// time (see internal/layers/sccadmission). The files are a package of
// this repo in place — tested by `go test ./...` — and a standalone
// stdlib-only module when the layer copies them into its docker build
// context, so the built image carries exactly the reviewed source.
//
//go:embed scc-admission/main.go
var SCCAdmissionWebhookMain string

// SCCAdmissionWebhookPatch is the webhook's admission patch logic, the
// second source file of the layer's image. See
// SCCAdmissionWebhookMain for the dual in-repo/build-context role.
//
//go:embed scc-admission/patch.go
var SCCAdmissionWebhookPatch string
