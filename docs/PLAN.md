# kind-ocp — implementation plan

Status: active. Owner: dev. Scope and acceptance criteria: `docs/BRIEF.md`
(authoritative). This file is the architecture and the ordered increment
checklist that the greenfield gates run against.

## 1. Objective

`kind-ocp` is a Go CLI and importable library that creates N (default 2)
OCP-like kind clusters: plain kind clusters plus a configurable set of
OpenShift layers. After `create` finishes, StackRox Central/Sensor and the
Compliance Operator deploy and work on every cluster. The repo creates
clusters; it deploys nothing.

## 2. Locked decisions

| Decision | Choice | Rationale |
|---|---|---|
| Language | Go 1.27, single `kind-ocp` binary plus importable packages | user decision (BRIEF §1) |
| CLI framework | spf13/cobra | standard, subcommands + flags |
| Config format | YAML (`gopkg.in/yaml.v3`), loaded only via `--config` or `KIND_OCP_CONFIG` | explicit and deterministic; no CWD magic |
| Config precedence | flag > env > file > built-in default | AC-2.2, unit-tested |
| kind provisioning | **kind as a library** (`sigs.k8s.io/kind` v0.33.0, pinned in go.mod) behind `provider.Provisioner` | no kind binary on the host; version pinned by go.mod (AC-4); programmatic kubeconfig export into the state dir (no `~/.kube/config` pollution); programmatic node access for cross-cluster checks. Risk (less stable library API) is contained by the thin wrapper and the version pin (BRIEF §9). |
| k8s access | `k8s.io/client-go` typed clientset (nodes, deployments) + dynamic client with server-side apply for arbitrary manifests | layers apply CRDs and schema-less CRs; SSA is also a ported gotcha (OLM CRDs exceed the client-side annotation limit) |
| SCC-UID webhook | **Go implementation**; image built at `create` time via digest-pinned multi-stage `docker build` (golang builder → `FROM scratch`), loaded into each cluster the same way `kind load` does | patch logic is plain Go and unit-testable (AC-6.2 explicitly lists it); no python runtime in the cluster; no third-party webhook image at all, which closes the PoC's floating `python:3.12-slim` gap (BRIEF §7.2) more strongly than digest-pinning a foreign image. The builder image is the pinned external input. Trade-off recorded in the layer docs. |
| openshift/api CRD sourcing (increment 4) | **Pinned runtime fetch over HTTPS**: the module's version/commit is embedded at build time from go.mod (a generated pin file — no committed CRD copies), and `create` fetches the 18 CRDs from that exact commit into a per-state-root cache with Go's net/http | AC-6.4 requires only Docker at runtime (no Go toolchain, no module cache on the host), and AC-3.4 requires go.mod to stay the single source of truth with no committed copies. Reading the module cache at runtime would tie the binary to a Go checkout; the pinned fetch keeps `go install` usable. Cluster creation already requires network (node-image pulls), so this adds no new environmental dependency. |
| Regeneration tool | ported to Go (`tools/regenerate`), fetches SHA-pinned upstream files over HTTPS, splices between `BEGIN/END UPSTREAM` markers | repo stays single-language; no python/PyYAML dependency (BRIEF §7.1) |
| Module path | `github.com/stackrox/kind-ocp` — **provisional** until the publishing location is confirmed (BRIEF §10.2) | org-standard guess; renaming before publishing is mechanical |
| License | Apache-2.0 (BRIEF §10.1) — confirm before publishing | org standard |
| CI cadence | lint + unit on every PR; integration job (create + verify + CO proof, ~10–20 min, one runner) behind an `integration` label, `workflow_dispatch`, and a nightly schedule on the default branch (BRIEF §10.3) | fast PR feedback; nightly determinism signal for the pinned external inputs |
| Binary distribution | `go install` initially; goreleaser deferred (BRIEF §10.4) | |

## 3. Architecture

### 3.1 Package layout

```
cmd/kind-ocp/            main() → internal/cli
internal/cli/            cobra commands: create, delete, list, kubeconfig, verify
kindocp/                 public façade: the provisioning loop (Create, Delete, List, Verify)
config/                  public config types, built-in defaults (pins), load + resolution
layers/                  public layer framework: Layer interface, Env, Registry, profiles
provider/                public Provisioner interface (future runtimes plug in here)
internal/provider/kind/  kind-as-library implementation
internal/state/          per-prefix state root, permissions, cluster records (internal: create results surface through kindocp.Record)
internal/kube/           k8s helpers: REST config, SSA apply, CRD/deployment waits, node readiness, cluster instance ID
internal/shell/          POSIX shell quoting for printed command lines
internal/layers/<name>/  the six builtin layer implementations
internal/builtin/        builtin.go — registers builtin layers (the ONLY file touched when adding a layer)
internal/verify/         CO fitness proof (imported only by tests/examples, never by create)
assets/                  embedded manifests + webhook source
test/integration/        build-tagged integration tests (Docker-gated)
tools/regenerate/        upstream-content regeneration tool
tools/pin-openshift-api/ regenerates the openshift/api pin file from go.mod
examples/                StackRox Central+Sensor recipe (example only)
```

`internal/cli` and `kindocp` share the façade: the CLI is a thin skin over the
library, so external Go consumers get the same behavior programmatically.

### 3.2 Layer model (AC-2)

```go
type Layer interface {
    Name() string
    Apply(ctx context.Context, env Env) error  // converge the cluster
    Verify(ctx context.Context, env Env) error // postconditions only; no state left behind
}
```

`Env` carries the target cluster (name, context, kubeconfig path), a contextualized
`slog.Logger`, and `DecodeSettings(layer, dst)` which decodes the
resolved per-layer config into the layer's own settings struct (pre-filled
with its defaults, so only explicitly set keys override — AC-4.3). It also
carries the cache-directory service for layers that fetch pinned upstream
content: `CacheDir` is a callback the framework binds to the layer being
applied, taking the layer's single content pin as its only argument, so a
layer can reach only its own cache directory under the pin it declares
(kindocp binds it per layer from the state store's walk-verified tree).
Only Apply environments carry it — calling it creates a directory, and
Verify leaves no state behind: the one transient mutation a check may
need (the scc-admission layer's probe pod) is removed, and its removal
confirmed, within the check. Layer names and pins are held to one safe
state-tree alphabet (`internal/safename`: lowercase ASCII letters and
digits with '-' inside, the shape of a layer name or a commit SHA),
enforced both at `Registry.Register` and at the state store's
`CacheDirFor` mapping: on a case-insensitive filesystem (macOS, a
supported platform) two identifiers differing only by case are one
directory, so two layers or two pins would silently share cache files —
kind-ocp refuses such identifiers explicitly instead of renaming them,
because the identifier is also the registry and config key.

- The `Registry` maps names to implementations and keeps registration order.
- Profiles: `full` = every registered layer in registration order; `minimal` =
  none; custom profiles from the config file select a subset. Unknown layer
  names in a profile are a validation error.
- Adding or removing a layer = new package under `internal/layers/` + one
  registration line in `internal/builtin/builtin.go`. The provisioning loop in
  `kindocp` never changes (AC-2.4).
- Registration order = apply order for the six builtins:
  `config-openshift` → `node-labels` → `olm` → `openshift-api-crds` →
  `platform-objects` → `scc-admission`. Reasons: OLM-created deployments need
  the master label (node-labels before olm); the real CRDs from
  openshift/api supersede the placeholder CRDs (config-openshift before
  openshift-api-crds); SCCs only validate against the real SCC CRD
  (openshift-api-crds before platform-objects). The scc-admission
  webhook scopes itself to its configured namespaces, which it creates,
  and depends on no other layer; it is last by convention.

### 3.3 Config resolution (AC-2.2, AC-2.3, AC-4)

```yaml
prefix: kind-ocp            # clusters are <prefix>-<i>; contexts kind-<name>
count: 2
profile: full               # default profile for every cluster
kindNodeImage: kindest/node:v1.36.4@sha256:099e…  # digest-pinned default
stateRoot: ""               # default: ${XDG_STATE_HOME:-~/.local/state}/kind-ocp
timeouts:
  nodeReady: 5m
clusters:                   # per-cluster overrides, keyed by full cluster name
  kind-ocp-1:
    profile: minimal        # the co-located Central pattern
profiles:                   # custom profiles
  olm-only:
    layers: [olm, node-labels]
layers:                     # per-layer settings; each layer documents its own
  olm:
    version: v0.46.0
    configmapRegistryImage: quay.io/operator-framework/configmap-operator-registry:v1.73.0@sha256:c59709…
```

Resolution: built-in defaults → overlay config file → overlay env
(`KIND_OCP_PREFIX`, `KIND_OCP_COUNT`, `KIND_OCP_PROFILE`, `KIND_OCP_CONFIG`)
→ overlay flags. Only the winning value of a key is parsed (a malformed
`KIND_OCP_COUNT` never fails a run that passes a valid `--count`). An
explicitly empty `--config` (`--config ''`) disables config-file lookup
entirely, `KIND_OCP_CONFIG` included — it is how the commands kind-ocp
prints for pasting stay independent of the environment they are pasted
into. A config file may contain exactly one YAML document; `count: 0` in
the file is rejected by create, not silently defaulted. The state root has
one per-invocation override (`--state-root`; default base `$XDG_STATE_HOME`)
and no env knob. `--count` and `--profile` are create inputs; the other
commands do not read their values — the CLI does not even read them outside
create — so a create command line can be reused verbatim (a malformed flag
value such as `--count=abc` is still a parse error on every command).
Resolve validates only the shared scope (the prefix) plus the structural
file format, so a create-only mistake (a bad `KIND_OCP_COUNT`, `--count 0`,
a cluster-3 override with count 2) never blocks delete, list, verify, or
kubeconfig. Create-only semantics are checked by `Config.ValidateCreate` in
the create path. Per-cluster overrides resolve after global resolution,
keyed by the final generated names. `kindocp.New` snapshots the resolved
Config (`Config.Clone`, a deep copy: nested profile slices and layer YAML
trees included) so a caller mutating its Config after construction cannot
re-scope or otherwise re-configure a live client. Validation: count ≥ 1,
prefix is a valid kind name (`config.ValidatePrefix`, also enforced by the
façade for library-constructed configs), profiles reference known layers,
state root creatable. All of this is unit-tested without Docker (AC-6.2).

Defaults live in exactly two kinds of places: global defaults
(`config/defaults.go`: prefix, count, profile, node image digest, state root,
timeouts) and each layer's settings struct (OLM version, webhook namespaces /
UID / builder digest, …). The README documents every default and pin (AC-4.2).

### 3.4 State (AC-1.3, AC-1.5)

```
${XDG_STATE_HOME:-$HOME/.local/state}/kind-ocp/<prefix>/
├── kind-ocp-state.json            # 0600 — state marker (proves ownership)
├── cache/<layer>/<pin>/…          # 0600 — layers' fetched-content caches, keyed by content pin (increment 4+)
├── clusters/<name>/kubeconfig     # 0600 — cluster-admin credential
└── clusters/<name>/record.json    # name, context, profile, layers, node image, instance ID, created
```

The state root is created `0700` (kubeconfigs are cluster-admin credentials —
BRIEF §8), lazily on first write, and it is a verified boundary, not just a
convention (`internal/fscheck`, `internal/state`): every component of the
path is checked before kind-ocp reads, writes, or returns anything under
it. Ancestors of the per-prefix directory may be shared system
directories — root- or caller-owned, writable by no one but their owner
unless the sticky bit protects entry substitution (the `/tmp` shape), with
symlinked components resolved hop by hop (macOS `$TMPDIR` lives under the
`/var` symlink). A symlink is only followed when the entry itself is
caller- or root-owned (in a sticky directory whoever owns an entry can
replace it), and every component of its target is checked — a target that
traverses another symlink is resolved the same way, hop by hop, so an
alias never depends on an unverified entry (a one-shot `EvalSymlinks`
would erase the intermediate links and hide exactly those). The
per-prefix directory and everything below it must be caller-owned,
private (0700/0600), and free of symlinks.
Verification and use are the same path: the walk returns the fully-resolved
directory it verified, and every read, write, provider handoff, and
recursive cleanup acts on that resolved path — never on the symlinked
alias it started from, which would leave the verified chain behind.
`delete` goes one step further for cleanup: it resolves the removable
root once, before deleting anything, and removes exactly that tree — an
ancestor alias re-pointed mid-delete is a refusal, not a silent switch
to whatever the state root resolves to by then. The `kubeconfig`
re-export hands back the path its post-export verification returned,
not the one resolved before it.
A restored or shared tree that cannot be verified fails closed with the
fix in the error; kubeconfigs are likewise refused by the client builder
(`kube.RESTConfig`) and by `kindocp.Kubeconfig` unless they are private,
caller-owned regular files. Credential writes are gated twice: the store
hands out a kubeconfig path only after a preflight (`fscheck.
CheckWriteTarget` via `store.KubeconfigPath`) that allows an absent file
and refuses an unsafe existing one — kind merges into an existing
destination, so the refusal has to happen before kind is invoked — and
the kind provider verifies what kind actually wrote, without repairing
it (no corrective chmod: kind creates new files at 0600, and a chmod
through an unverified path would itself be a write outside the
boundary). `record.json` gets the same treatment in both directions: a
read or a write through a symlinked or shared record file is refused.

Mode bits alone do not describe access on macOS: an extended ACL entry
can grant access the mode does not show (a 0600 kubeconfig with an
`everyone allow read,write` entry is world-readable in fact). Both
policies therefore check ACLs there (`fscheck`'s `acl_darwin.go`):
managed entries refuse any ACL entry at all (kind-ocp never creates
them), and ancestors refuse entries that grant write access beyond the
mode bits while tolerating denying entries and read-only grants — macOS
home directories carry a harmless `everyone deny delete` entry, and read
grants match what the mode bits already permit on shared system
directories. The check reads the extended ACL through libc
(`acl_get_file`), which requires a cgo build — the default on macOS; a
`CGO_ENABLED=0` darwin build fails closed rather than silently skipping
the check. Linux needs no ACL parsing of its own: POSIX ACL access is
reflected in the mode's group bits (the ACL mask), so an effective
named grant widens them — `setfacl -m u:other:rw` turns a 0600
kubeconfig into 0660 — and the mode policy already refuses it, while a
grant the mask leaves ineffective grants nothing (a directory created
0700 under a default ACL inherits masked entries and stays safe).
Parsing the entries themselves would add nothing the mode does not
already say, and would reject those safe inherited trees.


When kind-ocp first creates the per-prefix directory it writes a state
marker (`kind-ocp-state.json`); an existing unmarked directory is adopted
only when empty, and recursive cleanup (`delete`) only ever removes a
marked, verified tree — a pre-existing unrelated directory under a custom
`--state-root` is never deleted, and `delete` checks that before deleting
any cluster, so a refusal leaves everything in place.

Each record carries the cluster's
**instance ID** (the kube-system namespace UID): create binds it once the
cluster's API answers, before any layer runs, so every complete record —
and every pending record whose run died mid-layer — carries one. A cluster
deleted and re-created under the same name is a different instance, and
reuse or verify compares the live ID against the record's so a same-named
replacement is never trusted. An unbound record (its create run was
interrupted before the API answered) is never adopted either: the name
alone proves nothing, so reuse is rejected and recovery is delete and
re-create. `delete` removes every
kind cluster whose name has the exact generated shape
`<prefix>-<positive integer>` — queried from the provisioner, not from
state, and independent of the current count, so leftovers from a larger
count go too while overlapping prefixes (`team` vs `team-dev-1`) and
foreign names are never touched — **and** deletes the whole per-prefix
state directory. That closes the PoC's stale-state bug (BRIEF §7.4): a
`create` after `delete` starts from zero. Delete is idempotent; absent
clusters and absent state are not errors.

### 3.5 Provisioning flow (AC-1)

`create` resolves config, validates it eagerly (every cluster's profile is
resolved and every per-cluster override checked **before** the first cluster
is provisioned, so a mistake in the last cluster's profile cannot leave
earlier clusters behind), then for each cluster `<prefix>-<i>`:

1. Provision: if the kind cluster already exists, reuse it (reconcile) — but
   only when its state record matches the requested node image and profile
   and is bound to the live cluster instance; a mismatch (including
   "cluster exists with no record", i.e. unmanaged, an unbound record from
   an interrupted create, or a same-named replacement of the recorded
   instance) is rejected with instructions to delete and re-create, because
   kind-ocp never removes or upgrades applied layers. New clusters are
   created with `CreateWithRetain(true)` so kind keeps a half-created
   cluster instead of silently deleting it on a provisioning error.
2. Export the kubeconfig into the state dir (kind external endpoint),
   then verify it: the kind provider's post-write check (§3.4) is fatal —
   kind merges into an existing destination and would create a missing
   parent directory 0755. The store's preflight (§3.4) has already
   refused an unsafe existing destination before kind was invoked, so
   the verification refuses what kind produced instead of repairing it.
3. Write the record with status `pending` (managed, this image, this
   profile) — an honest marker if the run dies partway. On the reuse path
   it carries the existing record's binding; on the fresh path it is
   unbound until step 5.
4. Wait for node readiness: one bounded, context-aware check over the
   exported kubeconfig (`internal/kube.WaitForNodesReady`), on both the
   created and the reused path, before any layer touches the cluster. kind's
   internal wait is deliberately not used: it logs a timeout and returns
   success. The timeout bounds the whole wait through a deadline context
   passed to the node List call — a stalled apiserver response is cancelled
   at the deadline, and an all-Ready answer that arrives after the deadline
   is rejected, not accepted (the poll helper itself would accept a
   post-deadline "done"). Zero nodes is a failure, not a pass.
5. Bind the record to the cluster instance (kube-system UID) and rewrite it
   still pending — before any layer runs, so a mid-layer interruption
   leaves a bound record whose retry converges. A cluster replaced between
   the reuse check and this binding is rejected here too.
6. Apply each layer from the resolved profile, in registration order, with
   a contextualized logger; each layer decodes its own settings.
7. Rewrite the record with status `complete`, keeping the binding.

Failure at any step: exit non-zero with a chain that names cluster, step, and
cause; clusters are left in place (AC-1.7); and an executable cleanup command
— `kind-ocp delete --config '' --prefix <p> --state-root <root>` for the
effective prefix and state root, POSIX-shell-quoted by `internal/shell.Quote`
so a state root containing spaces or quotes survives a copy-paste — is
always printed after a failed attempt, whether or not the run recorded any
clusters. The `--config ''` pin makes the pasted command independent of the
config-file lookup: a losing or stale `KIND_OCP_CONFIG` in the shell the
command is pasted into cannot break recovery (the kubeconfig success hint
carries the same pin). Explicit `delete` is the cleanup path; there is no
`--teardown-on-failure` flag (it deleted pre-existing clusters too, and an
always-printed exact command is simpler and safer).

Increment 1 created clusters sequentially. Increment 7 made clusters 2..N
concurrent (per-cluster state isolation, the PoC's parallel-deploy lesson) and
keeps cluster 1 sequential-first: its create establishes the shared `kind`
network and pulls the pinned node image — the two cluster-scoped resources
every later cluster depends on — so the concurrent wave never races on either.
Before the fan-out, create checks cancellation, then ensures the state root
once (the discipline `state.walk` prescribes for concurrent creation — and the
boundary check the host tune runs behind, so a run whose state root is refused
never changed host settings on its way to failing), and only then asks the
provisioner — only when it implements the optional host-limit capability
(`provider.HostLimitRaiser`), which the kind runtime does — to raise
`fs.inotify.max_user_instances` best-effort (Linux only, warn on refusal,
note on other platforms; a provisioner without the capability is never asked,
so nothing but a real container runtime on this host changes host settings,
and a cancelled run asks for nothing at all). The raise itself re-checks
cancellation before any write: the direct procfs write cannot carry the
context, so the check is explicit, and a run that gave up never writes. The
wave then runs to completion: every cluster's failure is reported in one
aggregate error, and no in-flight layer application is cancelled out from
under its record.

All clusters share kind's default `kind` Docker network (kind behavior), which
is what makes cross-cluster NodePort reachability work — asserted in increment
7's verify (AC-1.4).

### 3.6 Verify model (AC-5.1)

`verify` runs, per cluster: global checks (all nodes Ready — zero nodes is a
FAIL; from increment 7 also cross-cluster NodePort reachability), the
record's instance identity (a same-named replacement FAILs `instance-id`,
and so does a record that was never bound to an instance), and every
enabled layer's `Verify`. Management state is reported
explicitly: an existing cluster without a state record FAILs
`managed-state` (instead of passing vacuously with zero layer checks), and
a record whose create run never finished FAILs `create-status`. Clusters
are selected by `list`, but every record-derived check (management status,
create status, instance identity, context, layer list) reads one current
record snapshot after selection: a record removed mid-verify FAILs
`managed-state` and stops, and a replaced record is verified as it now is.
An `instance-id` FAIL stops that cluster's remaining checks: they are
meaningless against a cluster the record does not describe. Output is
one line per check: PASS / FAIL (with detail) / SKIP (with reason). Any
FAIL exits non-zero. `verify <name>…` restricts to named clusters.

The cross-cluster NodePort check (AC-1.4, the Central→Sensor pattern's
requirement) runs after the per-cluster checks, over exactly the
clusters whose identity checks passed — a peer that failed
verification is reported by its own lines, never by a reachability
failure it did not cause (and a cluster named twice on the command
line is rejected up front: as two peers that are one cluster it would
satisfy the pair count with nothing to connect to). Per ordered pair
(source A, target B): on B, through the Kubernetes API, a namespace
holds a selectorless NodePort Service whose hand-written EndpointSlice
points at the endpoint B's own `kubernetes` service routes to (the
apiserver — a listener that exists on every cluster regardless of
profile, so the check pulls no image and depends on no layer); on A,
a TCP connect from A's control-plane node through the provisioner's
narrow probe capability (`provider.TCPProber`, typed IP and port —
no arbitrary node commands cross the provider boundary) — no image
preloaded on a kind node carries a connect tool (the node images are
distroless), so a pod-based client would force a registry pull into a
check that must also run on minimal-profile clusters. The endpoint a
probe receives is typed (netip.Addr and port) and never parsed as
shell language: the kind adapter's node-side script is a static
constant and the endpoint crosses to it as quoted positional
parameters, with scoped addresses and out-of-range ports refused
before the runtime is invoked at all. A prefix with a
single verified cluster SKIPs the check with that reason, and so does
a provisioner without the probe capability.

The probe's namespace ownership is proven, never assumed: the
namespace's name is unique to one preparation (a fresh random tail
every run, so concurrent verifies and killed-run leftovers never
collide and nothing is ever adopted), and the create stamps the
namespace with a per-attempt ownership marker (a label only that
attempt's create ever set), because a create that errors is not proof
nothing was created — the server can persist the namespace and lose
the response, and client-go's own retry can commit one attempt and
surface the next one's refusal. An ambiguous create is settled by one
follow-up read of the name, under the marker and through the same
client, on its own bounded context (the deadline that broke the
create must not break the read that settles what it left behind): a
namespace carrying the marker is this attempt's — the preparation
fails, but the namespace is registered for cleanup under the UID the
read observed — a namespace without it is not the check's and is left
entirely alone, and a read that can decide neither way reports the
name and the marker (which a late landing would carry) as an
unresolved outcome. The UID returned by the create (or observed by
the resolving read) is required as the deletion precondition (a
namespace replaced under the same name is not the check's to remove),
and the mutation stays bound to the cluster instance verify
validated — the identity is re-read through the very client the
namespace is created through, and that client is retained for
cleanup, so a kubeconfig swapped to a same-name replacement between
the record check and the preparation is a refusal. Before each child
write (the Service, the EndpointSlice) the namespace is re-read and
confirmed still the created instance: a replacement observed there is
a refusal, never written into. The window between a re-check and the
write after it cannot be closed by a check, so each child is stamped
with the attempt's marker and its UID retained when its create
answers. When the namespace delete cannot remove the children — the
name is held by an instance the check did not create, which is proven
by reading the name, never assumed from the delete's 409 (an
admission refusal answers the same status with the original still
live) — both children are attempted however the first goes: each is
removed by its own UID, or, for a create whose response was lost,
resolved by the marker on a bounded read and removed under the UID
the read observed, while the replacement and everything in it the
check did not create survives. Every deletion is confirmed by reading
under the cleanup's budget — a delete's return is a request, not a
result (finalizers hold objects past it): NotFound or a different UID
proves the original gone, and the same UID still serving is reported
as leftover state, never passed over. Cleanup covers partial
preparations exactly like completed
ones (a preparation that fails after the namespace exists still
returns it), runs under its own bounded context — a cancelled or
expired check still removes what it created — and its failures fold
into the owning cluster's single line, never a second line after a
PASS. Every setup call is deadline-bounded like the polling calls
(AC-6.5). Like every layer check it leaves no state behind on every
path the process survives; a namespace left by a killed run is never
adopted or removed (the name cannot collide, and deletion by name
alone would remove state the run cannot prove it owns) — deleting the
cluster removes such residue with everything else. The pod-level half
of AC-1.4 — a workload pod on A reaching B's NodePort — is asserted
by the integration test with a real client pod, and end to end by the
StackRox example (increment 9).

### 3.7 Errors and logging

`log/slog` text handler on stderr at info level (library consumers pass
their own `slog.Logger` through `kindocp.Options` and control verbosity
there; the CLI carries no verbosity flag until a concrete need exists).
kind's own progress is adapted onto the same logger with a verbosity
boundary: user-facing messages (kind level 0 and below) pass through, while
kind's debug/trace output — which includes the raw kubeadm config with the
bootstrap token and the kubeadm init/join output — is discarded, so default
logs never contain credential material (BRIEF §8). Loggers are derived per
cluster and per layer (`cluster=kind-ocp-2 layer=olm`). Errors wrap with
cluster, layer, and operation context (`fmt.Errorf` chains, no error-code
enum). No bare sleeps: waits are condition-based with timeouts (internal/kube
poll helpers — AC-6.5), and every wait bounds its API calls through the
deadline context.

## 4. Porting map from PR #23841 (BRIEF §7)

| PoC (tests/e2e/kind-compliance/) | kind-ocp |
|---|---|
| `ensure_kind_cluster` | `internal/provider/kind` Create + wait |
| `raise_inotify` | best-effort Linux sysctl with warning (increment 7) |
| `install_olm` | `internal/layers/olm` |
| placeholder CRDs + 3 singletons + Infrastructure status patch (`install_co` config-shim part) | `internal/layers/configopenshift` + `assets/` |
| node labeling (`install_co_olm` label line) | `internal/layers/nodelabels` |
| `OPENSHIFT_CRD_MAP` + `stage_openshift_crds` | `internal/layers/openshiftapicrds` (path map + staging from the pinned `openshift/api` module) |
| `apply_platform_shim` (objects.yaml, sccs.yaml) | `internal/layers/platformobjects` + `assets/` |
| `regenerate.sh` (+ python splice) | `tools/regenerate` (Go) |
| `install_scc_webhook` + `scc-shim-webhook.py` | `internal/layers/sccadmission` (Go webhook, TLS generated in Go, namespaces/UID configurable, concurrent server) |
| `install_co` OLM part (CatalogSource/OperatorGroup/Subscription, CSV fallback), `wait_profilebundles_valid` | `internal/verify/compliance` (CO fitness proof — never reachable from `create`) |
| multi-cluster parallel `up`/`down`/`verify` orchestration | `kindocp` façade |
| NOT ported: `render_charts`, `deploy_central`, `central_internal_endpoint`, `start_central_pf`, `generate_init_bundle`, `deploy_secured`, `central_api`, `cluster_id_for`, `cluster_ready` | `examples/stackrox` (increment 9, example only) |

Gotchas to preserve (BRIEF §7.3) land with the code that needs them and are
asserted by verify checks: OLM CRDs via SSA; wait for CRD Established before
using kinds; CatalogSource in namespace `olm`; SCCs via create (or SSA), with
delete+recreate on re-runs; status writes through the status subresource;
master label on control-plane nodes; serving-cert secret before CO starts;
CSV channel-head fallback; per-cluster temp/state paths; per-wait deadlines;
exit-code fidelity in Make recipes.

## 5. Increment checklist

Each increment is one gated pass: implement → verify (tester) → review →
security → triage. Acceptance for every increment: `make build test lint`
green, unit tests need no Docker, integration evidence attached.

The entries below record what each increment delivered and the design
decisions behind it; the narratives of the review rounds that shaped them
are deliberately not kept here — the decisions are in the code, the tests
that pin them, and §3.

- [x] **1. Scaffold and framework skeleton.** Go module, Makefile
  (build/test/lint), golangci-lint config, Apache-2.0 LICENSE, .gitignore.
  CLI: `create`, `delete`, `list`, `kubeconfig`, `verify`. Config system with
  documented precedence (flag > env > file > default), per-cluster profile
  overrides, custom profiles, validation; all pins as defaults (node image
  digest). Public layer framework (`layers.Layer/Env/Registry`) with an empty
  builtin registry — profile `full` resolves to all registered layers,
  `minimal` to none. kind-as-library provisioner behind
  `provider.Provisioner`. State root with 0700/0600 permissions, kubeconfig
  export into state, idempotent prefix-scoped delete clearing all state
  (re-create works). Sequential `--count N` create (default 2), names
  `<prefix>-<i>`. On failure: keep clusters, name cluster + step, print the
  exact delete command — an explicit delete is the cleanup path (a
  `--teardown-on-failure` flag was evaluated and dropped: it also deleted
  pre-existing clusters). The cleanup command is POSIX-quoted
  (`internal/shell`) and pins `--config ''` so it survives a copy-paste into
  any environment. `verify` runs the node-Ready global check. Records carry
  the cluster's instance ID (kube-system UID), bound before the first layer
  runs: a same-named replacement is rejected by create-reuse and FAILs
  verify's `instance-id` check; an unbound record (interrupted create) is
  rejected the same way; a mid-layer interruption keeps its binding and its
  retry converges.
- [x] **2. Kubernetes helpers + `olm` layer.** `internal/kube`: REST config
  from state kubeconfig, dynamic-client server-side apply, CRD Established
  wait, deployment rollout-complete wait (kubectl rollout status semantics:
  generation observed, desired updated and available replicas, none left
  from the old set — the Available condition alone reports success
  mid-rollout under OLM's Recreate strategy), manifest fetch from an HTTP
  client the fetcher itself owns (callers inject only a transport and the
  timeout) so the https-only redirect rule and the bound chain are decisions
  of the fetcher no caller can re-scope. `olm` layer (Apply + Verify):
  pinned release v0.46.0 default, SSA for CRDs (annotation-limit gotcha),
  Established wait, rollout waits; the release's two manifest files are
  verified against reviewed SHA-256 digests before decoding (release assets
  are mutable under a tag), a version override is trusted as pinned without
  a reviewed digest — documented in the layer. The layer also closes the
  release's two floating image references: it drops the public
  `operatorhubio-catalog` CatalogSource (nothing kind-ocp targets consumes
  it) and pins the catalog-operator's `--configmapServerImage` via the
  `configmapRegistryImage` setting (version-and-digest pin of the
  operator-registry image the release itself uses). Verify checks both
  deployments Available and `operators.coreos.com/v1alpha1` served.
- [x] **3. `config-openshift` and `node-labels` layers.** Port the 13
  placeholder CRDs and the 3 singletons as embedded assets; Infrastructure
  status via the status subresource (Sensor's OpenShift-detection input);
  master label on control-plane nodes — the empty-string value, which the
  CSV nodeSelectors that schedule OLM operators match exactly (a non-empty
  value schedules them nowhere, so Apply and Verify enforce the value, not
  just the key's presence). The placeholder CRDs are bootstrapped with
  create-only semantics: each is created only where absent, never
  server-side applied over an existing CRD of the same name (a re-create
  whose later openshift-api-crds fetch fails must not overwrite the real
  CRDs with placeholders), and the creates go straight to the API's fixed
  CRD resource path (one POST per CRD, no per-object discovery). Verify
  checks: `Infrastructure/cluster` readable with `status.platformStatus`;
  every control-plane node labeled `master=""`.
- [x] **4. `openshift-api-crds` layer.** `github.com/openshift/api` pinned
  in go.mod is the single source of truth. A build-time-generated pin file
  carries the module's commit, version, the expected SHA-256 of each of the
  18 manifests as it exists in the pinned module, and the byte size of the
  largest (kept in sync with go.mod by unit tests). At `create` time the
  layer fetches the 18 CRDs over HTTPS at exactly that commit into a
  per-state-root cache keyed by commit; every cache read and download is
  validated against the build-time digests; every cache read is bounded by
  the largest pinned manifest; only a missing or corrupt entry is fetched
  and atomically replaced; the digest-validated downloads are the objects
  Apply applies. No committed CRD copies (AC-3.4). SSA, Established wait
  for all 18. Verify checks all 18 Established **and** carrying the real
  schema — the served root schemas themselves, not a marker (every version
  of a real CRD defines root properties and preserves no unknown fields;
  the placeholder is the exact opposite, and the traits are pinned against
  the module at build time). Cache directory names — the layer's and the
  pin's — are held to the safe state-tree alphabet (§3.2), refused at
  registration and at the mapping, never silently renamed.
- [x] **5. `platform-objects` layer + regeneration tool.** Port `objects.yaml`
  and `sccs.yaml` as embedded assets. Namespaces first, then singletons
  (status via subresource), SCCs with create semantics and delete+recreate
  on re-runs. Port the regeneration tool to Go (`tools/regenerate`):
  SHA-pinned upstream fetches, marker-bounded splice, shape validation
  before writing. Both assets are built and validated in memory before
  anything is written; the assembled SCC stream is validated against the
  expected kinds, names, and every top-level field the pinned SCC CRD marks
  required (the twelve-key set the layer's Verify shares, itself pinned
  against the module by test); the splice's marker pairing is strict (a
  BEGIN is closed by the very next marker line; a missing, duplicate,
  nested, or unmanaged pair is refused) and bound to its destination (the
  supported layout is exact: a literal block scalar config.yaml, unique
  keys, unique destination identity — five wrong shapes are refused, pinned
  by command-path regressions); the managed JSON key must occur exactly
  once; the replacement is a byte-range edit that never normalizes
  untouched bytes; the spliced file is validated against the pre-splice
  file as decoded objects (every object unchanged but the two managed
  argument maps, which are compared as the managed map alone — raw bytes
  for every other JSON key, so a value no float64 can represent neither
  breaks the comparison nor gets normalized). Applies across the repo take
  their field manager as a required argument (`kube.Client.Apply`/
  `ApplyStatus`): no hidden default, no client clone. The status-subresource
  404 that means "this kind carries status in the main resource" is
  recognized only for networks.config.openshift.io; anywhere else a 404 is
  an error. Verify checks: both SCCs, the four control-plane ConfigMaps,
  singleton CRs with populated status.
- [x] **6. `scc-admission` layer.** Go webhook (concurrent server) with
  configurable watched namespaces and injected UID; TLS material generated
  in Go; image built via a digest-pinned multi-stage docker build and
  loaded into each cluster; MutatingWebhookConfiguration scoped by
  namespaceSelector; failurePolicy `Ignore`, documented with the trade-off
  (a wedged webhook degrades to OpenShift-absent behavior rather than
  blocking pod creation). Verify check: a pod with `runAsNonRoot: true`
  and no `runAsUser` in a watched namespace becomes Running with the
  injected UID.
  The webhook is the most heavily hardened component in the repo, because
  it is an admission controller and its Verify mutates the cluster:
  Apply proves the stored registration converged before anything probes
  (server-side apply preserves fields another field manager owns, so the
  stored MutatingWebhookConfiguration is read back and checked for drift —
  namespace allowlist, Pod CREATE rules, no extra entries, no nonempty
  objectSelector, no matchConditions — and any drift fails the create
  naming the recovery, a scoped `kubectl --kubeconfig <path> delete`
  rendered against the failed cluster's own kubeconfig and
  POSIX-quoted). Every client the probe uses is built from one loaded
  kubeconfig whose transport refuses redirects; the probe's create is
  exactly-once (MaxRetries 0); the mutation is bound to the cluster
  instance verify validated (`Env.ClusterInstanceID` — the layer re-reads
  the identity through its own clients and refuses to mutate anything
  else); both the webhook Deployment's template and the probe pod set
  `automountServiceAccountToken: false`; the probe's no-redirect client
  reports its failures without embedding redirect targets (a Location can
  carry credentials in its userinfo or query). The drift, secrecy,
  converge, and wire-identity behaviors are pinned by unit regressions
  against fake apiservers, plus a real-API integration regression for the
  SSA-merge semantics.
- [x] **7. Multi-cluster + complete verify.** Concurrent creation of
  clusters 2..N (per-cluster state isolation), N=3 covered. Cluster 1 runs
  sequential-first: its create establishes the shared `kind` network and
  pulls the pinned node image, so the concurrent wave never races on
  either. The store root is ensured once before the fan-out; the wave runs
  to completion with per-cluster errors joined into one aggregate; a
  cluster-1 failure stops the run before any other cluster starts.
  Best-effort inotify raise on Linux (floor 512, never lower a higher
  value, warning on refusal, note on non-Linux) behind the optional
  `provider.HostLimitRaiser` capability, ordered behind the state-boundary
  check and cancellation. `verify` completes: global checks + every enabled
  layer's check, PASS/FAIL/SKIP output.
  The cross-cluster NodePort check (AC-1.4) runs after the per-cluster
  checks, over exactly the clusters whose identity checks passed: on each
  target, a selectorless NodePort Service whose hand-written EndpointSlice
  points at the endpoint the cluster's own `kubernetes` service routes to;
  from each source cluster's control-plane node, a TCP connect through the
  narrow `provider.TCPProber` capability (typed netip.Addr and port; the
  node-side script is a static constant, the endpoint crosses as quoted
  positional parameters, scoped addresses and out-of-range ports refused
  before the runtime is invoked). The endpoint the check reads from a
  cluster's API is corroborated against the provisioner's own view before
  any write (`provider.ControlPlaneAPIEndpointResolver`: the addresses the
  runtime reports for the node on the network Docker's own record says kind
  attached it to, each carrying kind's internal API port — the full
  address-and-port must match, and the Service backend is the
  provisioner-owned endpoint), so a stale or tampered EndpointSlice cannot
  produce a PASS that tested nothing. The check's namespace is unique to
  one preparation, stamped with a per-attempt ownership marker, registered
  for cleanup the moment it exists, deleted under UID preconditions with
  every deletion confirmed by read; children run under their own fresh
  deadline after a replacement is confirmed; ambiguous creates are settled
  by one bounded marker read, never assumed. Cleanup covers partial
  preparations exactly like completed ones and folds its failures into the
  owning cluster's single line. Every lifecycle client kind-ocp builds for
  its own paths refuses redirects at the transport level. The pod-level
  half of AC-1.4 is asserted by the integration test with a digest-pinned
  busybox client pod. Integration tests: default count=2 full profile and
  a count=3 run.
- [x] **8. CO fitness proof + CI.** `internal/verify/compliance`: the
  serving-cert secret (Go-generated certificate) before the Subscription,
  CatalogSource in `olm` addressing the proof's own in-cluster catalog
  server, OperatorGroup + Subscription, CSV resolution with the
  deterministic channel-head fallback, ProfileBundle VALID waits, ocp4-cis
  ScanSettingBinding with ≥1 PASS and ≥1 FAIL ComplianceCheckResult. A
  deployment proof, not a verify check — never reachable from the create
  path (an import-boundary test fails the build on any import outside
  itself and the tagged integration test), one-shot on a fresh cluster (a
  serving-cert marker refuses a second run), fixed budgets behind a
  budgets struct, a fresh deadline per wait. The Subscription carries no
  config.env: the upstream catalog's CSV ignores those overrides, and the
  operator's own `--platform` flag defaults to OpenShift.
  The catalog the proof installs from is committed in this repo
  (`internal/verify/compliance/catalog`): `tools/pin-compliance-catalog`
  (run via `make pin-compliance-catalog`) re-derives it from the pinned
  bundle and image digests through a pinned, externally-run crane — every
  index the tool trusts is bound by the tool's own hash, each platform's
  image is pulled by the concrete child digest the verified index names
  into a digest-verified local archive before being exported, both
  platforms must agree byte-for-byte, and crane runs in an isolated
  credential environment (private workspace, empty Docker config,
  allowlisted child environment) owned end-to-end by the tool's run
  function. The bundle entry embeds the manifests inline and carries no
  image at all; the staged pod runs the pinned image's own `/bin/opm
  serve` with the shape OLM's own catalog pods ride (subPath-mounted file,
  restricted PodSecurity, `olm.catalogSource` label); a staging wait fails
  fast before any OLM object is applied. The one external image the
  resolution still depends on is the pinned catalog image itself
  (DefaultCatalogServerImage). The catalog README records the pin table,
  the bump procedure, and the GO-2026-6865 advisory at the crane pin
  (reassess at the next crane or bundle bump).
  The scc-admission webhook image build moved behind
  `provider.ImageBuilder` (BuildImage(ctx, ref, contextDir) — the contract
  names a populated context directory and a reference, never a runtime):
  the kind provider implements it as exactly one `docker build
  --provenance=false`, argv-pinned by regressions, bound into
  `layers.Env.BuildImage` under the same rule as LoadImage — present
  exactly when the provisioner carries the capability, never in Verify
  environments. No layer invokes a container-runtime CLI directly; the
  boundary does not assume Docker (a podman implementation can follow).
  CI: lint + unit on every PR (plain `pull_request`, never
  `pull_request_target`), integration behind the `integration` label (the
  `labeled` activity type re-evaluates the gate on an open PR),
  workflow_dispatch, and a nightly cron; actions SHA-pinned, workflow-level
  `contents: read` and nothing more, no secrets. The suite's Docker gate is
  environment-aware: locally an unavailable daemon skips with a clear
  message, under CI (`CI=true`) the same condition fails the test — a
  green run that skipped everything would be a false pass. Measured cold,
  the full uncached suite (including the CO proof's per-cluster image
  pulls) runs in ~17–18 minutes against the Makefile's 60m go test budget
  and the CI job's 90m.
- [ ] **9. StackRox example + docs + release readiness.** `examples/stackrox`
  recipe: Central on cluster 1, Sensor on 2..N over NodePort on the shared
  network, released public images, collector disabled. The recipe is a
  `package main` (structurally unimportable — the create path can never
  reach it) that uses only kind-ocp's public surface (`config.Resolve`,
  `kindocp.New/Create/Kubeconfig`) plus standard client-go, the way an
  outside consumer would. It pins the two StackRox chart tarballs
  (stackrox-central-services and stackrox-secured-cluster-services 500.0.0,
  appVersion 5.0.0 — released public images `quay.io/stackrox-io/main:5.0.0`
  and `central-db:5.0.0`), fetching them once per run from
  charts.stackrox.io and verifying them against recorded SHA-256 digests
  before a bounded, traversal-refusing extraction. Central deploys with
  NodePort exposure, scanner and telemetry off, and resource requests
  shrunk to kind scale; Sensors register with a cluster registration
  secret (CRS — init bundles are deprecated in Central 5.0) generated
  through Central's REST API over a client-go port-forward to the Central
  pod. The CRS name is deterministic, so a re-run converges: Central
  refuses a name held by a non-revoked init bundle or CRS, and revocation
  removes only the bootstrap credential — Sensors that already registered
  keep their issued identities and connections, while the helm upgrade
  that follows re-applies the fresh manifest to every Sensor cluster. The
  readiness gate is Central reporting every Sensor cluster registered
  and HEALTHY — which is the end-to-end AC-1.4 proof (a
  workload pod on one cluster reaching a NodePort on another) and the
  modern replacement for the PoC's init-bundle flow. Secrets (admin
  password, CRS) live in 0600 files under a 0700 work directory outside
  the state root, passed to helm via `-f`, never through argv, never
  logged. The example's host requirements beyond kind-ocp's own: the helm
  CLI. Unit tests (no Docker, no helm, no network): chart fetch
  verification and safe extraction, values rendering, CRS decode and
  validation, Central API client and readiness classification against an
  httptest server, node/NodePort/pod discovery against fake clientsets.
  Three behaviors the live runs pinned as regressions: the values
  files and chart roots are full paths (helm -f takes paths, not names,
  and a chart archive carries its own top-level directory — the resolved
  root is validated to be exactly one directory holding Chart.yaml),
  authenticated Central API calls retry 401s within a bounded window,
  because an upgrade that changed the admin password rewrites
  central-htpasswd through a helm hook and Central applies the new file
  on its own watcher's schedule, and CRS generation converges on the
  name conflict a re-run hits — a 409 from the generate call lists the
  same-named CRS, revokes it by id, and regenerates (Central reports a
  failing revocation inside a 200 answer, so the response's per-id
  errors and its revoked-id coverage are both checked). Live runs
  (evidence in the workflow output): the recipe converged end to end —
  Central on kind-ocp-1 at
  its NodePort on the shared network, Sensor on kind-ocp-2 registered
  and HEALTHY in Central, `kind-ocp verify` all-PASS on the same
  clusters, `kind-ocp delete` leaving zero containers — and a
  back-to-back second run converged too: it revoked and regenerated the
  CRS, upgraded the Sensors with the fresh manifest (the Sensor pod
  rolls, keeps its issued identity, and reconnects within seconds), and
  ended with every Sensor cluster HEALTHY while Central's CRS listing
  holds exactly the one fresh secret, so re-runs do not accumulate
  credentials.
  README: the root README is the product readme (purpose, quickstart,
  prerequisites with the Docker requirement and the podman follow-up
  through the ImageBuilder boundary, architecture, config reference,
  every default and pin, bump/maintenance policy, known limitations); the
  greenfield workflow doc it replaced is preserved at `docs/workflow.md`
  (`.opencode/` is a protected configuration path for the automated
  sessions that develop this repo, so the documented fallback applies).
  Consumer-facing docs were stripped of process artifacts: run IDs and
  review-round narratives are out of BRIEF/PLAN, leaving each increment's
  deliverables and design decisions; the git history was squashed into a
  single commit describing the software, as the release-readiness
  requirement demands.
  The CI workflow move (`docs/ci-workflow.yaml` →
  `.github/workflows/ci.yaml`) was attempted and refused by the
  workflow-node sandbox: `.github/` is a protected configuration path for
  every tool in these sessions, the same policy increment 8 recorded.
  DONE since the last pass: an authorized normal session moved the file
  (commit `63e1d13`) — AC-6.7 is satisfied. Remove the staged-move
  instructions and the restriction narrative from the consumer-facing
  docs in this pass.
  Outstanding from the last review (P1, must fix before release):
  `examples/stackrox/central.go` CRS conflict handling collects and
  revokes every same-named match, then verifies batch coverage — no
  requirement needs batch revocation, and Central's `IssueCRS`
  `store.Add` rejects duplicate active names. Resolve exactly one named
  ID, reject zero or ambiguous matches before PATCH, and check the one
  returned ID while preserving the response-error check. Keep the 409
  re-run regression passing.
  Carried gate findings folded in: the failed-build regression's fake `go`
  emits a unique stderr marker and the test asserts it in the returned
  error (proving the fake ran, not that go failed to start);
  `craneWorkspace.root` is removed (assigned, never read — run owns the
  cleanup path); the workspace-creation failure path (unwritable TMPDIR →
  clean instant failure) gained its test.
## 6. Testing strategy

- Unit (no Docker): config resolution and precedence, per-cluster overrides,
  profile/layer selection, name generation, state paths and permissions, CRD
  path map, webhook patch logic, manifest rendering, wait conditions against
  fakes.
- Integration (build tag `integration`): lifecycle per increment, cross-cluster
  reachability (7), CO proof (8), N=3 concurrency (7). Docker-gated with a
  shared guard that is environment-aware: locally an unavailable daemon skips
  the test with a clear message; under CI (any environment setting `CI`,
  which every GitHub Actions runner does) the same condition fails it — a
  green CI run that skipped every test would be a false pass for exactly the
  job whose purpose is to run them.
- Manual evidence per increment: real `create`/`verify`/`delete` runs with
  command output kept in the workflow run (this repo has no external PR until
  publishing).

### 6.1 Local macOS development note

On macOS with Docker Desktop, `/var/run/docker.sock` is a symlink to the
Desktop daemon's socket, and that symlink's target has been observed to
change (it once pointed at a rootless podman machine, which kind refuses
with "rootless provider requires … Delegate=yes"). The documented local run
commands therefore pin the endpoint explicitly instead of trusting the
symlink:

```
export DOCKER_HOST=unix://$HOME/.docker/run/docker.sock
```

The full-profile suite also builds the scc-admission webhook image
(`docker build` through the provider's ImageBuilder capability), and that
needs the buildx CLI plugin discoverable **together with** DOCKER_HOST:
`~/.docker/cli-plugins/docker-buildx` must exist, and when DOCKER_CONFIG is
set in the shell it must point at a directory carrying that same
`cli-plugins/docker-buildx` entry — one without the other fails the build
with either "docker buildx not found" or a daemon-mismatch error. CI
(linux/amd64 GitHub runners) uses a plain rootful Docker daemon and needs
neither workaround. These are environment constraints, not kind-ocp
defects — the errors surface unchanged from kind through the contextual
wrapping.

Long local runs (the integration suite, the StackRox example) are launched
detached (`nohup … > log 2>&1 &`) and polled in bounded foreground calls:
an interrupted tool call then loses nothing, and the log file is the
durable evidence.

## 7. Risks and mitigations

- Upstream pin drift breaks create or the CO proof → pins are defaults,
  overridable by config; bump procedure documented (README).
- kind library API instability → pinned version, thin `provider` boundary.
- Local-build webhook on create (increment 6) adds a docker build step →
  measured once and documented; if it proves fragile, the documented fallback
  is a digest-pinned third-party image with the patch logic tested by
  integration only (weaker AC-6.2 coverage — avoid).
- CI cost of the CO proof → single job, label-gated on PRs, nightly (§2).
- macOS quirks (inotify, sysctl) → warnings, never failures (BRIEF §9).
