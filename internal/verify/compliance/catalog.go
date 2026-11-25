package compliance

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The proof's catalog is committed content, not a registry artifact.
// Upstream's catalog — the digest the PoC pinned — floats at two
// levels: its bundle entry references compliance-operator-bundle:latest,
// and the ClusterServiceVersion that bundle carries pins none of its
// images (the operator, scanner, and content images all ride :master
// tags). A registry tag change could have changed which code OLM
// installed on a proved cluster with no change to this repo.
//
// The manifests under catalog/manifests are the pinned bundle image's
// /manifests content with every image reference rewritten to an
// immutable digest (tools/pin-compliance-catalog regenerates them —
// reading the registries through a pinned, digest-verifying crane —
// and verifies, before writing, that the bundle is byte-identical on
// linux/amd64 and linux/arm64 and that every image the proof depends
// on, the catalog server's pin and every reference the rewritten CSV
// carries included, is a manifest list carrying both platforms — so
// the catalog is platform-independent and the proof works unchanged
// on either architecture). This file assembles them into the
// file-based catalog
// the proof serves: the bundle entry embeds the manifests inline and
// carries no image reference at all, so OLM resolves the Subscription
// against exactly the bytes committed here and pulls nothing for the
// resolution that this repo does not pin. The workload images the CSV
// names are the only images the install pulls, each by digest.
//
//go:embed catalog/manifests/*.yaml
var catalogManifests embed.FS

// The names of the catalog-serving objects. Fixed, like every other
// name the proof owns: they are the proof's identity inside the
// cluster it proves.
const (
	// catalogServerName names the ConfigMap, Pod, and Service that
	// serve the catalog, and the CatalogSource points at the Service.
	catalogServerName = "compliance-operator-catalog"

	// catalogDataKey is the ConfigMap key the assembled catalog is
	// written to and the file the serving pod's volume materializes
	// (a ConfigMap volume materializes one file per key).
	catalogDataKey = "catalog.json"

	// catalogServerPort is the port opm serves the catalog on and the
	// Service forwards; it is OLM's own convention for catalog
	// servers.
	catalogServerPort = 50051

	// catalogMountPath is the directory the catalog is served from: an
	// emptyDir carrying exactly one file, the ConfigMap's catalog
	// mounted into it by subPath. A whole ConfigMap volume cannot be
	// served directly — its kubelet-managed layout (the ..data symlink
	// and the timestamped directory behind it) is not a plain
	// directory tree, and opm's walker reads both copies — so the
	// proof mounts the one key it carries as a single file into a
	// directory that contains nothing else.
	catalogMountPath = "/serving"

	// catalogChannel is the channel the proof's catalog publishes and
	// the proof's Subscription subscribes to: upstream's package
	// shape, carried over.
	catalogChannel = "alpha"

	// catalogSkipRangeFloor is the lower bound of the channel entry's
	// skipRange, verbatim from the upstream catalog — the oldest
	// version the entry lets a Subscription skip from. A fresh
	// cluster installs the channel head regardless.
	catalogSkipRangeFloor = ">=0.1.17"
)

// catalogAddress is the address the CatalogSource points at: the
// serving Service's cluster-DNS name, in the form OLM's own registry
// services use (<service>.<namespace>.svc:<port>), which the catalog
// operator dials in-cluster.
func catalogAddress() string {
	return fmt.Sprintf("%s.%s.svc:%d", catalogServerName, catalogNamespace, catalogServerPort)
}

// catalogDigestPattern matches an exact digest-pinned image reference
// (repository plus sha256 digest), the only shape the assembled
// catalog's CSV may carry.
var catalogDigestPattern = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// catalogCSV is the parsed shape of the committed bundle's
// ClusterServiceVersion: the identity and package metadata the catalog
// derives from it, and the image references whose digest-pinning the
// assembly enforces.
type catalogCSV struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Version                   string                   `yaml:"version"`
		RelatedImages             []catalogCSVRelatedImage `yaml:"relatedImages"`
		CustomResourceDefinitions struct {
			Owned []catalogCSVOwnedCRD `yaml:"owned"`
		} `yaml:"customresourcedefinitions"`
		Install struct {
			Spec struct {
				Deployments []struct {
					Spec struct {
						Template struct {
							Spec struct {
								Containers []struct {
									Image string `yaml:"image"`
									Env   []struct {
										Name  string `yaml:"name"`
										Value string `yaml:"value"`
									} `yaml:"env"`
								} `yaml:"containers"`
							} `yaml:"spec"`
						} `yaml:"template"`
					} `yaml:"spec"`
				} `yaml:"deployments"`
			} `yaml:"spec"`
		} `yaml:"install"`
	} `yaml:"spec"`
}

// catalogCSVOwnedCRD is one CustomResourceDefinition the CSV owns:
// the GVK the catalog's olm.gvk properties carry.
type catalogCSVOwnedCRD struct {
	Group   string `yaml:"group"`
	Kind    string `yaml:"kind"`
	Version string `yaml:"version"`
}

// catalogCSVRelatedImage is one named image reference the CSV carries.
type catalogCSVRelatedImage struct {
	Name  string `json:"name" yaml:"name"`
	Image string `json:"image" yaml:"image"`
}

// deploymentImages collects every image reference the CSV's install
// deployment carries: the container images themselves and the
// RELATED_IMAGE_* environment values the operator reads at runtime.
func (c catalogCSV) deploymentImages() []string {
	var images []string
	for _, d := range c.Spec.Install.Spec.Deployments {
		for _, container := range d.Spec.Template.Spec.Containers {
			images = append(images, container.Image)
			for _, env := range container.Env {
				if strings.HasPrefix(env.Name, "RELATED_IMAGE_") {
					images = append(images, env.Value)
				}
			}
		}
	}
	return images
}

// renderCatalog assembles the committed manifests into the file-based
// catalog the serving pod exposes: one olm.package, one olm.channel
// whose single entry is the bundle, and one olm.bundle that embeds
// every manifest inline (base64-encoded JSON, the property operator
// tooling reads bundle objects from) with the package and GVK
// properties OLM's resolver needs. The bundle entry carries no image:
// inline objects are what OLM installs, and the catalog needs no
// bundle image to exist anywhere.
//
// The output is deterministic — same manifests, same bytes — and is
// validated on the way out: the CSV must exist and name its version,
// and every image reference it carries (the deployment image, the
// RELATED_IMAGE_* environment values, and the relatedImages list) must
// be an exact digest reference. A regenerated manifest set that ships
// a floating reference fails the proof at assembly, before anything is
// applied to the cluster.
func renderCatalog() (string, error) {
	names, err := catalogManifestFiles()
	if err != nil {
		return "", err
	}
	csv, objects, err := decodeCatalogManifests(names)
	if err != nil {
		return "", err
	}

	// The properties OLM's resolver reads: the package identity with
	// the CSV's version, one GVK per owned API, and one inline object
	// per manifest file.
	properties := []catalogProperty{
		{Type: "olm.package", Value: map[string]any{
			"packageName": catalogPackageName,
			"version":     csv.Spec.Version,
		}},
	}
	for _, owned := range csv.Spec.CustomResourceDefinitions.Owned {
		properties = append(properties, catalogProperty{
			Type: "olm.gvk",
			Value: map[string]any{
				"group":   owned.Group,
				"kind":    owned.Kind,
				"version": owned.Version,
			},
		})
	}
	for _, name := range names {
		properties = append(properties, catalogProperty{
			Type:  "olm.bundle.object",
			Value: map[string]any{"data": base64.StdEncoding.EncodeToString(objects[name])},
		})
	}

	catalog := []catalogDocument{
		{
			Schema:         "olm.package",
			Name:           catalogPackageName,
			DefaultChannel: catalogChannel,
		},
		{
			Schema:  "olm.channel",
			Name:    catalogChannel,
			Package: catalogPackageName,
			Entries: []catalogChannelEntry{{
				Name:      csv.Metadata.Name,
				SkipRange: catalogSkipRangeFloor + " <" + csv.Spec.Version,
			}},
		},
		{
			Schema:        "olm.bundle",
			Name:          csv.Metadata.Name,
			Package:       catalogPackageName,
			Properties:    properties,
			RelatedImages: csv.Spec.RelatedImages,
		},
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	for _, doc := range catalog {
		if err := enc.Encode(doc); err != nil {
			return "", fmt.Errorf("encoding the catalog's %s document: %w", doc.Schema, err)
		}
	}
	return b.String(), nil
}

// catalogPackageName is the OLM package the proof's catalog publishes
// and its Subscription names.
const catalogPackageName = "compliance-operator"

// catalogManifestFiles returns the committed manifest file names in a
// stable order.
func catalogManifestFiles() ([]string, error) {
	entries, err := catalogManifests.ReadDir("catalog/manifests")
	if err != nil {
		return nil, fmt.Errorf("listing the committed catalog manifests: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		names = append(names, "catalog/manifests/"+entry.Name())
	}
	if len(names) == 0 {
		return nil, errors.New("the committed catalog carries no manifests")
	}
	sort.Strings(names)
	return names, nil
}

// decodeCatalogManifests reads every committed manifest as YAML,
// converts it to canonical JSON (the encoding the catalog embeds), and
// parses the bundle's single ClusterServiceVersion. Every file must
// decode; the CSV must exist exactly once; every image reference it
// carries must be digest-pinned.
func decodeCatalogManifests(names []string) (csv catalogCSV, objects map[string][]byte, err error) {
	objects = make(map[string][]byte, len(names))
	var csvRaw []byte
	for _, name := range names {
		raw, err := catalogManifests.ReadFile(name)
		if err != nil {
			return csv, nil, fmt.Errorf("reading the committed manifest %s: %w", name, err)
		}
		var doc catalogCSVHeader
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return csv, nil, fmt.Errorf("parsing the committed manifest %s: %w", name, err)
		}
		if doc.Kind == "ClusterServiceVersion" {
			if csvRaw != nil {
				return csv, nil, errors.New("the committed catalog carries more than one ClusterServiceVersion")
			}
			csvRaw = raw
		}
		var value any
		if err := yaml.Unmarshal(raw, &value); err != nil {
			return csv, nil, fmt.Errorf("parsing the committed manifest %s: %w", name, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return csv, nil, fmt.Errorf("encoding the committed manifest %s as JSON: %w", name, err)
		}
		objects[name] = encoded
	}
	if csvRaw == nil {
		return csv, nil, errors.New("the committed catalog carries no ClusterServiceVersion")
	}
	if err := yaml.Unmarshal(csvRaw, &csv); err != nil {
		return csv, nil, fmt.Errorf("parsing the committed ClusterServiceVersion: %w", err)
	}
	if csv.Metadata.Name == "" || csv.Spec.Version == "" {
		return csv, nil, errors.New("the committed ClusterServiceVersion carries no metadata.name or no spec.version")
	}
	if len(csv.Spec.CustomResourceDefinitions.Owned) == 0 {
		return csv, nil, errors.New("the committed ClusterServiceVersion owns no CustomResourceDefinitions")
	}
	if err := validateCatalogImages(csv); err != nil {
		return csv, nil, err
	}
	return csv, objects, nil
}

// catalogCSVHeader is the kind-bearing header every manifest is
// scanned with to find the CSV.
type catalogCSVHeader struct {
	Kind string `yaml:"kind"`
}

// validateCatalogImages refuses a CSV whose image references are not
// all exact digest references: the deployment container images, the
// RELATED_IMAGE_* environment values, and the relatedImages entries.
// This is the assembly-time half of the catalog's determinism
// contract; the committed-manifest tests pin the same invariant, and
// the tool that regenerates the manifests refuses to write a floating
// reference in the first place.
func validateCatalogImages(csv catalogCSV) error {
	var images []string
	for _, ri := range csv.Spec.RelatedImages {
		images = append(images, ri.Image)
	}
	images = append(images, csv.deploymentImages()...)
	if len(images) == 0 {
		return errors.New("the committed ClusterServiceVersion carries no image references at all — the shape the assembly validates has changed")
	}
	for _, image := range images {
		if !catalogDigestPattern.MatchString(image) {
			return fmt.Errorf("the committed ClusterServiceVersion carries the non-digest image reference %q — regenerate the catalog with a complete pin table (make pin-compliance-catalog)", image)
		}
	}
	return nil
}

// catalogProperty is one FBC property: a type and its JSON value.
type catalogProperty struct {
	Type  string         `json:"type"`
	Value map[string]any `json:"value"`
}

// catalogChannelEntry is one channel entry: the bundle it names and
// the version range it lets Subscriptions skip from.
type catalogChannelEntry struct {
	Name      string `json:"name"`
	SkipRange string `json:"skipRange,omitempty"`
}

// catalogDocument is one FBC document; the assembled catalog is the
// three of them concatenated, which is the format operator tooling
// (opm serve among them) reads.
type catalogDocument struct {
	Schema string `json:"schema"`

	// olm.package
	Name           string `json:"name,omitempty"`
	DefaultChannel string `json:"defaultChannel,omitempty"`

	// olm.channel
	Package string                `json:"package,omitempty"`
	Entries []catalogChannelEntry `json:"entries,omitempty"`

	// olm.bundle
	Properties    []catalogProperty        `json:"properties,omitempty"`
	RelatedImages []catalogCSVRelatedImage `json:"relatedImages,omitempty"`
}
