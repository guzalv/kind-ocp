# The proof's Compliance Operator catalog

These manifests are the catalog the CO fitness proof installs from.
They are not hand-authored and must not be edited by hand: they are
the `/manifests` content of a digest-pinned upstream bundle image,
with every image reference rewritten to an immutable digest, generated
by `make pin-compliance-catalog` (`tools/pin-compliance-catalog`).

The tool reads the registries through a pinned crane
(`github.com/google/go-containerregistry/cmd/crane@v0.21.5`, built
once per run into a private temporary directory so the version and
checksums are fixed and nothing is added to this repo's dependencies).
It requests digests only — no tag is ever resolved. Every index the
tool trusts is bound to its pin by the tool's own hash: the exact
bytes `crane manifest` printed are hashed against the requested
digest before they are parsed (the pinned crane substitutes the
registry's Docker-Content-Digest header for its own computation on
signed schema-1 responses, and prints what it fetched without a
media-type check). Each platform's image is then pulled by the
concrete child digest that verified index names — a direct pull
applies crane's schema-1 rejection to the fetched child, which the
--platform resolution path does not. Blobs are read in two steps so
the digest check cannot be skipped: `crane pull` first writes the
image to a local archive — its writer copies every compressed layer
through EOF, which is exactly when crane checks the blob digest —
and `crane export - -` then unpacks that archive from stdin without
contacting a registry again (exporting straight from the registry
would stop reading each layer at the tar end marker inside it, before
the checksum is checked).

The crane subprocess runs in an explicitly empty private credential
environment: a fresh HOME and XDG_RUNTIME_DIR, an empty Docker config
in a private DOCKER_CONFIG, and nothing inherited but the system PATH,
the temporary directory, proxy settings, and CA bundle locations. The
maintainer's registry credentials and custom Docker HTTP headers
cannot reach a registry's auth challenge, so a compromised registry
that names a token realm cannot be answered with one. Crane sends no
inherited registry credentials or Docker HTTP headers — it can still
obtain and send anonymous bearer tokens, and its requests travel
through any configured proxy, proxy credentials included. The tool
vouches for the manifests and layers its own hashes verify, not for
every byte fetched.

Why a committed catalog: the upstream catalog the PoC pinned resolves
its bundle through a floating `:latest` tag, and the bundle's
ClusterServiceVersion pins none of its images (the operator, scanner,
and content images all rode `:master` tags). A registry tag change
could have changed which code OLM installs on a proved cluster with no
change to this repo. Here, OLM resolves the Subscription against
exactly these bytes: the proof assembles them into a file-based
catalog whose bundle entry embeds the manifests inline (no bundle
image is pulled at all) and serves it on the pinned catalog-server
image's own `opm` binary. The only images the install pulls are the
workload images the CSV names, each by digest.

Current pins (recorded in `tools/pin-compliance-catalog/main.go`;
the tool verifies each before writing anything):

| What | Reference |
|---|---|
| Bundle (manifest source) | `ghcr.io/complianceascode/compliance-operator-bundle@sha256:7dcfa003c4a47886a24e46234fd4c4598100de769e61e6d949cbff3af230e78d` |
| Operator | `quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-dev@sha256:db7d55544de2621d0c5c423763b8cba1d41538e00307b9cf66358c0258d9939c` |
| OpenSCAP scanner | `quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-openscap-dev@sha256:f031084ff31a17f33c8e58a0566665d1c1325ed8928c26d679a0985165b32036` |
| Profile content | `quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-content-dev@sha256:d798f32ebb3e726e2f0231364e583d5d10485ac7d7396d5bdb34206c89a1f6e2` |
| Must-gather (annotation only) | `quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-must-gather-dev@sha256:32ee7fd9c5aeaba83d4bd81f51806bb6a9779cc30f1c0bfd384898d495f9db90` |
| Catalog server (`opm` binary) | `ghcr.io/complianceascode/compliance-operator-catalog@sha256:b5ad7b09f3fa12d99821b7768d6af54eba02e9328feb71c5aee7624401d5a3e8` (in `internal/verify/compliance`) |

Every image the proof depends on — the table above, the catalog
server's pin (read from the proof's own constant, so the two cannot
drift apart), and every reference the rewritten CSV carries — is
verified as a manifest list advertising `linux/amd64` and
`linux/arm64` entries (only the bundle's child manifests and layers
are pulled during regeneration), and the
bundle's manifests are byte-identical across the two platforms (the
tool refuses to write otherwise), so the catalog is
platform-independent.

To bump the Compliance Operator: resolve the new bundle digest and the
digests the CSV's floating references point at, edit the pins in the
tool, run `make pin-compliance-catalog`, commit the regenerated
manifests together with the pin change, and re-run the live proof (the
tagged integration test) on both default clusters.
