# kind-ocp — implementation plan

This file is the architecture and the increment record: what was built,
in what order, and the decisions that shaped it.

## 1. Objective

`kind-ocp` is a Go CLI and importable library that creates N (default 2)
OCP-like kind clusters: plain kind clusters plus a configurable set of
OpenShift layers. After `create` finishes, StackRox Central/Sensor and the
Compliance Operator deploy and work on every cluster. The repo creates
clusters; it deploys nothing.

## 2. Locked decisions

| Decision | Choice | Rationale |
|---|---|---|
| Language | Go 1.27, single `kind-ocp` binary plus importable packages | user decision |
| CLI framework | spf13/cobra | standard, subcommands + flags |
| Config format | YAML (`gopkg.in/yaml.v3`), loaded only via `--config` or `KIND_OCP_CONFIG` | explicit and deterministic; no CWD magic |
| Config precedence | flag > env > file > built-in default | AC-2.2, unit-tested |
| kind provisioning | **kind as a library** (`sigs.k8s.io/kind` v0.33.0, pinned in go.mod) behind `provider.Provisioner` | no kind binary on the host; version pinned by go.mod (AC-4); programmatic kubeconfig export into the state dir (no `~/.kube/config` pollution); programmatic node access for cross-cluster checks. Risk (less stable library API) is contained by the thin wrapper and the version pin. |
| k8s access | `k8s.io/client-go` typed clientset (nodes, deployments) + dynamic client with server-side apply for arbitrary manifests | layers apply CRDs and schema-less CRs; SSA is also a ported gotcha (OLM CRDs exceed the client-side annotation limit) |
| SCC-UID webhook | **Go implementation**; image built at `create` time via digest-pinned multi-stage `docker build` (golang builder → `FROM scratch`), loaded into each cluster the same way `kind load` does | patch logic is plain Go and unit-testable (AC-6.2 explicitly lists it); no python runtime in the cluster; no third-party webhook image at all, which closes the PoC's floating `python:3.12-slim` gap more strongly than digest-pinning a foreign image. The builder image is the pinned external input. Trade-off recorded in the layer docs. |
| openshift/api CRD sourcing (increment 4) | **Pinned runtime fetch over HTTPS**: the module's version/commit is embedded at build time from go.mod (a generated pin file — no committed CRD copies), and `create` fetches the 18 CRDs from that exact commit into a per-state-root cache with Go's net/http | AC-6.4 requires only Docker at runtime (no Go toolchain, no module cache on the host), and AC-3.4 requires go.mod to stay the single source of truth with no committed copies. Reading the module cache at runtime would tie the binary to a Go checkout; the pinned fetch keeps `go install` usable. Cluster creation already requires network (node-image pulls), so this adds no new environmental dependency. |
| Regeneration tool | ported to Go (`tools/regenerate`), fetches SHA-pinned upstream files over HTTPS, splices between `BEGIN/END UPSTREAM` markers | repo stays single-language; no python/PyYAML dependency |
| Module path | `github.com/guzalv/kind-ocp` | final |
| License | Apache-2.0 | org standard |
| CI cadence | lint + unit on every PR; integration job (create + verify + CO proof, ~10–20 min, one runner) behind an `integration` label, `workflow_dispatch`, and a nightly schedule on the default branch | fast PR feedback; nightly determinism signal for the pinned external inputs |
| Binary distribution | `go install` initially; goreleaser deferred | |

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
  `platform-objects` → `scc-admission`. Reasons: OLM-created deployments
  need the master label (node-labels before olm) — and the value must be
  the empty string, which the CSV nodeSelectors match exactly (a
  non-empty value schedules them nowhere); the real CRDs from
  openshift/api supersede the placeholder CRDs (config-openshift before
  openshift-api-crds) — the placeholders are create-only, never
  server-side applied over an existing CRD, so a failed real-CRD fetch
  cannot overwrite them; SCCs only validate against the real SCC CRD
  (openshift-api-crds before platform-objects). The scc-admission
  webhook scopes itself to its configured namespaces, which it creates,
  and depends on no other layer; it is last by convention.

  Layers that consume upstream releases never trust a tag: OLM release
  assets are mutable under a tag, so the release's manifests and the
  catalog-operator image are digest-verified or pinned via
  `configmapRegistryImage`; a user version override is trusted as pinned.

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
re-scope or otherwise re-configure a live client. Validation: count
within create's safety cap (`config.IsCreatableCount`, the same
classifier verify's count assertion uses for persisted expectations),
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
├── expected-clusters              # 0600 — the count the last create set out to build
├── cache/<layer>/<pin>/…          # 0600 — layers' fetched-content caches, keyed by content pin (increment 4+)
├── clusters/<name>/kubeconfig     # 0600 — cluster-admin credential
└── clusters/<name>/record.json    # name, context, profile, layers, node image, instance ID, created
```

The state root is created `0700` (kubeconfigs are cluster-admin
credentials), lazily on first write, and it is a verified boundary, not
just a
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
any cluster, so a refusal leaves everything in place. The marker is
written once, at root creation, and never rewritten: only its `tool`
field is interpreted, every other field is ignored whatever shape it
carries, and nothing kind-ocp writes later can invalidate it. The expected
cluster count lives in a separate private file beside it
(`expected-clusters`): the count create set out to build is persisted
there by atomic replacement, before the first cluster is provisioned,
so a failed count write leaves the previous expectation and the marker
untouched — ownership and count validate independently. The assertion
semantics, including legacy and corrupt-state handling, are §3.6's.

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
state directory. That closes the PoC's stale-state bug: a
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

Clusters 2..N are provisioned concurrently (per-cluster state isolation,
the PoC's parallel-deploy lesson); cluster 1 is sequential-first: its
create establishes the shared `kind` network and pulls the pinned node
image — the two cluster-scoped resources every later cluster depends
on — so the concurrent wave never races on either, and a cluster-1
failure stops the run before any other cluster starts. Before the
fan-out, create checks cancellation, then ensures the state root once
(the discipline `state.walk` prescribes for concurrent creation — and
the boundary check the host tune runs behind, so a run whose state
root is refused never changed host settings on its way to failing),
records the run's cluster count in the private expectation file beside
the state marker (§3.6), and only then asks the provisioner — only
when it implements the optional host-limit capability
(`provider.HostLimitRaiser`), which the kind runtime does — to raise
`fs.inotify.max_user_instances` best-effort (Linux only, warn on
refusal, note on other platforms; a provisioner without the capability
is never asked, so nothing but a real container runtime on this host
changes host settings, and a cancelled run asks for nothing at all).
The raise itself re-checks cancellation before any write: the direct
procfs write cannot carry the context, so the check is explicit, and a
run that gave up never writes. The wave runs to completion: every
cluster's failure is reported in one aggregate error, and no in-flight
layer application is cancelled out from under its record.

All clusters share kind's default `kind` Docker network (kind behavior), which
is what makes cross-cluster NodePort reachability work — asserted by verify's
cross-cluster check (§3.6, AC-1.4).

### 3.6 Verify model (AC-5.1)

`verify` runs, per cluster: global checks (all nodes Ready — zero nodes is a
FAIL; also cross-cluster NodePort reachability), the
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

The whole-prefix form (no names — the audit form) opens with a
prefix-scoped `cluster-count` line: every cluster the last create set
out to build, per the expectation create persisted beside the state
marker, must be in the inventory (the union of the provisioner's
clusters and the state records). Without it, a cluster whose container
and state record both vanished is invisible to `list` and to every
per-cluster check, and the survivors alone would report all-PASS. The assertion reads only the persisted expectation — never a
verify-time count resolution, so a prefix created with `--count 3`
verified by a plain `verify` that would resolve count 2 reports the
create's truth. It also runs before verify's empty-inventory refusal:
with every cluster vanished there is nothing for `list` to select, and
the refusal alone would hide the audit FAIL behind a structural error —
instead the count line FAILs naming everything missing and the results
are returned (the CLI exits non-zero on the FAIL); the structural
error stands only when nothing exists and the assertion could not FAIL
(a prefix create never wrote an expectation for, or one that cannot be
asserted). A state root without an expectation (older binary), a
corrupt count — an integer no create could have written, or a value
that is not an integer at all (a string, a fraction, an overflow;
`state.ErrCorruptCount`) — or an unreadable tree all degrade to a
SKIP with reason and remedy — never a false FAIL, never blocking the
clusters that exist. The expectation file's shape is deliberately
independent of the ownership marker: a corrupt count is reported by
the count line alone, and a marker that does not parse keeps refusing
the tree (the kubeconfig check FAILs), because ownership and the count
are validated separately. Corrupt expectations are classified against
create's own count bound (`config.IsCreatableCount`) before any name
is generated: a corrupt count can be arbitrarily large, and the
generated-name set is allocated to match the count, so a huge value
can neither be reported as dozens of phantom missing clusters nor
reach the sized allocation. Only a valid expectation with named
clusters missing from both the provisioner and state FAILs, naming the
clusters and the recovery: re-run the original create command with the
same prefix, configuration, and count — the FAIL names the expected
count — because re-running create with a smaller count is a supported
downscope that rewrites the expectation and can clear the failure
without restoring anything, but only when the smaller count excludes
every missing cluster: no count create can write excludes
`<prefix>-1`, so a missing first cluster fails under every downscope.
Extra prefix-scoped clusters beyond the
expectation are not a FAIL — leftovers from a larger count are a
supported state, and each gets its own lines. The named form runs no
count assertion: a targeted verify during recovery must not fail on a
sibling.

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

The probe's namespace ownership is proven, never assumed. The name is
unique to one preparation (a fresh random tail every run, so concurrent
verifies and killed-run leftovers never collide), and the namespace is
stamped with a per-attempt ownership marker — a create that errors is
not proof nothing was created (the server can persist the namespace
and lose the response), so an ambiguous create is settled by one
bounded marker read: a marked namespace is this attempt's and
registered for cleanup, an unmarked one is left entirely alone.
Before any write, the Service's endpoint is corroborated against the
provisioner's own view (`provider.ControlPlaneAPIEndpointResolver` —
the address and kind's internal API port the runtime reports for the
node), so a stale or tampered EndpointSlice cannot produce a PASS
that tested nothing. The mutation stays bound to the cluster instance
verify validated, and every child write re-reads and confirms the
namespace first. Deletions are UID-preconditioned (a replacement under
the same name is not the check's to remove) and confirmed by reading
under the cleanup's budget — a delete's return is a request, not a
result (finalizers hold objects past it). Cleanup covers partial
preparations exactly like completed ones, runs under its own bounded
context — a cancelled or expired check still removes what it created
— and folds its failures into the owning cluster's single line. Like
every layer check it leaves no state behind on every path the process
survives, and a namespace left by a killed run is never adopted or
removed — deleting the cluster removes such residue with everything
else. The pod-level half of AC-1.4 — a workload pod on A reaching
B's NodePort — is asserted by the integration test with a real client
pod, and end to end by the StackRox example (§5.1).

### 3.7 Errors and logging

`log/slog` text handler on stderr at info level (library consumers pass
their own `slog.Logger` through `kindocp.Options` and control verbosity
there; the CLI carries no verbosity flag until a concrete need exists).
kind's own progress is adapted onto the same logger with a verbosity
boundary: user-facing messages (kind level 0 and below) pass through, while
kind's debug/trace output — which includes the raw kubeadm config with the
bootstrap token and the kubeadm init/join output — is discarded, so default
logs never contain credential material. Loggers are derived per
cluster and per layer (`cluster=kind-ocp-2 layer=olm`). Errors wrap with
cluster, layer, and operation context (`fmt.Errorf` chains, no error-code
enum). No bare sleeps: waits are condition-based with timeouts (internal/kube
poll helpers — AC-6.5), and every wait bounds its API calls through the
deadline context.

### 3.8 Admission-webhook hardening

The scc-admission webhook is the most heavily hardened component in
the repo: it is an admission controller, and its Verify mutates the
cluster. Apply reads the stored MutatingWebhookConfiguration back
after server-side apply and fails the create on drift (SSA preserves
fields another field manager owns) — namespace allowlist, Pod CREATE
rules, no extra entries, no nonempty objectSelector, no
matchConditions — naming a scoped, POSIX-quoted recovery command.
Every client the probe uses refuses redirects and never embeds
redirect targets in its failures (a Location can carry credentials);
the probe's create is exactly-once (MaxRetries 0); the mutation is
bound to the cluster instance verify validated
(`Env.ClusterInstanceID`); the webhook Deployment's template and the
probe pod both set `automountServiceAccountToken: false`. Pinned by
unit regressions against fake apiservers, plus a real-API integration
regression for the SSA-merge semantics.

## 4. Porting map from PR #23841

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

Gotchas to preserve land with the code that needs them and are
asserted by verify checks: OLM CRDs via SSA; wait for CRD Established before
using kinds; CatalogSource in namespace `olm`; SCCs via create (or SSA), with
delete+recreate on re-runs; status writes through the status subresource;
master label on control-plane nodes; serving-cert secret before CO starts;
CSV channel-head fallback; per-cluster temp/state paths; per-wait deadlines;
exit-code fidelity in Make recipes.

## 5. Increment checklist

Each increment is one gated pass: implement → verify (tester) → review →
security → triage. Acceptance for every increment: `make build test lint`
green, unit tests need no Docker, integration evidence attached. All
eleven increments are delivered; the table records what each delivered
and where its behavior is pinned. Design rationale lives in §3 — each
row names the section that carries it — except for the two entries kept
as blocks below the table.

| # | Increment | Delivered | Pinned by |
|---|---|---|---|
| 1 | Scaffold and framework skeleton | Go module, Makefile (build/test/lint), lint config, LICENSE; CLI (`create`, `delete`, `list`, `kubeconfig`, `verify`); config system with precedence, per-cluster overrides, custom profiles, validation, pins as defaults; public layer framework with an empty builtin registry; kind-as-library provisioner behind `provider.Provisioner`; state root 0700/0600 with kubeconfig export and idempotent prefix-scoped delete; sequential `--count N` create with the failure contract (keep clusters, print the POSIX-quoted delete command); verify's node-Ready check; instance-ID-bound records. Restates §3.3–§3.5. | `config/`, `layers/`, `internal/state/`, `internal/cli/` unit tests |
| 2 | Kubernetes helpers + `olm` layer | `internal/kube`: REST config, dynamic-client SSA, CRD Established wait, rollout-complete wait, owned no-redirect HTTP fetch. `olm` layer: pinned release default, SSA for CRDs (annotation-limit gotcha), digest-verified release manifests, catalog-operator image pinned via `configmapRegistryImage`, floating `operatorhubio-catalog` CatalogSource dropped; verify checks both deployments Available and `operators.coreos.com/v1alpha1` served. Rationale: §3.2. | `internal/kube`, `internal/layers/olm` tests |
| 3 | `config-openshift` and `node-labels` layers | 13 placeholder CRDs + 3 singletons as embedded assets; Infrastructure status via the status subresource; master label with the empty-string value; create-only placeholder CRDs through the fixed CRD resource path. Rationale: §3.2. | `internal/layers/configopenshift`, `internal/layers/nodelabels`, `assets/` tests |
| 4 | `openshift-api-crds` layer | Build-time-generated pin file (commit, per-manifest SHA-256, size bound) kept in sync with go.mod by unit tests; runtime HTTPS fetch at the pinned commit into a per-state-root cache — digest-validated, bounded reads, atomic replacement; SSA + Established wait; verify checks all 18 Established and carrying the real root schemas. | `internal/layers/openshiftapicrds`, `tools/pin-openshift-api` tests |
| 5 | `platform-objects` layer + regeneration tool | `objects.yaml` and `sccs.yaml` as embedded assets (namespaces first, then singletons, SCCs with delete+recreate on re-runs); SCC stream validated against the pinned CRD's required fields; `kube.Client.Apply`/`ApplyStatus` take the field manager as a required argument. `tools/regenerate` (Go, SHA-pinned fetches): strict splice/marker rules — a BEGIN is closed by the very next marker line, missing/duplicate/nested/unmanaged pairs and wrong layouts are refused, the replacement is a byte-range edit that never normalizes untouched bytes, and the spliced file is validated against the pre-splice file as decoded objects. | `internal/layers/platformobjects` tests; `tools/regenerate` command-path regressions |
| 6 | `scc-admission` layer | Go webhook (concurrent server, generated TLS, configurable watched namespaces and injected UID); image built via a digest-pinned multi-stage docker build and loaded into each cluster; MutatingWebhookConfiguration scoped by namespaceSelector; failurePolicy `Ignore` (documented trade-off). Verify: a `runAsNonRoot: true` pod with no `runAsUser` in a watched namespace becomes Running with the injected UID. Hardening: §3.8. | `internal/layers/sccadmission` fake-apiserver unit regressions + SSA-merge integration regression |
| 7 | Multi-cluster + complete verify | Concurrent clusters 2..N with cluster 1 sequential-first; inotify raise behind `provider.HostLimitRaiser` (§3.5); verify complete — global checks, every enabled layer's check, PASS/FAIL/SKIP; cross-cluster NodePort check (§3.6). Integration tests: count=2 full profile and count=3. | `test/integration` |
| 8 | CO fitness proof + CI | `internal/verify/compliance` (semantics and gate behavior: §6); scc-admission image build moved behind `provider.ImageBuilder` — no layer invokes a container-runtime CLI directly; CI: lint + unit on every PR, integration behind the `integration` label, `workflow_dispatch`, nightly; actions SHA-pinned, workflow-level `contents: read`, no secrets. | `internal/verify/compliance` tests; `.github/workflows/ci.yaml` |
| 9 | StackRox example | `examples/stackrox` — recipe design and boundary rules: §5.1. | example unit tests (no Docker, no helm, no network) |
| 10 | Usability and honesty pass | Expected-vs-inventory count assertion (§3.6) with its regressions; example values-override flags and their refusal rules (§5.1); README honesty, prerequisites, recovery decision tree, performance budget (the README is the artifact; numbers in §6). | `kindocp` count-assertion regressions; `examples/stackrox` override regressions |
| 11 | Cold-machine findings | Refusal remedies: the ancestor policy's write-bit refusals name their chmod fix and are classifiable as `fscheck.ErrUnsafeAncestor`, and the state-root walk decorates that class with the `--state-root` / `XDG_STATE_HOME` alternative — one error carries both remedies (§5.3); hermetic test scratch: `internal/testscratch`, whose TestMain helper masks ambient umask write bits and re-points the scratch base under the system `/tmp` when the ambient chain is refused, wired into the seven affected packages; unselected-profile layer validation in `planClusters` — create-only by construction, a deliberate tightening (release-notes: a config with a typo'd unselected profile that previously created fine now fails fast); README: umask-002 prerequisite warning, scc-shortname limitation, disk-footprint totals and small-disk warning; maintenance doc: lint-toolchain requirement. | `internal/fscheck`, `internal/state`, `kindocp`, `internal/cli` regressions; `internal/testscratch` tests; the §5.3 umask-002 proof run |

### 5.1 StackRox example (increment 9)

`examples/stackrox`: Central on cluster 1, Sensors on 2..N over NodePort
on the shared kind network, released public images, collector disabled.
A `package main` (structurally unimportable — the create path can never
reach it) using kind-ocp's public surface plus standard client-go the
way an outside consumer would, with one deliberate exception: its
credential workspace is verified through kind-ocp's internal filesystem
boundary (`internal/fscheck`), the same policy the state tree walks,
reused rather than forked. Beyond kind-ocp's own requirements it needs
only the helm CLI.

The two StackRox chart tarballs (central-services and
secured-cluster-services 500.0.0, appVersion 5.0.0) are fetched once
per run from charts.stackrox.io, verified against recorded SHA-256
digests before a bounded, traversal-refusing extraction.
Central deploys with NodePort exposure, scanner and telemetry off, and
resource requests shrunk to kind scale. Sensors register with a
cluster registration secret (CRS — init bundles are deprecated in
Central 5.0) generated through Central's REST API over a client-go
port-forward. The CRS name is deterministic, so a re-run converges: a
409 from the generate call lists the same-named CRS and revokes it by
id — only when exactly one entry holds the name — then regenerates.
Revocation removes only the bootstrap credential: Sensors that
already registered keep their issued identities, and the helm upgrade
that follows re-applies the fresh manifest everywhere. The readiness gate is Central reporting every
Sensor cluster registered and HEALTHY — the end-to-end AC-1.4 proof.
Secrets (admin password, CRS) live in 0600 files under a 0700 work
directory, passed to helm via `-f`, never through argv, never logged.

The workspace is a per-prefix sibling of the state root, derived from
the *resolved* state base and opened through the same `WalkDirNames`
walk the store uses — disjoint from the state tree by construction,
with no path comparison left to fool: on a case-insensitive volume
(macOS, a supported platform) two spellings that differ by case are
one directory while remaining two strings, exactly the shape a lexical
overlap check passes while pointing credentials into the tree
`kind-ocp delete` removes. Override containment is by directory
identity (`os.Stat` + `os.SameFile` over the override's ancestors)
for the same reason. The Central client authenticates Central before
any credential is sent: the TLS peer must present a certificate
chaining to the CA the chart's install hook wrote into the
`central-tls` secret, verified as `central.stackrox.svc`; redirect
answers and non-https base URLs are refused outright. The
port-forward binds 127.0.0.1 only, local port chosen by client-go
(port 0 — no listen-close-use window), under a single cause-carrying
cancel context that is the forward's stop signal, every API call's
lifetime, and the thing the goroutine cancels the moment
`ForwardPorts` returns — the tunnel's own failure is published as the
context's cause.

Values overrides (`--central-values`, `--sensor-values`) overlay the
generated values, one `-f` each, in order. A user-supplied path is
resolved to its absolute checked form before helm sees it (the
workspace's ancestor policy, private leaf): helm v4.3.1 reads a bare
`-` as stdin and parses each `-f` operand as a CSV record — a comma
splits it into several files, a quote breaks the parse, a newline or
CR ends the record early (verified live) — so operands containing a
comma, double quote, newline, or carriage return are refused, and
only the absolute path is handed on. Overrides must be single-link
files (hard-linked overrides are refused — in-place writers elsewhere
in the tree would clobber them) and are checked private/caller-owned
like all credential files. helm's own stdout/stderr are discarded
(its diagnostics quote values content, which carries the admin
password); a failed run reports the exit status and the full
shell-quoted command line.

### 5.2 Usability and honesty pass (increment 10)

The code delivery is the expected-vs-inventory count assertion (§3.6)
and its Docker-free regressions in `kindocp`: vanished-cluster FAIL
naming cluster and remedy, drift (count-3 create, count-2 verify →
PASS), the corrupt/legacy SKIP table, atomic replacement and
concurrent updates of the expectation file, marker untouched by count
writes. The documentation work — the "What is proven" separation, the
real prerequisites and network endpoints, the recovery decision tree,
the performance and resource budget (§6) — lives in the README, which
is the artifact. The example gained the values-override flags (§5.1);
their refusal rules (operands with comma/quote/newline/CR, overrides
inside the workspace or the run's state tree, hard-linked files,
case-colliding spellings) are pinned by regressions in
`examples/stackrox`.

### 5.3 Cold-machine findings (increment 11)

Findings from the first clean-machine install test (v0.1.0, Ubuntu,
umask 002 — the Debian usergroups default). The state-tree privacy
policy is correct by design and stays; the gap is that its refusals
meet the most common Linux default with no remedy offered.

- **State root on umask-002 distros (the defect)**: `~/.local` exists
  0775 (created by other tooling under umask 002), so the default state
  root `~/.local/state/kind-ocp` is refused: "is group-writable". A
  fresh machine is fine — the walk creates a missing `~/.local` as
  0700 — but the refusal must carry its remedy (chmod g-w the
  offending path, or point --state-root / XDG_STATE_HOME elsewhere),
  and the README prerequisites must warn umask-002 users before they
  hit it.
- **Test hermeticity, same root cause**: 7 of 23 unit-test packages
  (internal/cli, internal/state, internal/fscheck,
  internal/provider/kind, kindocp, test/publicapi, examples/stackrox)
  fail when the ambient TMPDIR chain contains a user-owned
  group-writable non-sticky directory. The suites must be hermetic
  against the ambient chain: a shared test helper that, when the
  TMPDIR chain fails the ancestor policy, re-points the package's
  scratch base to a 0700 directory directly under the system /tmp
  (sticky — the policy deliberately tolerates it). Prove with the full
  suite run under umask 002 with a group-writable TMPDIR.
- **`kubectl get scc` shortname**: the pinned upstream SCC CRD
  declares no shortNames, and adding one would diverge from the
  digest-verified bytes. Document in the README (use the full
  `securitycontextconstraints`); no code change.
- **Unselected-profile validation**: a custom profile in the config
  file whose layer list names an unknown layer errors only when that
  profile is selected (`layers.LayerNamesForProfile`). Validate every
  defined profile's layer names in the create path, so a typo in an
  unselected profile fails fast. Regressions for selected and
  unselected shapes.
- **lint toolchain**: golangci-lint binaries built with go1.25 cannot
  lint a go1.27.4 module. Document in docs/maintenance.md: install
  golangci-lint built with go1.27.4.
- **Disk footprint**: the README performance section states per-cluster
  numbers but no total and no small-disk warning. Add both: a default
  2-cluster run costs ~5 GB of images plus ~1.5 GB per cluster in
  volumes, and a small disk caps the practical cluster count well
  below the 64 limit.

Delivered as increment 11. The refusal remedies and the validations
landed as specified above. One mechanism detail the findings did not
spell out: re-pointing the scratch base alone is not enough, because
the testing package creates `t.TempDir`'s per-test subdirectories with
mode 0777 — they are umask-subject, so under umask 002 they come out
0775 (user-owned, group-writable, non-sticky) wherever the base lives.
The delivered helper (`internal/testscratch`, wired into the seven
packages' TestMain through `testscratch.Main`) therefore does two
things: it adds the group- and other-write bits to the test binary's
umask (preserving stricter ambient masks, never loosening them), and
it re-points `GOTMPDIR`/`TMPDIR` to a fresh 0700 directory directly
under the system `/tmp` when the ambient chain fails the ancestor
policy. Neither touches what the boundary refuses: tests that chmod a
directory into an unsafe shape keep failing as designed, because chmod
applies exactly the requested mode. Proof (the §6 manual-evidence
convention): the seven packages fail before the fix and the full
suite passes after it, run under `umask 002` with `TMPDIR` pointed at
a user-owned group-writable non-sticky directory.

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
- CO fitness proof (`internal/verify/compliance`, increment 8): a
  deployment proof, never a verify check — an import-boundary test
  fails the build on any import of it outside itself and the tagged
  integration test, and it is one-shot on a fresh cluster (a
  serving-cert marker refuses a second run). The catalog it installs
  from is committed in the repo; `make pin-compliance-catalog`
  re-derives it from the pinned bundle and image digests through a
  pinned, externally-run crane. Measured cold, the full uncached
  suite (including the proof's per-cluster image pulls) runs in
  ~17–18 minutes against the Makefile's 60m go test budget.
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

### 6.2 Measured performance and resource budget

On the development machine (Docker Desktop 28.1.1): create 105 s cold,
85 s warm, 8 s reconcile; delete ~1 s; verify 17 s; 58 s for a minimal
cluster pair and 89 s for three full clusters; ~17 min for the cold
integration suite. Per-cluster idle budget: ~1 GiB RAM, 15–20 % of one
CPU per node container, ~1.5 GB volume + ~4 MB layer, node image
shared once per daemon. The hard count cap is 64 (enforced by
create); there is no concurrency limiter after the first cluster (a
deliberate scope cut).

## 7. Risks and mitigations

- Upstream pin drift breaks create or the CO proof → pins are defaults,
  overridable by config; bump procedure documented (README).
- kind library API instability → pinned version, thin `provider` boundary.
- Local-build webhook on create (increment 6) adds a docker build step →
  measured once and documented; if it proves fragile, the documented fallback
  is a digest-pinned third-party image with the patch logic tested by
  integration only (weaker AC-6.2 coverage — avoid).
- CI cost of the CO proof → single job, label-gated on PRs, nightly (§2).
- macOS quirks (inotify, sysctl) → warnings, never failures.
