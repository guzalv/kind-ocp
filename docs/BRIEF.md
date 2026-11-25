# kind-ocp — product brief

Status: implemented. This brief is the authoritative statement of what
kind-ocp is for: the acceptance criteria in §5 and the non-goals in §6
govern every change. The architecture and increment records live in
`docs/PLAN.md`.

User decisions locked at scoping:

- Implementation: Go CLI plus an importable library.
- Fitness proof: the Compliance Operator install via OLM is an automated CI test.
  A full StackRox Central + Sensor deployment ships as a runnable example, not
  as a CI gate.

Origin, in one paragraph: kind-ocp is the productized successor of the
multi-cluster kind proof in stackrox PR #23841 — an 804-line bash harness
coupled to StackRox deployment that proved kind clusters plus a set of
OpenShift layers run a real Compliance Operator at full fidelity under one
StackRox Central. This repo keeps everything that proof learned (§7 records
the porting map, the exact pins, the gotchas, and the bugs not to
reproduce) and drops everything that was stackrox CI plumbing.

## 1. Problem

StackRox has no automated multi-cluster e2e coverage. Prow gives every job
exactly one OpenShift cluster. PR stackrox/stackrox#23841 proved the missing
capability in bash inside the stackrox monorepo: kind clusters with an added
set of OpenShift layers run a real Compliance Operator at full fidelity, and a
StackRox Central manages them. That proof is an 804-line bash script coupled to
StackRox deployment. It is not reusable outside the stackrox CI job.

## 2. Users

1. Primary: stackrox CI engineers who wire e2e jobs. They need N identical,
   cheap, OCP-like clusters on a GitHub Actions runner.
2. Secondary: engineers who want local OCP-like clusters for StackRox or
   Compliance Operator work without an OCP install.

## 3. Product statement

`kind-ocp` is a Go CLI and library. It creates N (default 2) kind clusters and
applies a configurable set of OpenShift layers to each. After `create`
finishes, StackRox Central/Sensor and the Compliance Operator can be deployed
on every cluster and work properly. The repo creates clusters. It deploys
nothing.

## 4. Terms

- Layer: one discrete OpenShift-ness concern behind one Go interface
  (for example `olm`, `scc-admission`).
- Profile: a named set of layers. Profile `full` (the default) enables all six
  layers in AC-3. Profile `minimal` enables none and yields a plain kind
  cluster.
- Prefix: the cluster-name prefix. Default `kind-ocp`. Clusters are named
  `<prefix>-<i>`, so the default names are `kind-ocp-1` and `kind-ocp-2`.
  kubectl contexts follow kind's rule `kind-<name>`.

## 5. Acceptance criteria

### AC-1 Cluster lifecycle

1. `kind-ocp create` with no options creates exactly 2 clusters named
   `kind-ocp-1` and `kind-ocp-2` with profile `full`.
2. `--count N` (N >= 1) creates N clusters `<prefix>-1..N`. Clusters 2..N are
   created concurrently. A run with N = 3 must not corrupt any per-cluster
   state. The integration test covers N = 3 at least once.
3. Every cluster writes its kubeconfig under a deterministic, per-prefix state
   root outside the repo. `kind-ocp kubeconfig <name>` prints the path.
4. All clusters attach to one shared Docker network. A pod in cluster A must
   reach a NodePort service on cluster B by node IP. The integration test
   asserts this cross-cluster reachability, because the Central-to-Sensor
   pattern depends on it.
5. `kind-ocp delete` removes all clusters with the prefix and all state under
   the prefix's state root. It is idempotent. A full `create` after `delete`
   succeeds with no manual cleanup (see 7.4, stale state).
6. `kind-ocp list` prints each cluster with name, context, state, and applied
   layers.
7. On failure, `create` exits non-zero, names the failing cluster and step, and
   prints the exact `delete` command. Clusters are left in place for debugging.
8. Supported platforms: linux/amd64 on GitHub Actions runners and macOS with
   Docker. `create` works on both.

### AC-2 Layer system (pluggability)

1. Every layer implements one common Go interface with at least Apply and
   Verify operations. dev finalizes the exact interface in docs/PLAN.md.
2. Layers are selected by profile in a config file. Flags override the file.
   The file overrides built-in defaults. The precedence is documented and
   unit-tested.
3. The config format supports per-cluster layer overrides. Cluster 1 can run
   profile `minimal` while clusters 2..N run `full` (the co-located Central
   pattern from the PoC). Config resolution for this is unit-tested.
4. Adding or removing a layer must not require changes to the provisioning
   loop. Review enforces this.

### AC-3 The six default layers

Each layer lists its behavior and its verify check. All verify checks run in
`kind-ocp verify`.

1. `olm` — installs upstream OLM (default v0.46.0, pinned) from the release
   URLs. Uses server-side apply for the CRDs, because the ClusterServiceVersion
   CRD exceeds the client-side annotation limit. Waits for CRDs to reach
   Established. Waits for the `olm-operator` and `catalog-operator` rollouts.
   Verify check: both deployments Available and `operators.coreos.com/v1alpha1`
   served.
2. `config-openshift` — applies the 13 placeholder `config.openshift.io` CRDs
   and the three singletons (APIServer/cluster, Infrastructure/cluster,
   ClusterVersion/version). Writes the Infrastructure status through the status
   subresource. Verify check: `Infrastructure/cluster` is readable and carries
   `status.platformStatus`. This is the exact input StackRox Sensor uses to
   detect OpenShift.
3. `node-labels` — labels control-plane nodes with
   `node-role.kubernetes.io/master=""`. OLM-created operator Deployments select
   that label. Verify check: every control-plane node carries the label.
4. `openshift-api-crds` — stages the 18 real OpenShift CRDs from the pinned
   `github.com/openshift/api` Go module and applies them server-side. The
   go.mod pin is the single source of truth. No committed CRD copies. Verify
   check: all 18 CRDs Established **and** carrying the real schema — a CRD
   replaced by its config-openshift placeholder FAILs the check.
5. `platform-objects` — applies the ported `objects.yaml` (6 namespaces, ~20
   singleton objects, the four control-plane ConfigMaps that drive ocp4-cis
   PASS/FAIL) and `sccs.yaml` (restricted-v2 and privileged). SCCs are applied
   with create semantics, not client-side apply (see 7.3). Status writes go
   through the status subresource. Includes the ported regeneration tool with
   SHA pins (see 7.2). Verify check: both SCCs exist, the four ConfigMaps
   exist, and the singleton CRs exist with populated status.
6. `scc-admission` — deploys the SCC-UID mutating webhook that emulates
   OpenShift SCC UID injection. Generalizes the PoC: the watched namespaces and
   the injected UID are config values. The webhook server must handle
   concurrent admission requests (see 7.4, single-threaded flake). The webhook
   container image is digest-pinned. The failurePolicy choice (Fail versus
   Ignore) is documented with its trade-off. Verify check: a pod with
   `runAsNonRoot: true` and no `runAsUser` in a watched namespace becomes
   Running with an injected UID.

### AC-4 Determinism and pinning

1. Every external input is pinned by digest, version, or SHA: the kind node
   image, the OLM release, the openshift/api module, the upstream operator SHAs
   for regenerated content, the webhook image, and (verification path only) the
   CO catalog — the committed bundle manifests with digest-pinned images, plus
   the digest-pinned image that serves them.
2. Defaults live in one place in the code and are documented in the README. No
   default references a floating tag or a branch.
3. Every runtime pin is overridable through config. One exception, ratified at
   scoping: the openshift/api commit pin is a build-time pin owned by go.mod
   (AC-3.4 single source of truth). Overriding it means bumping go.mod and
   regenerating the pin file (`make generate`) — never a config knob.

### AC-5 Verify and the fitness proof

1. `kind-ocp verify` runs every enabled layer's verify check plus global
   checks: nodes Ready and cross-cluster NodePort reachability. Any failure
   exits non-zero and prints per-check pass, fail, or skip with detail.
2. CO fitness (integration test, runs in CI and locally). After `create` with
   profile `full`, for each cluster:
   - Create the `compliance-operator-serving-cert` TLS secret in
     `openshift-compliance` before the operator starts. The operator exits
     without it.
   - Install the Compliance Operator via OLM: CatalogSource in namespace `olm`
     addressing the proof's own in-cluster catalog server (the committed,
     digest-pinned bundle manifests served on the pinned catalog image's `opm`
     — nothing upstream floats and no bundle image is pulled), OperatorGroup
     and Subscription in `openshift-compliance` on channel `alpha`, CSV
     resolution with the channel-head fallback, wait for CSV phase `Succeeded`,
     wait for `deploy/compliance-operator`.
   - Assert ProfileBundles `ocp4` and `rhcos4` reach `VALID` in
     `.status.dataStreamStatus`.
   - Create an `ocp4-cis` ScanSettingBinding and assert the scan yields at
     least one PASS and one FAIL ComplianceCheckResult. This proves the
     platform layers end to end.

   This CO install code lives in the verification or examples module. It must
   not be reachable from the default `create` path.
3. StackRox example (not a CI gate). An `examples/` recipe deploys Central on
   one cluster and Sensor on the others over NodePort on the shared network,
   with released public images and the collector disabled. It runs successfully
   at least once during development. The run log is kept as evidence in the
   workflow run output.

### AC-6 Engineering quality

1. `make build`, `make test`, and `make lint` pass. Lint is golangci-lint with
   a committed config.
2. Unit tests cover config resolution and precedence, profile and layer
   selection, per-cluster overrides, name generation, the CRD path map, and the
   webhook patch logic. Unit tests need no Docker.
3. Integration tests are build-tagged or env-gated. They skip with a clear
   message when Docker is unavailable.
4. The runtime requires only Docker plus either an embedded kind provisioner or
   the kind CLI. dev picks one and documents it (see risks). The tool must not
   require jq, openssl, curl, or python on the host.
5. Waits are condition-based with timeouts and clear failure messages. No bare
   sleeps as synchronization.
6. Errors carry cluster, layer, and operation context. Logging is structured.
7. CI: GitHub Actions with lint and unit tests on every PR, plus the
   integration test (create, verify, CO proof). Actions are pinned by SHA.
   GITHUB_TOKEN permissions are minimal.
8. README covers purpose, quickstart, prerequisites, architecture, config
   reference, pinning and maintenance policy, and known limitations.

## 6. Non-goals

- No StackRox Central or Sensor deployment as product functionality. Example
  only.
- No Compliance Operator deployment as product functionality. Verification
  only.
- No e2e test suites. Consumers own their tests.
- No real OpenShift parity beyond the six layers. Node scans (`rhcos4-*`) and
  MachineConfig remediation cannot work on kind nodes. No OAuth, console,
  marketplace, or image builds.
- No k3d, vcluster, or minikube support. The design must not prevent adding a
  provisioner, but none is implemented.
- No migration of PR #23841 or of stackrox CI to consume this repo. That is
  follow-up work.
- No cluster upgrades or Kubernetes version management beyond what kind
  provides.
- No Windows support.

## 7. Porting reference from PR #23841

Head commit at time of scoping: `53fcc67f9652f9913f878ce45e7407fef16f6dda`,
branch `gualvare/poc-compliance-kind`. Full diff:
https://patch-diff.githubusercontent.com/raw/stackrox/stackrox/pull/23841.diff

### 7.1 What to port

From `tests/e2e/kind-compliance/`:

- `setup-multicluster-compliance-ci.sh`: only the cluster and layer parts.
  Port `ensure_kind_cluster`, `raise_inotify`, `stage_openshift_crds` with
  `OPENSHIFT_CRD_MAP`, `install_olm`, `install_scc_webhook`,
  `apply_platform_shim`, the config-shim part of `install_co`, and the node
  labeling. Do not port `render_charts`, `deploy_central`,
  `central_internal_endpoint`, `start_central_pf`, `generate_init_bundle`,
  `deploy_secured`, `central_api`, `cluster_id_for`, or the Central-based
  `verify`. Those are StackRox deployment — they became the example.
- `assets/openshift-shim/config-crds.yaml` (13 placeholder CRDs) and
  `config-objects.yaml` (3 singletons).
- `assets/openshift-platform-shim/`: `objects.yaml`, `sccs.yaml`,
  `regenerate.sh`, `README.md`.
- `assets/scc-shim-webhook.py`.

### 7.2 Exact pins to carry over

- kind node image:
  `kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed`
  (the workflow pin. The script default `v1.31.2` floats and must not be used).
- OLM: `v0.46.0`, release URLs `crds.yaml` and `olm.yaml`.
- openshift/api: pinned through this repo's own go.mod.
- Regeneration SHAs: `KAS_OPERATOR_SHA=5105250647f33a232b1bf2982372f7cc66580deb`
  (cluster-kube-apiserver-operator) and
  `KCM_OPERATOR_SHA=cd45079d8bb7a391ba0eb3282d9ef75232fe425c`
  (cluster-kube-controller-manager-operator).
- CO catalog (verification path): the PoC's pinned digest
  `ghcr.io/complianceascode/compliance-operator-catalog@sha256:b5ad7b09f3fa12d99821b7768d6af54eba02e9328feb71c5aee7624401d5a3e8`
  carries over with a narrowed role — the image whose `opm` binary serves the
  proof's catalog — but it is no longer the determinism knob: that catalog
  floats (its bundle entry rides `:latest`, and the bundle's CSV pins none of
  its images). The proof instead installs from committed, digest-pinned bundle
  manifests (`internal/verify/compliance/catalog`, regenerated by
  `make pin-compliance-catalog` from the pins recorded in that tool), served
  inline so no bundle image is pulled at all. Every image the install pulls —
  operator, OpenSCAP scanner, content — is digest-pinned in the committed CSV.
  The Subscription env overrides do not work upstream, so do not try to wire
  them.
- New pin that closes a PoC gap: the webhook image `python:3.12-slim` floats in
  the PoC. kind-ocp replaces it with a Go webhook built from a digest-pinned
  builder image.

### 7.3 Gotchas to preserve

- OLM CRDs need server-side apply.
- Wait for CRDs to reach Established before applying objects of those kinds.
- The CatalogSource must live in namespace `olm`. OLM resolves Subscriptions
  only against its global catalog namespace and the Subscription's own
  namespace.
- SCCs need create (or server-side apply), never client-side apply. The CRD
  requires null fields to be present, and client-side apply strips them.
  Re-runs use delete and recreate, not replace.
- Status fields must be written through the status subresource. The API server
  discards `.status` on normal writes.
- Control-plane nodes need the `node-role.kubernetes.io/master` label so
  OLM-created operator Deployments schedule.
- The `compliance-operator-serving-cert` secret must exist before the CO
  operator starts (verification path only).
- CSV resolution needs the fallback. `status.currentCSV` may name a CSV the
  catalog never creates, so fall back to the compliance-operator CSV object OLM
  actually created.
- Multi-cluster hosts may need `fs.inotify.max_user_instances` raised. Do this
  best-effort with a warning. Linux only.
- The CO catalog digest bundles both the operator and the content image. Bump
  the digest to change either one.

### 7.4 PoC bugs to not reproduce

These came out of the PR review and the PoC iteration log:

- Shared fixed-path temp files across parallel cluster installs (certificates,
  keys, CSRs, rendered YAML). Use per-cluster paths.
- One deadline reused across two sequential waits (CSV resolve, then CSV
  Succeeded). Reset the deadline per wait.
- Stale state survives teardown, so a later create reuses artifacts from a
  deleted cluster. `delete` must clear all state under the prefix.
- A single-threaded webhook server missed its 10-second admission timeout under
  host load. The replacement must handle concurrent requests.
- A Makefile recipe masked test exit status through `tee` without `pipefail`.
  Keep exit-code fidelity in any wrapper scripts.

## 8. Security review focus

- The SCC-UID webhook is an admission controller. Review its TLS material
  generation and storage, its namespaceSelector scope, its failurePolicy, and
  the container image it runs.
- Kubeconfigs in the state dir are cluster-admin credentials. Set restrictive
  file permissions on the state dir.
- The regeneration tool fetches upstream content over HTTPS and writes into
  the repo. It must validate content shape before splicing, as the ported tool
  already does with markers and key checks.
- No secrets in logs. The StackRox example handles an admin password.
- CI uses least-privilege tokens and SHA-pinned actions.

## 9. Risks

- Upstream drift in any pin can break `create` or the CO proof. Mitigation:
  pins, the regeneration tool, and a documented bump procedure.
- CI cost: the CO proof pulls the operator, content, and openscap images per
  cluster. Expect roughly 10 to 20 minutes per integration run. Keep it to one
  job.
- kind as a library avoids a binary dependency but uses a less stable API.
  kind as a CLI is the conservative choice. Either is acceptable. Isolate the
  choice behind the provisioner interface.
- macOS local quirks (inotify, sysctl) degrade to warnings.

## 10. Open questions

1. License: Apache-2.0 assumed, matching org standard. Confirm before
   publishing.
2. Module path and final repo name. Confirm before publishing.
3. Integration CI cadence: every PR or nightly. dev decides and records the
   cost in docs/PLAN.md.
4. Binary releases via goreleaser: deferred. `go install` is enough initially.

## 11. Suggested increment order (advisory)

dev owns docs/PLAN.md. This order minimizes rework and keeps every increment
independently testable:

1. Scaffold: Go module, CLI skeleton, config system, kind provisioning for one
   cluster with profile `minimal`, state and kubeconfig handling, `create`,
   `delete`, `list`.
2. Layer framework and the `olm` layer.
3. `config-openshift` and `node-labels` layers.
4. `openshift-api-crds` layer sourced from go.mod.
5. `platform-objects` layer plus the regeneration tool port.
6. `scc-admission` layer, hardened, with its verify check.
7. Multi-cluster: N clusters, concurrency, shared network, cross-cluster
   reachability, complete `verify`.
8. CO fitness integration test and CI workflows.
9. StackRox example, docs polish, release readiness.
