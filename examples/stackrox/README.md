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

- Everything kind-ocp needs (see the root README): Docker (client and
  daemon, plus the buildx CLI plugin for the full profile), Go 1.27.4 or
  newer.
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
(default `$XDG_STATE_HOME/kind-ocp`). Two optional flags overlay your own
helm values on the generated ones (see
[pointing the example at your own images](#pointing-the-example-at-your-own-images)):
`--central-values <file>` for the Central release and
`--sensor-values <file>` for every Sensor release. The example always
uses profile `full` and never reads a kind-ocp config file: it is a
recipe, and an ambient config changing what it deploys would be a
surprise, not a feature.

On success the run prints the Central endpoint, the admin password's
location, a port-forward command for the Central UI, and the exact cleanup
command (`kind-ocp delete`).

## What this run proves — and what it does not

Proven, end to end: Central deployed and serving, cross-cluster NodePort
reachability (every Sensor is a workload pod on one cluster that reached
a NodePort service on another and stayed connected), and every Sensor
cluster registered and reporting `HEALTHY` in Central.

Not proven: anything that needs real OpenShift. The example pins
`env.openshift: false` for both charts, so StackRox's OpenShift
deployment mode — the SCC and Route paths — is never exercised (see
[why the Sensors do not run in OpenShift mode](#why-the-sensors-do-not-run-in-openshift-mode)).
And nothing anywhere in this repo drives a scan from Central to results:
the Compliance Operator proof covers operator-initiated scans, this
example covers deployment and connectivity. A Central-driven
scan-to-results round trip still needs a real OpenShift cluster or a
future proof.

## Pointing the example at your own images

The pinned released charts stay the default path, fetched and
digest-verified exactly as before. When you are testing your own build —
a Sensor off an unmerged branch, say — pass a values file instead of
editing the repo:

```sh
# Create the values file private: it can carry secrets (image pull
# credentials, proxy passwords), and the example refuses a file other
# local users could read. umask 077 makes the shell write it 0600.
umask 077
cat > my-sensor.yaml <<'EOF'
# Point the Sensor at an engineer-built image. The chart composes every
# component image; `image.main` is the one Sensor, Admission Control,
# and Compliance run.
image:
  main:
    fullRef: my-registry.example.com/stackrox/main:my-branch
EOF
go run ./examples/stackrox --sensor-values my-sensor.yaml
```

helm merges the later file over the example's generated values, so only
the keys you set change; everything the recipe pins (the registration
secret, the shrunk resource requests, the disabled collector) stays.
`--central-values` works the same way for the Central release. Both
flags accept one file each, validated before the clusters are touched:
the file must be a private, caller-owned regular file (a values file
can carry secrets, so the example refuses a symlinked or
world-readable one), and the directory holding it must pass the same
ancestor policy the example's workspace does — a values file reached
through a directory other local users can write is refused, because
the entry could be swapped between the check and the moment helm
opens it. The path is resolved to its absolute form before helm sees
it (a relative path — or a file named `-`, which helm reads as stdin —
would make helm open whatever its own resolution picks), and the
resolved path must not contain a comma, a double quote, a newline, or
a carriage return: helm parses each `-f` operand as a CSV record, so
such a path would make it load a different, unchecked file (the
example refuses those paths up front). Nothing in the file is ever
logged.
Your images must be pullable from every cluster (kind nodes have no
registry access beyond what the daemon and the network allow).

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
  Its name is derived from the *resolved* state root — the state root's
  own name plus `-stackrox-example` — and the derived location is
  opened with alias following disabled, because `kind-ocp delete`
  removes the state tree: a symlink at the workspace's base or leaf is
  refused wherever it points, so no alias can point the credentials
  into the state tree. The refusal, not a path comparison, is what
  keeps the trees apart — on a case-insensitive filesystem (macOS's
  default) `State` and `state` are one directory while remaining two
  strings, so no lexical overlap check could tell an alias naming the
  state root in the other case from any other alias.
- The directory is opened through kind-ocp's own filesystem boundary:
  every ancestor is verified before it is trusted (substitutable
  ancestors and permissive directories are refused), and the workspace
  tree — its base and the per-prefix directory alike — is created
  `0700` or verified caller-owned and private. The run writes through
  the resolved path the walk returned.
- The Central admin password is generated once (crypto/rand), stored
  `0600` in the work directory, reused across runs, and never printed or
  logged. Every credential file — the password and the Sensor values
  files — is checked private (owner, mode, no symlink) before each read
  and write; a symlinked or shared file is refused, not followed.
- The cluster registration secret rides in the Sensor values files,
  written `0600` and passed to helm with `-f` — never through argv,
  which other local users could read.
- helm's own output is discarded, not forwarded to the run's log: its
  diagnostics quote values-file content verbatim (a malformed YAML line
  is echoed back in the error), and those files carry the admin
  password and the registration secret. A failed helm run reports the
  exit status and the full command line, shell-quoted — every argument
  and the helm binary itself — so copying it into a shell reproduces
  the run even across paths containing spaces or shell
  metacharacters. Re-running it by hand prints helm's diagnostics,
  which can quote values content, to your terminal only — never to the
  run's log.
- The example's own API calls to Central authenticate it before any
  credential is sent: the TLS peer must present the certificate the
  chart's CA issued — the CA is read from the `central-tls` secret
  through the cluster's own authenticated Kubernetes API, and the
  certificate is verified as `central.stackrox.svc`, the name the chart
  generates. Redirect answers are refused rather than followed (Go's
  default client would carry the Authorization header to the redirect
  target), and a base URL that is not https is refused outright. The
  Sensors' connection is separate — mTLS with their registration
  identity over the shared network, verified by the readiness wait.

## Cleaning up

`kind-ocp delete` (the command the run prints) removes the clusters and
kind-ocp's state, but not the example's work directory: the admin
password and the Sensor values files stay on disk so a later re-run can
reuse them. When you are done with them, remove the work directory
itself — by default:

```sh
rm -rf ~/.local/state/kind-ocp-stackrox-example/<prefix>
```

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

- With the collector disabled there is no runtime process-level signal
  collection; that is a deliberate scope cut for a topology example, not a
  supported deployment shape.
- Deleting the clusters (`kind-ocp delete`) removes Central's database with
  them; the example is a recipe, not a durable StackRox installation.
