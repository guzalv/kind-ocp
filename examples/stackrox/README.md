# StackRox Central + Sensor across kind-ocp clusters

This example deploys [StackRox](https://www.stackrox.io) across a kind-ocp
prefix: **Central on cluster 1, Sensor on clusters 2..N**, connected over a
NodePort service on the network kind gives every cluster in the prefix. It
is the multi-cluster topology the clusters exist to host, run end to end:
when the example finishes, Central reports every Sensor cluster registered
and healthy — which means a workload pod on one cluster reached a NodePort
service on another and stayed connected.

It is an example, not product functionality: kind-ocp creates the clusters
and applies the OpenShift layers; everything here deploys third-party
software on top of them. The code drives kind-ocp through its public
surface (`config`, `kindocp`) plus the standard Kubernetes Go clients,
the way an outside consumer would, and lives as a `package main` so
nothing can import it. Its credential workspace is the one part that
reaches inside the module: it is verified with kind-ocp's internal
filesystem boundary (`internal/fscheck`), the same policy the state tree
walks, reused rather than forked. An outside consumer would keep its
credentials under a caller-owned directory it verifies itself.

## What it does

1. Creates (or reuses) `<prefix>-1..N` clusters with profile `full`, through
   the same kind-ocp façade the CLI uses.
2. Deploys Central on cluster 1 from the pinned
   `stackrox-central-services` chart — scanner disabled, telemetry disabled,
   resource requests shrunk so Central, Central DB, and its sidecar
   deployments co-schedule on a single kind node, exposed through a NodePort
   service.
3. Resolves the endpoint the Sensors use: the central cluster's node address
   on the shared kind network plus Central's allocated NodePort.
4. Generates a cluster registration secret (CRS — the modern replacement
   for init bundles, which Central 5.0 deprecates) through Central's REST
   API, over a localhost port-forward to the Central pod. On a re-run the
   name is already taken (Central refuses a name held by a non-revoked
   init bundle or CRS), so the previous run's CRS is revoked and a fresh
   one generated: revocation removes only the bootstrap credential, and
   Sensors that already registered keep their issued identities and
   connections.
5. Deploys Sensor on every remaining cluster from the pinned
   `stackrox-secured-cluster-services` chart — collector disabled
   (`collectionMethod: NO_COLLECTION`: no collector container; the
   compliance container stays, it serves the Compliance Operator
   integration), admission control scaled to one replica, each registered
   under its own cluster name.
6. Waits until Central reports every Sensor cluster `HEALTHY`.

The whole run is idempotent: re-running it converges the clusters
(kind-ocp reuse), the releases (`helm upgrade --install`), and the cluster
registration secret (revoked and regenerated under the recipe's name).

## Prerequisites

- Everything kind-ocp needs (see the root README): Docker, Go 1.27+.
- The `helm` CLI, v3 or newer.
- Network access to `charts.stackrox.io` (charts, verified against recorded
  SHA-256 digests) and `quay.io/stackrox-io` (the released public images the
  charts reference).

## Run

```sh
go run ./examples/stackrox
```

Flags mirror kind-ocp's own: `--prefix` (default `kind-ocp`), `--count`
(default 2, minimum 2 — Central on 1, Sensor on 2..N), `--state-root`
(default `$XDG_STATE_HOME/kind-ocp`). The example always uses profile
`full` and never reads a kind-ocp config file: it is a recipe, and an
ambient config changing what it deploys would be a surprise, not a feature.

On success the run prints the Central endpoint, the admin password's
location, a port-forward command for the Central UI, and the exact cleanup
command (`kind-ocp delete`).

## Pins and determinism

The two chart tarballs are the pinned unit, fetched once per run from
StackRox's public chart repository and verified against reviewed SHA-256
digests recorded in `charts.go`:

| Chart | Version | SHA-256 (prefix) |
|---|---|---|
| `stackrox-central-services` | 500.0.0 | `4a391463…2484c2` |
| `stackrox-secured-cluster-services` | 500.0.0 | `db9a1f31…254e788` |

The images the charts reference are the released public tags of the
appVersion (`quay.io/stackrox-io/main:5.0.0`, `central-db:5.0.0`, …). They
float by tag within the release the chart pins — that is what "released
public images" means here, and it is the documented trade-off: the chart
tarball is byte-verified, the images it names are the vendor's released
tags. Bumping the example to a new StackRox release means picking the new
version in the repository's `index.yaml`, recording its digest beside it in
`charts.go`, and running once; the two charts are released together per
StackRox version and should stay on the same appVersion.

## Secrets handling

- The work directory sits beside kind-ocp's state root (default
  `~/.local/state/kind-ocp-stackrox-example/<prefix>/`), not under the
  system temporary directory: it holds persistent credentials, and a
  predictable temp path has parents another local user could substitute.
- The directory is opened through kind-ocp's own filesystem boundary:
  every ancestor is verified before it is trusted (substitutable
  ancestors, symlinks, and permissive directories are refused), and the
  directory itself is created `0700` or verified caller-owned and
  private. The run writes through the resolved path the walk returned.
- The Central admin password is generated once (crypto/rand), stored
  `0600` in the work directory, reused across runs, and never printed or
  logged. Every credential file — the password and the Sensor values
  files — is checked private (owner, mode, no symlink) before each read
  and write; a symlinked or shared file is refused, not followed.
- The cluster registration secret rides in the Sensor values files,
  written `0600` and passed to helm with `-f` — never through argv,
  which other local users could read.

## Why the Sensors do not run in OpenShift mode

The clusters look like OpenShift to workloads that read the API (that is
the layers' purpose), and the chart would auto-detect that. The example
pins `env.openshift: false` for the StackRox charts themselves: StackRox's
OpenShift deployment mode builds shapes these clusters do not serve (SCC
binding for its own pods, Routes). The OpenShift-ness is for the monitored
workloads — the Compliance Operator's ProfileBundles and
api-resource-collector — not for StackRox's own deployment plumbing. The
Compliance Operator fitness proof (`internal/verify/compliance`) covers
that half.

## Known limitations

- Central's chart generates a self-signed certificate; the example's own
  API client skips TLS verification for its loopback port-forward. That
  tunnel is bounded: client-go binds its listener on `127.0.0.1` only,
  picks the local port itself, and every authenticated call runs under a
  context cancelled the moment the forward exits — so the admin password
  never travels to a local port the forward does not own (the Sensors'
  connection is separate: mTLS with their registration identity over the
  shared network, verified by the readiness wait).
- With the collector disabled there is no runtime process-level signal
  collection; that is a deliberate scope cut for a topology example, not a
  supported deployment shape.
- Deleting the clusters (`kind-ocp delete`) removes Central's database with
  them; the example is a recipe, not a durable StackRox installation.
