# kind-ocp — maintenance and CI

How to bump the pinned external inputs, and the CI that guards them.
The pin table itself lives in the root README ("Defaults and pins").

## Bump and maintenance policy

- **Node image / OLM release**: change the pin (config or default), run
  `make test` and the integration suite. A node-image bump means new
  clusters — kind-ocp does not upgrade nodes in place; `delete` and
  re-create.
- **openshift/api**: bump the go.mod requirement, then `make generate`
  (regenerates the build-time pin file: commit digests and the expected
  manifest digests). The pin file and go.mod are kept in sync by test.
- **platform-objects**: `make regenerate` refetches the pinned upstream
  commits, splices between the managed markers, and records fixtures;
  review the diff like any vendored content.
- **CO catalog**: `make pin-compliance-catalog` re-derives the committed
  manifests from the pinned bundle/image digests (through a pinned crane,
  digest-verifying every read); a bump is a new bundle digest plus a
  regenerated catalog, run through the integration suite. Reassess the
  recorded GO-2026-6865 advisory at the crane pin on every crane or bundle
  bump.
- **StackRox charts (example)**: record the new version and digest from
  `https://charts.stackrox.io/index.yaml` in `examples/stackrox/charts.go`
  and run the example once.

## Lint toolchain

`make lint` needs a golangci-lint binary built with a Go at least as
new as the module's `go` directive (currently 1.27.4). A binary built
with an older Go refuses to load the module — the v0.1.0 clean-machine
install test hit exactly that with a golangci-lint built with go1.25.
Check yours with `golangci-lint --version`, which prints the Go it was
built with; when it is too old, install one built with go1.27.4 or
newer — building the same version from source with the local toolchain
(`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0`)
is the reliable way, since a prebuilt release carries whatever Go its
maintainers built it with. CI installs the pinned version through the
golangci-lint action (see the workflow).

## CI

Lint + unit on every PR; the Docker-gated integration suite (multi-cluster
create/verify plus the Compliance Operator proof) on PRs carrying the
`integration` label, on manual dispatch, and nightly. Nightly CI is the
determinism signal for all pins: it re-creates clusters from the pins on
a cold runner. Actions are SHA-pinned, permissions are `contents: read`,
no secrets. The workflow lives at `.github/workflows/ci.yaml`.
