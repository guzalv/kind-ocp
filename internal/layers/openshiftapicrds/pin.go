package openshiftapicrds

// The openshift/api pin. go.mod is the single source of truth for which
// openshift/api content this layer fetches (docs/PLAN.md §2, locked
// decision; no committed CRD copies). The version go.mod requires is
// embedded at build time as ModuleVersion, ModuleCommit is the full
// commit SHA that version resolves to, and ModuleDigests is the
// expected SHA-256 of every one of the 18 manifests as it exists in
// that module — the digests every cache read and download is validated
// against (see cache.go). All three live in pin_gen.go, which
// tools/pin-openshift-api regenerates:
//
//	go get github.com/openshift/api@<version>
//	go generate ./internal/layers/openshiftapicrds
//
// (or `make generate`). The generated file is committed, so builds and
// tests need no network. TestModulePin pins its freshness against the
// module graph, and TestCRDSourcesAgainstModule pins the layer's path
// map, the digests, and the real-CRD marker against the module in the
// local module cache.
//
// The blank import below is what keeps the openshift/api requirement in
// go.mod: nothing in this package compiles against the module's Go
// types — the CRDs are fetched as manifest bytes, not linked in — and
// `go mod tidy` drops requirements nothing imports. Importing one group
// package (config/v1, the largest group this layer fetches) anchors the
// requirement and puts the version in the build info TestModulePin
// reads, while the linker drops the types from the binary: the import
// pins a version, it does not add code.

//go:generate go run github.com/guzalv/kind-ocp/tools/pin-openshift-api

import _ "github.com/openshift/api/config/v1"
