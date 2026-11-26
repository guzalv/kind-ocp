# The proof's Compliance Operator catalog

These manifests are the catalog the CO fitness proof installs from.
They are not hand-authored and must not be edited by hand: they are
the `/manifests` content of a digest-pinned upstream bundle image,
with every image reference rewritten to an immutable digest, generated
by `make pin-compliance-catalog` (`tools/pin-compliance-catalog`).

The upstream catalog resolves its bundle through a floating `:latest`
tag and its CSV pins no images. OLM here resolves against exactly
these bytes: the proof embeds them in a file-based catalog served on
the pinned catalog image's `opm`; only CSV-named workload images are
pulled, each by digest.

Regeneration is platform-independent (manifests byte-identical across
`linux/amd64` and `linux/arm64`). The pin tool verifies every digest
itself, fetches through a pinned crane in an isolated credential
environment, and refuses to write on any mismatch — see
`tools/pin-compliance-catalog`.

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

To bump the Compliance Operator: resolve the new bundle digest and the
digests the CSV's floating references point at, edit the pins in the
tool, run `make pin-compliance-catalog`, commit the regenerated
manifests together with the pin change, and re-run the live proof (the
tagged integration test) on both default clusters.
