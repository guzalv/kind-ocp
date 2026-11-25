# kind-ocp — N OCP-like kind clusters for CI and local development.
#
# Recipes run the real toolchain; none of them mask exit status (no tee without
# pipefail — a bug carried over from the original PoC must not return).
GO ?= go
GOLANGCI_LINT ?= golangci-lint
BIN := bin/kind-ocp

.PHONY: build test lint integration generate regenerate pin-compliance-catalog all
all: build test lint

## build: compile the kind-ocp binary to $(BIN)
build:
	$(GO) build -o $(BIN) ./cmd/kind-ocp

## test: unit tests (no Docker required)
test:
	$(GO) test ./...

## lint: golangci-lint with the committed config
lint:
	$(GOLANGCI_LINT) run

## integration: Docker-gated integration tests (build tag "integration").
## -count=1 keeps the run real: the suite's outcome depends on Docker and
## cluster state outside the go test cache's key, so a cached "ok" is not
## evidence of a passing environment. -v keeps per-test progress visible:
## the suite runs tens of minutes and includes the Compliance Operator
## fitness proof, whose value is in what it printed along the way. The
## timeout covers the CO proof's image pulls (the CO catalog, operator,
## scanner, and content images, per cluster) on top of the cluster
## lifecycle the rest of the suite pays for.
integration:
	$(GO) test -tags integration -count=1 -v -timeout 60m ./test/integration/...

## generate: regenerate build-time generated files (the openshift/api pin
## the openshift-api-crds layer fetches; needs network for the commit lookup)
generate:
	$(GO) generate ./...

## regenerate: refresh the platform-objects layer's upstream-derived asset
## content (sccs.yaml, and two fields in objects.yaml) from the pinned
## upstream commits, and record the fetched bytes as the offline test
## fixtures (needs network; commit the result together with a pin bump)
regenerate:
	$(GO) run ./tools/regenerate

## pin-compliance-catalog: regenerate the CO fitness proof's committed
## catalog manifests (internal/verify/compliance/catalog/manifests)
## from the pinned bundle and image digests (needs network: the
## registries, and on first use the pinned crane's module download —
## the tool requests digests only, hashes every manifest it reads
## against its pin, pulls each platform's image by the concrete child
## digest the verified index names into a digest-verified local
## archive before unpacking it, and verifies that every pinned index
## advertises linux/amd64 and linux/arm64 entries — only the bundle's
## child manifests and layers are pulled — before writing anything;
## commit the result together with a pin bump)
pin-compliance-catalog:
	$(GO) run ./tools/pin-compliance-catalog
