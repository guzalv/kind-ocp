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
- **Consumer actions**: the `kind_ocp_version` input default
  (kept identical in `.github/actions/create/action.yml` and
  `.github/actions/delete/action.yml`), the `go_version` default (must satisfy
  the pinned release's go.mod minimum), and the SHA-pinned
  `actions/setup-go` / `actions/checkout` commits there — the same pin
  discipline as ci.yaml (commit SHA plus version comment). Both
  actions source their shared helpers from `.github/actions/lib.sh` (each
  step's ACTION_LIB env mapping), so input validation and the tool
  log wrappers live there, not per step. Inputs
  reach the `run:` scripts only through step `env:` mappings
  (`INPUT_<NAME>: ${{ inputs.<name> }}`) — composite run steps get no
  automatic INPUT_* variables — and outputs are single-line
  `key=value` directives with CR/LF inputs refused and the version
  inputs held to the single-token charset; keep those invariants
  when editing. A bump is: edit the files, run
  `tools/local-gate.sh all` (needs Docker, and network: its `yaml`
  scenario resolves every `uses:` pin against api.github.com; its
  `smoke` scenario fails if any step's env wiring drifts from the
  gate's recorded expectation — a removed mapping or a changed
  scope reference fails with empty defaults), and dispatch the
  self-test workflow remotely and require green.

## Lint toolchain

`make lint` needs a golangci-lint binary built with a Go at least as
new as the module's language version — the minor version of the `go`
directive, currently 1.27. The patch release is not the boundary: a
binary built with go1.27.2 lints this go1.27.4 module fine (the
language is the same, and the go command switches to the module's
toolchain via GOTOOLCHAIN). A binary built with an older minor refuses
to load the module — the v0.1.0 clean-machine install test hit exactly
that with a golangci-lint built with go1.25. Check yours with
`golangci-lint --version`, which prints the Go it was built with; when
it is too old, build the pinned version from source with a current
local toolchain
(`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0`)
— the reliable way to control the build Go, since a prebuilt release
carries whatever Go its maintainers built it with. CI installs the
pinned version through the golangci-lint action (see the workflow).

## CI

Lint + unit on every PR; the Docker-gated integration suite (multi-cluster
create/verify plus the Compliance Operator proof) on PRs carrying the
`integration` label, on manual dispatch, and nightly. Nightly CI is the
determinism signal for all pins: it re-creates clusters from the pins on
a cold runner. Actions are SHA-pinned, permissions are `contents: read`,
no secrets. The workflow lives at `.github/workflows/ci.yaml`.
