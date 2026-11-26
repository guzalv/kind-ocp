# kind-ocp — N OCP-like kind clusters

`kind-ocp` is a Go CLI and importable library that creates N (default 2)
kind clusters and applies a configurable set of OpenShift layers to each.
The layers provide the OpenShift API surface and admission behavior that
StackRox Central/Sensor and the Compliance Operator depend on. The repo
creates clusters; it deploys nothing: the [Compliance Operator fitness
proof](#the-compliance-operator-proof) (an integration test) and the
[StackRox example](examples/stackrox) live outside the create path by
design — they prove the clusters are fit, they are not product
functionality. It exists because multi-cluster OpenShift e2e work has
nowhere cheap to run: kind-ocp turns one Docker daemon into N OCP-like
clusters in minutes.

## What is proven

Two proofs exist, and each covers a different slice of "deploy and
work properly"; the round trip between them is proven by neither:

- The **Compliance Operator proof** (integration test) installs a real
  Compliance Operator through OLM on every cluster and drives
  operator-driven `ocp4-cis` scans to real PASS and FAIL results.
- The **StackRox example** deploys Central on one cluster and Sensor on
  the others, and proves deployment, cross-cluster NodePort
  reachability, and Sensors registered and reporting HEALTHY in Central.
  The example runs StackRox itself in vanilla-Kubernetes mode
  (`env.openshift: false` for both charts): StackRox's OpenShift
  deployment mode — the SCC and Route paths — is not exercised and not
  supported by the example.
- **Not proven by anything**: a Central-driven scan-to-results round
  trip (a user requesting a scan in Central and Compliance results
  flowing back). No test drives a scan from Central. The scans that run
  in the proofs are operator-driven, not Central-driven.

The scc-admission layer is an *emulation*, not an implementation, of
OpenShift SCC admission. It defaults `runAsUser`, `runAsGroup`, and
`fsGroup` on pod creates in its watched namespaces, each only where the
pod left it unset; an explicit `runAsUser` always wins. It never denies
a pod, and it does not evaluate `runAsNonRoot` (the pod-side gate that
pairs with it in real OpenShift). Real OpenShift SCC admission can
reject pods; the shim cannot.

## Quickstart

```sh
# Prerequisites: Docker client + daemon, the buildx CLI plugin (see
# Prerequisites below), Go 1.27.4+, kubectl for inspection.
# Build from a checkout and invoke the binary where you built it —
# no PATH setup needed:
go build -o kind-ocp ./cmd/kind-ocp

./kind-ocp create                  # 2 clusters: kind-ocp-1, kind-ocp-2, profile full
./kind-ocp verify                  # per-check PASS/FAIL/SKIP across all clusters
kubectl --kubeconfig "$(./kind-ocp kubeconfig kind-ocp-1)" get nodes
./kind-ocp list                    # name, context, state, layers per cluster
./kind-ocp delete                  # removes the clusters and all state
```

Or use it as a library — the CLI is a thin skin over the same façade:

```go
cfg, _ := config.Resolve(config.Flags{}, config.Env{})
client, _ := kindocp.New(kindocp.Options{Config: cfg, Log: slog.Default()})
records, _ := client.Create(ctx)   // create-or-reuse, layers applied
```

The [StackRox example](examples/stackrox) shows the full pattern: it
resolves a config, creates the clusters through the façade, and deploys
Central on cluster 1 with Sensor on 2..N over a NodePort on the shared
kind network.

## Prerequisites

- **The Docker client and a running Docker daemon.** `create` needs no
  other runtime tool — no kind CLI, no kubectl to create, no jq, no
  openssl, no python.
- **The buildx CLI plugin** for full-profile create: the scc-admission
  layer builds its webhook image with `docker build` (profile `minimal`
  does not build images and needs no buildx).
- **Go 1.27.4 or newer** to build from a checkout (there is no released
  binary yet): the `go.mod` directive pins 1.27.4 as the minimum
  toolchain; an older one is refused.
- **`kubectl`** for the inspection commands the quickstart itself shows
  (kind-ocp does not need or use it).
- **Network access** for the endpoints `create` fetches from:
  `docker.io` (the pinned kind node image and the scc-admission builder
  image), `quay.io` (the OLM operator image), `github.com` (the OLM
  release manifests, on every full-profile create), and
  `raw.githubusercontent.com` (the 18 OpenShift API CRDs, at the pinned
  `openshift/api` commit).
- Platforms: linux/amd64 and macOS (Docker Desktop). On macOS the
  Linux-only host tunings degrade to notes, never failures.
- **macOS pitfalls** (environment constraints, not kind-ocp defects):
  - `/var/run/docker.sock` is a symlink to the Docker Desktop daemon's
    socket, and that symlink's target has been observed to change (it
    once pointed at a rootless podman machine, which kind refuses). If
    the default endpoint misbehaves, pin it explicitly:
    `export DOCKER_HOST=unix://$HOME/.docker/run/docker.sock`.
  - buildx must be discoverable *together with* `DOCKER_HOST`:
    `~/.docker/cli-plugins/docker-buildx` must exist, and when
    `DOCKER_CONFIG` is set it must point at a directory carrying that
    same `cli-plugins/docker-buildx` entry — one without the other fails
    the webhook image build with either "docker buildx not found" or a
    daemon-mismatch error.
  - Source builds on macOS must keep cgo enabled (the default): a
    `CGO_ENABLED=0` darwin build fails closed on the state tree's
    extended-ACL checks.

## Architecture

### Where things live

The public surface is at the repo root: `kindocp/` (the provisioning
façade), `config/`, `layers/` (the layer framework), and `provider/`
(the runtime boundary future runtimes plug into). Implementations are
under `internal/`: kind-as-library (`internal/provider/kind/`, pinned in
go.mod), state, kube helpers, and `internal/layers/<name>/` for the six
builtin layers. Supporting trees: `assets/` (embedded manifests +
webhook source), `tools/` (regeneration and pinning tools),
`test/integration/`, `examples/stackrox` (example only), and
`internal/verify/` (the CO proof, imported only by tests, never by
create).

### The six layers (profile `full`, in apply order)

| # | Layer | What it does | Verify check |
|---|---|---|---|
| 1 | `config-openshift` | 13 placeholder `config.openshift.io` CRDs + the 3 singletons (APIServer, Infrastructure, ClusterVersion), Infrastructure status via the status subresource | `Infrastructure/cluster` readable with `status.platformStatus` |
| 2 | `node-labels` | labels control-plane nodes `node-role.kubernetes.io/master=""` (the exact value OLM's CSV nodeSelectors match) | every control-plane node carries the label |
| 3 | `olm` | upstream OLM (digest-verified release manifests, SSA for the CRDs, rollout waits); drops the floating community catalog and pins the catalog-operator's `:latest` image reference | both deployments Available, `operators.coreos.com/v1alpha1` served |
| 4 | `openshift-api-crds` | the 18 real OpenShift CRDs, fetched at create time over HTTPS from the `openshift/api` commit go.mod pins, cached per state root | all 18 Established **and** carrying the real schema (a placeholder swap FAILs) |
| 5 | `platform-objects` | ~20 singleton objects (namespaces, ConfigMaps, ClusterOperators…) + the `restricted-v2`/`privileged` SCCs, ported from OpenShift and regenerable from pinned upstream commits | both SCCs, the four control-plane ConfigMaps, singletons with populated status |
| 6 | `scc-admission` | a Go mutating webhook that emulates SCC UID injection — see [What is proven](#what-is-proven); its image is built at create time and loaded into each cluster | a probe pod in a watched namespace runs with the injected UID |

`minimal` applies none (a plain kind cluster). Custom profiles select any
subset; per-cluster overrides can run one cluster differently (for example
Central-only). Adding a layer is one package under `internal/layers/` plus
one registration line — the provisioning loop never changes.

### Provisioning, state, and verify

- `create` validates everything up front, then runs cluster 1 alone (it
  establishes the shared kind network and pulls the node image) and
  clusters 2..N concurrently. Failures name the cluster and step and keep
  the clusters in place, with the exact cleanup command printed.
- State lives under `${XDG_STATE_HOME:-~/.local/state}/kind-ocp/<prefix>/`
  — kubeconfigs (cluster-admin credentials) at `0600`, the tree `0700`,
  verified on every access: ownership, privacy, and no symlinks, with
  macOS extended ACLs checked too.
- Every record carries the cluster's instance ID (the kube-system
  namespace UID); a same-named replacement cluster is never trusted.
- `verify` runs global checks (nodes Ready, cross-cluster NodePort
  reachability through the shared network) plus every enabled layer's
  check, one PASS/FAIL/SKIP line per check. With no cluster names it
  also asserts the persisted cluster count: a cluster whose container
  and record both vanished FAILs by name.

### The Compliance Operator proof

`internal/verify/compliance` — run by the tagged integration suite,
never reachable from `create` — installs a real Compliance Operator
through OLM from a catalog committed in this repo (every image
digest-pinned) and waits for real scan results: ProfileBundles `VALID`,
one `ocp4-cis` scan with PASS and FAIL ComplianceCheckResults. It is the
strictest OpenShift-emulation test kind-ocp has.

## When something goes wrong

Four branches, each resting on a documented behavior:

1. **`create` failed mid-run** — a cluster's layers were partially
   applied and its record is `pending` but *bound* to the live cluster
   instance. Re-run the same `create` command: a matching bound record
   is reused, every layer re-applies under the same field manager
   (server-side apply converges), and the record completes. Failed
   clusters are never deleted by kind-ocp — they stay in place for
   exactly this retry. If you would rather start over, the failed run
   printed the exact `kind-ocp delete` command.
2. **A cluster is unbound or was replaced** — create refuses to reuse it
   ("not bound to a cluster instance", "different instance", or an
   image/profile mismatch). Run `kind-ocp delete`, then `create` again.
   kind-ocp never removes or upgrades applied layers, so it refuses to
   adopt a cluster it cannot prove is the one it recorded; delete and
   re-create is the only safe convergence.
3. **`delete` stopped on the first error** — re-run `kind-ocp delete`.
   It is idempotent and retryable: already-deleted clusters and absent
   state are not errors, so a re-run continues where the stopped one
   left off.
4. **An unsafe state root blocks cleanup** — delete refuses to run at
   all when the per-prefix state directory exists but does not verify as
   kind-ocp's (wrong owner or permissions, symlinks, no state marker).
   The refusal names the exact problem and path; resolve what it names
   (fix ownership or modes, remove the unrelated directory, point
   `--state-root` at the right place), then re-run `delete`. Nothing is
   removed until the tree verifies.

A cluster that vanished *entirely* — no container, no state record — is
caught by `verify`'s cluster-count assertion (it FAILs naming the
cluster). Re-run the **original** `create` command — same prefix,
configuration, and `--count`: clusters still present are reused when
they match and the missing one is built fresh. A create with a *smaller*
`--count` is a supported downscope that rewrites the persisted
expectation, but it clears the failure only when the smaller count
excludes every missing cluster (`<prefix>-1` is excluded by none).
Match the count the FAIL line reports.

## Configuration

Config file (YAML, passed with `--config` or `KIND_OCP_CONFIG`), env
(`KIND_OCP_PREFIX`, `KIND_OCP_COUNT`, `KIND_OCP_PROFILE`), and flags
(`--prefix`, `--count`, `--profile`, `--state-root`) resolve in that
order under flag > env > file > default. Only the winning value of each
key is parsed. `--config ''` disables file lookup entirely (it is how the
commands kind-ocp prints stay paste-safe).

```yaml
prefix: kind-ocp            # clusters are <prefix>-<i>; contexts kind-<name>
count: 2
profile: full               # full | minimal | a custom profile below
kindNodeImage: kindest/node:v1.36.4@sha256:099e…   # digest-pinned default
stateRoot: ""               # default: ${XDG_STATE_HOME:-~/.local/state}/kind-ocp
timeouts:
  nodeReady: 5m
clusters:                   # per-cluster overrides, keyed by full cluster name
  kind-ocp-1:
    profile: minimal        # e.g. the co-located Central pattern
profiles:                   # custom profiles
  olm-only:
    layers: [olm, node-labels]
layers:                     # per-layer settings; each layer documents its own
  olm:
    version: v0.46.0
  scc-admission:
    watchedNamespaces: [openshift-compliance]
    runAsUser: 1001
```

Unknown keys, unknown layers in profiles, `count: 0`, and overrides for
ungenerated cluster names are validation errors. `delete`, `list`,
`verify`, and `kubeconfig` never read create-only values, so a create-only
mistake never blocks cleanup.

## Defaults and pins

Every external input is pinned; every pin is overridable through config
(except the one noted). Defaults live in exactly two kinds of places —
`config/defaults.go` for the globals below, and each layer's settings
struct for its own.

| Input | Default (pin) | Override |
|---|---|---|
| kind node image | `kindest/node:v1.36.4@sha256:099e049362a1526b2db71494e1947aae99bd16290d7c895f2b7ea312e3cbfaed` | `kindNodeImage` |
| OLM release | `v0.46.0` (release manifests verified against reviewed SHA-256 digests `2ecd51a3…`, `3a6cd5ca…`) | `layers.olm.version` (an override has no reviewed digest) |
| OLM configmap registry image | `quay.io/operator-framework/configmap-operator-registry:v1.73.0@sha256:c59709ea…` | `layers.olm.configmapRegistryImage` |
| openshift/api CRDs (18) | the exact commit go.mod pins (`v0.0.0-20260317165824-54a3998d81eb`), manifests digest-verified at fetch | bump go.mod + `make generate` — deliberately not a config knob |
| platform-objects content | regenerated from upstream commits `51052506…` (kube-apiserver-operator) and `cd45079d…` (kube-controller-manager-operator) | `make regenerate` |
| scc-admission builder image | `docker.io/library/golang:1.27.4-alpine@sha256:70bd7dde…` (final stage: `FROM scratch`) | `layers.scc-admission.builderImage` (must be digest-pinned) |
| scc-admission scope | watched namespace `openshift-compliance`, injected UID `1001` | `layers.scc-admission.*` |
| CO catalog (proof only) | committed manifests in `internal/verify/compliance/catalog`, every image digest-pinned, served by `ghcr.io/complianceascode/compliance-operator-catalog@sha256:b5ad7b09…` | `make pin-compliance-catalog` |
| StackRox charts (example only) | `stackrox-{central,secured-cluster-services} 500.0.0`, tarballs SHA-256-verified | constants in `examples/stackrox/charts.go` |

Bump procedures for every pin, and the CI policy, live in
[docs/maintenance.md](docs/maintenance.md).

## Performance and resource budget

Measured on this repo's development machine — Apple M4 Pro, macOS 27.2.1,
Docker Desktop (daemon 28.1.1, 14 CPUs, 7.65 GiB RAM, overlayfs) — with
the default `create` (2 clusters, profile `full`) unless noted. Your
numbers will differ; the shape is what transfers.

| Operation | Measured |
|---|---|
| cold `create` (node + builder images pulled, fresh state root) | 105 s |
| warm `create` (images cached, fresh state root) | 85 s |
| re-run `create` over the same prefix (full reuse — the recovery path) | 8 s |
| `delete` (2 clusters + all state) | ~1 s |
| `verify` (2 clusters, all checks) | 17 s |
| 2 clusters, profile `minimal` (image-cached) | 58 s |
| 3 clusters, profile `full` (image-cached) | 89 s |
| full integration suite, cold CO image pulls | ~17 min |

Steady-state cost per cluster, measured with `docker stats` on idle
clusters: about **1 GiB RAM** and **15–20 % of one CPU** per node
container (of the 14 the daemon sees). Disk per cluster: a **~1.5 GB**
anonymous volume (etcd, containerd, logs — it grows with images loaded
into the cluster) plus a **~4 MB** container layer; the pinned node
image (~1.3 GB) is pulled once and shared by every cluster on the
daemon, as is the ~15 MB scc-admission image.

`--count` is **hard-capped at 64**: create refuses a larger count up
front. Within the cap, clusters 2..N are created concurrently with no
concurrency limiter, so the practical bound is the daemon's capacity.

## Known limitations

- Not OpenShift: no OAuth, console, marketplace, image builds,
  MachineConfig remediation, OpenShift routing, or integrated image
  registry. Routing is absent as a stack — no `Route` API is applied
  and no router exists to serve one (the applied `ingress` CRDs are
  API objects only), and the registry's operator and API are not
  applied at all. `rhcos4-*` node scans cannot work on kind nodes. The
  layers cover the API surface and admission behavior the targeted
  workloads need — no more.
- No StackRox Central/Sensor or Compliance Operator deployment as
  product functionality (example and verification only). Consumers own
  their e2e test suites.
- No cluster upgrades or Kubernetes version management beyond what
  kind provides.
- Docker only today. The runtime boundary (`provider.ImageBuilder` and
  friends) does not assume Docker; a podman implementation is a follow-up.
- kind-as-library is pinned (`sigs.k8s.io/kind v0.33.0`); its API is less
  stable than a CLI's, contained by the thin provider wrapper.
- Linux host tunings (inotify instance limits) are best-effort with a
  warning; on macOS they are notes.
- The scc-admission webhook's `failurePolicy` is `Ignore` (a wedged
  webhook degrades to OpenShift-absent behavior rather than blocking pod
  creation); its scope is the namespaces it is configured to watch.
- The module path `github.com/stackrox/kind-ocp` is provisional until the
  publishing location is confirmed; renaming is mechanical.

## Repository layout and history

Docs: [docs/PLAN.md](docs/PLAN.md) (architecture and increment records)
and [docs/maintenance.md](docs/maintenance.md) (bump procedures and CI).

kind-ocp is a productized port of the multi-cluster kind proof in
stackrox PR #23841. The porting map and preserved gotchas are in
docs/PLAN.md §4. The repo was built by an automated multi-agent
development workflow; docs/PLAN.md records the increment history.

License: Apache-2.0.
