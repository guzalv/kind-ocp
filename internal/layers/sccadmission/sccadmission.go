// Package sccadmission implements the scc-admission layer: a mutating
// admission webhook that gives a plain kind cluster the one OpenShift
// SCC behavior the Compliance Operator's scanner pods depend on — a
// runAsUser assigned to pods that set runAsNonRoot without a UID.
// OpenShift's SecurityContextConstraints admission does this inside the
// apiserver; kind has no SCCs, so the kubelet rejects such pods
// ("container has runAsNonRoot and image will run as root") and the
// Compliance Operator's profileparser and scanner pods never run.
//
// Ported from the PoC's install_scc_webhook and its python webhook
// (stackrox PR #23841), with the PoC's gaps closed:
//
//   - The webhook is Go (assets/scc-admission), served by net/http, so
//     admission calls are handled concurrently — the PoC's python
//     HTTPServer answered one request at a time.
//   - The image is built at create time by a digest-pinned multi-stage
//     build (golang builder → FROM scratch) from the embedded source —
//     through the provider's image-building capability, so no layer
//     invokes a container-runtime CLI directly — then loaded into the
//     cluster's nodes the same way `kind load docker-image` does. The
//     PoC ran a floating python:3.12-slim image with the webhook source
//     in a ConfigMap; here the builder image is the only pinned
//     external input (DefaultBuilderImage) and the built image carries
//     no third-party code at all.
//   - The TLS material (a private CA and the serving certificate) is
//     generated in Go on the host — no openssl, no ConfigMap-shipped
//     source — and carried by a kubernetes.io/tls Secret.
//   - The MutatingWebhookConfiguration selects namespaces by the
//     built-in kubernetes.io/metadata.name label against the
//     configured watchedNamespaces list, so the effective scope is
//     exactly that list. Apply creates each configured watched
//     namespace if absent, so the namespaces the Compliance Operator
//     will use are watched before anything deploys into them; a
//     namespace that leaves the list is left in place, merely
//     unwatched.
//   - On a re-run a still-valid TLS secret is kept, which fixes a
//     latent PoC bug: it regenerated the certificate on every run, so
//     the running webhook kept serving the old key while the
//     configuration bundled the new CA. Only a missing or unusable
//     secret is regenerated. The Deployment's pod template carries a
//     SHA-256 fingerprint of the secret's material, re-derived from
//     the secret on every Apply, so a changed secret — regenerated
//     here, or written by a run that died before it could roll —
//     changes the template and the Deployment controller rolls the
//     webhook's pods onto it.
//
// failurePolicy is Ignore, as in the PoC, and the trade-off is
// deliberate: with Ignore, a webhook that is down or unreachable
// admits pods unmutated, and the kubelet then rejects the
// runAsNonRoot-without-UID pods with a CreateContainerError that names
// the cause — the failure surfaces at the pod. With Fail the same
// outage would block every pod creation in the watched namespaces,
// including during the layer's own rollout, when the webhook Deployment
// is legitimately still starting. On a disposable cluster whose purpose
// is OpenShift emulation, Ignore fails small and self-recovers; the
// layer's Verify probe catches a broken webhook explicitly instead.
//
// Both Apply and Verify end in a live probe: pods with runAsNonRoot
// and no runAsUser in each watched namespace are created until one
// comes back from the API carrying the injected UID — the only
// observable of the apiserver having loaded the
// MutatingWebhookConfiguration, which lands on a delay of its own —
// and that pod must then reach Running, the kubelet's acceptance of
// the injected UID. For Apply the probe is the gate that a converged
// cluster actually mutates; for Verify it is the plan's check: the
// probe pod is Verify's one intentional, transient mutation, without
// which the check would prove nothing. Both run that probe only after
// proving the MutatingWebhookConfiguration is exactly the layer's own
// shape — the namespace allowlist, the Pod CREATE rules, and no extra
// entries — because a probe that passes in the watched namespaces says
// nothing about what a drifted registration mutates elsewhere. Verify
// proves it from the stored object; Apply proves it by reading the
// stored registration back after its apply, because server-side apply
// preserves the fields another field manager owns — a separately
// managed matchCondition or objectSelector survives the apply while
// the probe still passes — so drift the apply could not converge
// fails the create instead of reporting success. And when the
// environment carries a cluster instance binding (kindocp's verify
// path does), Verify runs its probe only against the instance the
// embedding validated, so a kubeconfig replaced mid-check cannot turn
// the probe into a mutation of an unverified cluster. The probe pod is
// the webhook's own image (started with an argument, it idles). On
// success the probe leaves no pod behind: every pod it created — and
// any it may have created through a create whose outcome was unknown,
// adopted by the per-attempt token stamped on it — is deleted with the
// deletion confirmed by a read that no longer finds it. The probe's
// own create is exactly one wire POST — client-go's automatic retries
// are disabled for that one request, and its HTTP client refuses
// redirects, which Go would otherwise follow by re-sending the POST —
// so an attempt is never silently re-sent behind the resolution of
// the first. On failure a probe pod may remain: a create whose
// outcome could not be determined, or a cleanup that could not
// finish; the failure's diagnostic names the pod's namespace and
// name together with the attempt's ownership token, so the pod can be
// identified as the probe's — a matching name or the static probe
// label alone is not proof — and removed by hand once confirmed. The
// probe runs under a deadline context derived once from its budget, so
// every call it makes is bounded, and probe and cleanup failures are
// reported together.
//
// The layer's one client set is built from one loaded kubeconfig — a
// client bundle (kube.NewNoRedirectClientFromConfig) whose single
// shared transport refuses redirects. It serves everything the layer
// and its probe issue: the applies, the reads and deletes, and the
// probe's exactly-once create, which borrows the clientset's own
// core/v1 REST client. A kubeconfig rewritten between two loads (a
// same-name cluster replacement) cannot point the probe's POST at the
// replacement while its reads, its ambiguity-resolution reads, and its
// UID-preconditioned cleanup stay on the recorded cluster.
//
// Settings (config file, `layers.scc-admission` section; all optional,
// overlaying these defaults):
//
//	watchedNamespaces: [openshift-compliance]  # created if absent; the admission scope
//	runAsUser: 1001                            # the injected non-root UID
//	builderImage: <digest-pinned golang image> # the image build's builder stage
//
// The builderImage override must itself be one valid image reference
// carrying an exact sha256 digest: the build runs with
// cluster-admin reachability at create time, the value is interpolated
// into the Dockerfile, and an unpinned or malformed builder would make
// every cluster's admission webhook depend on — or inject — whatever
// the string carries.
package sccadmission

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/internal/shell"
	"github.com/stackrox/kind-ocp/layers"
)

// Name is the layer's stable identifier, as referenced by profiles and
// the config file.
const Name = "scc-admission"

// settings is the layer's resolved configuration. DecodeSettings
// overlays the config file onto defaultSettings, so only explicitly
// set keys override the defaults.
type settings struct {
	WatchedNamespaces []string `yaml:"watchedNamespaces"`
	RunAsUser         int64    `yaml:"runAsUser"`
	BuilderImage      string   `yaml:"builderImage"`
}

func defaultSettings() settings {
	return settings{
		WatchedNamespaces: []string{defaultWatchedNamespace},
		RunAsUser:         defaultRunAsUser,
		BuilderImage:      DefaultBuilderImage,
	}
}

func decodeSettings(env layers.Env) (settings, error) {
	s := defaultSettings()
	if err := env.DecodeSettings(Name, &s); err != nil {
		return settings{}, err
	}
	if len(s.WatchedNamespaces) == 0 {
		return settings{}, errors.New("watchedNamespaces must list at least one namespace")
	}
	for _, ns := range s.WatchedNamespaces {
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return settings{}, fmt.Errorf("watchedNamespaces: %q is not a valid namespace name: %s", ns, strings.Join(errs, "; "))
		}
	}
	if s.RunAsUser <= 0 {
		return settings{}, fmt.Errorf("runAsUser must be a positive (non-root) UID, got %d", s.RunAsUser)
	}
	if err := validateBuilderImage(s.BuilderImage); err != nil {
		return settings{}, err
	}
	return s, nil
}

// Wait budgets. Fixed constants, not settings: AC-3.1 requires the
// waits to be bounded, and no documented use case overrides a budget.
const (
	// rolloutTimeout bounds the webhook Deployment's rollout wait in
	// Apply, matching the olm layer's budget: the first rollout
	// includes the docker build's image pull.
	rolloutTimeout = 10 * time.Minute

	// probeTimeout bounds one probe pod's wait for Running and, in
	// Verify, each read. In Apply it also bounds the one read that
	// proves the stored registration converged. The image is already
	// on the node and the container is the webhook binary; two minutes
	// is generous.
	probeTimeout = 2 * time.Minute
)

// Layer installs the SCC-UID mutating webhook. It is stateless;
// construct once.
type Layer struct{}

// Name returns "scc-admission".
func (Layer) Name() string { return Name }

// Apply converges the cluster to the layer's desired state: build the
// webhook image from the embedded source (digest-pinned builder), load
// it into the cluster, create the watched namespaces, ensure the TLS
// secret, apply the webhook's Deployment, Service, and
// MutatingWebhookConfiguration, read the stored registration back and
// fail on any drift the apply could not converge, wait for the rollout,
// then prove the admission path live with a probe pod in every watched
// namespace. Re-running converges: the image ref is derived from the
// build inputs, a still-valid TLS secret is kept, and the applies are
// no-ops against identical content.
func (l Layer) Apply(ctx context.Context, env layers.Env) error {
	s, err := decodeSettings(env)
	if err != nil {
		return err
	}
	if env.BuildImage == nil {
		return errors.New("no image-building service: this embedding cannot build images (kindocp binds one for create; Verify environments never do)")
	}
	if env.LoadImage == nil {
		return errors.New("no image-loading service: this embedding cannot load images into the cluster (kindocp binds one for create; Verify environments never do)")
	}
	// One load of the kubeconfig builds the one client set this layer
	// uses — applies, reads, waits, and the probe's exactly-once create
	// all go through it, and its shared transport refuses redirects. A
	// kubeconfig rewritten between two loads (a same-name cluster
	// replacement) would otherwise point the probe's POST at the
	// replacement while its reads, its ambiguity-resolution reads, and
	// its UID-preconditioned cleanup stayed on the recorded cluster.
	cfg, err := kube.RESTConfig(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("loading the kubeconfig: %w", err)
	}
	client, err := kube.NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	ref := imageRef(s)
	// The build itself goes through the env's image-building service —
	// the provider capability (provider.ImageBuilder) the framework
	// binds when the runtime can build images. The layer renders its
	// own context (the embedded source and the pinned builder) into a
	// private directory and hands it over; which container runtime
	// turns it into the image is the provider's business, never the
	// layer's (a podman provider is a drop-in replacement for this
	// call).
	dir, err := materializeContext(s)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	started := time.Now()
	env.Log.Info("building the scc-admission webhook image", "ref", ref, "builder", s.BuilderImage)
	if err := env.BuildImage(ctx, ref, dir); err != nil {
		return fmt.Errorf("building the scc-admission webhook image: %w", err)
	}
	env.Log.Info("built the scc-admission webhook image", "ref", ref, "elapsed", time.Since(started).Round(time.Millisecond))
	if err := env.LoadImage(ctx, ref); err != nil {
		return fmt.Errorf("loading the webhook image into cluster %s: %w", env.ClusterName, err)
	}
	return l.converge(ctx, env, client, s, ref, probeTimeout)
}

// converge is the cluster side of Apply — everything after the image
// is built and loaded. It is separate so the unit tests can drive it
// against a fake apiserver without Docker: the image build and the
// image load are the only parts of Apply that touch the host. The
// probe budget is a parameter, like Verify's read budget, so the
// regression tests can drive it short. client is the layer's one
// client set, the no-redirect bundle built from the one load: every
// request of the apply, its probe included, resolves exactly the
// cluster that one load resolved.
func (l Layer) converge(ctx context.Context, env layers.Env, client *kube.Client, s settings, ref string, probeBudget time.Duration) error {
	manager := kube.LayerFieldManager(Name)
	if err := client.Apply(ctx, manager, namespaceObjects(s)); err != nil {
		return fmt.Errorf("applying the scc-admission namespaces: %w", err)
	}
	data, err := ensureSecret(ctx, env.Log, client.Clientset)
	if err != nil {
		return err
	}
	objs := []*unstructured.Unstructured{
		deployment(s, ref, tlsFingerprint(data)),
		service(),
		mutatingWebhookConfiguration(s, data[secretCAKey]),
	}
	if err := client.Apply(ctx, manager, objs); err != nil {
		return fmt.Errorf("applying the scc-admission objects: %w", err)
	}
	// The registration, as stored, must be exactly the shape the layer
	// renders before anything probes it. Server-side apply writes every
	// field the layer's manifest carries but preserves the fields
	// another field manager owns — a separately managed matchCondition
	// or objectSelector is absent from the manifest and survives every
	// re-run — so a positive probe would say nothing about the
	// registration's reach while the create reported success and Verify
	// failed after it. The drift is read back through the layer's one
	// client set, within the same budget the probe runs with, and it
	// fails the create here, before the rollout wait and the probe.
	if err := ensureRegistrationConverged(ctx, client.Clientset, env, s, probeBudget); err != nil {
		return err
	}
	// The deployment's pod template carries the fingerprint of the TLS
	// material just applied, so a changed secret rolls the pods through
	// the Deployment controller — including a secret a previous run
	// wrote but never rolled onto. The rollout wait below covers that.
	if err := kube.WaitForDeploymentRolledOut(ctx, client.Clientset, webhookNamespace, webhookDeployment, rolloutTimeout); err != nil {
		return err
	}
	probe := client.Clientset
	for _, ns := range s.WatchedNamespaces {
		if err := runProbe(ctx, probe, s.RunAsUser, ref, ns, probeBudget); err != nil {
			return err
		}
	}
	return nil
}

// ensureRegistrationConverged reads the MutatingWebhookConfiguration
// back after the layer's apply and fails when the stored registration
// is not exactly the shape the layer renders
// (registrationDriftProblems). Server-side apply preserves the fields
// another field manager owns: a registration narrowed by someone
// else's matchCondition or objectSelector — fields the layer's
// manifest does not carry — survives the apply untouched while the
// probe still passes, because the pods the probe creates are the
// shapes the drifted registration admits. The read is one typed GET
// on the client set the layer already uses, bounded by the same
// budget the probe runs with. When drift remains, the error names it
// and the recovery, and the recovery is scoped to this cluster: the
// layer never removes the fields itself, because deleting another
// manager's work silently is not the layer's call to make — and a
// delete command printed without the scoping runs against whatever
// the operator's ambient KUBECONFIG points at, which with several
// clusters under one prefix is how a healthy cluster loses its
// registration while the drifted one keeps the foreign field.
func ensureRegistrationConverged(ctx context.Context, cs kubernetes.Interface, env layers.Env, s settings, readBudget time.Duration) error {
	readCtx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()
	mwc, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(readCtx, webhookConfig, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading back the %s MutatingWebhookConfiguration the apply just wrote: %w", webhookConfig, err)
	}
	drift := registrationDriftProblems(mwc, s)
	if len(drift) == 0 {
		return nil
	}
	return fmt.Errorf(
		"the stored %s MutatingWebhookConfiguration on cluster %s is not the registration the layer applies: %s — "+
			"server-side apply preserves the fields another field manager owns, and the layer never removes fields it does not own; "+
			"to recover, remove the foreign fields or delete the registration through this cluster's kubeconfig — kubectl --kubeconfig %s delete mutatingwebhookconfiguration %s — "+
			"never through the ambient KUBECONFIG, which can point at another cluster under the same prefix and would delete the wrong registration — "+
			"then re-run create, which re-creates it in the layer's shape",
		webhookConfig, env.ClusterName, strings.Join(drift, "; "), shell.Quote(env.KubeconfigPath), webhookConfig)
}

// ensureSecret converges the webhook's TLS secret, returning the
// secret's effective data — kept or freshly written. A secret that
// parses, matches its key, chains to its CA for the service's DNS
// names, and is outside the renewal margin is kept as-is. Anything
// else is regenerated; the caller's deployment template then carries
// the new material's fingerprint, which rolls the webhook's pods.
func ensureSecret(ctx context.Context, log *slog.Logger, cs kubernetes.Interface) (data map[string][]byte, err error) {
	secrets := cs.CoreV1().Secrets(webhookNamespace)
	existing, getErr := secrets.Get(ctx, webhookSecret, metav1.GetOptions{})
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return nil, fmt.Errorf("reading the %s/%s secret: %w", webhookNamespace, webhookSecret, getErr)
	}
	if getErr == nil {
		if validationErr := validateSecretData(existing.Data, time.Now()); validationErr != nil {
			log.Info("regenerating the webhook TLS secret", "reason", validationErr.Error())
		} else {
			return existing.Data, nil
		}
	}

	material, err := generateTLS(time.Now())
	if err != nil {
		return nil, fmt.Errorf("generating the webhook's TLS material: %w", err)
	}
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: webhookSecret, Namespace: webhookNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			secretCAKey:             material.caCertPEM,
			corev1.TLSCertKey:       material.tlsCertPEM,
			corev1.TLSPrivateKeyKey: material.tlsKeyPEM,
		},
	}
	if getErr == nil {
		secret.ResourceVersion = existing.ResourceVersion
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return nil, fmt.Errorf("updating the %s/%s secret: %w", webhookNamespace, webhookSecret, err)
		}
	} else {
		if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating the %s/%s secret: %w", webhookNamespace, webhookSecret, err)
		}
	}
	return secret.Data, nil
}

// Verify checks the layer's postconditions: the webhook Deployment
// Available, the TLS secret usable, the MutatingWebhookConfiguration's
// caBundle matching the secret's CA and its scope exactly the layer's
// configuration — the namespace allowlist, no objectSelector or
// matchConditions, the Pod CREATE rules, and no extra webhook entries
// — and, the live proof, a probe pod with
// runAsNonRoot and no runAsUser in each watched namespace coming back
// mutated with the configured UID and reaching Running.
// The probe pod is Verify's one transient mutation (created and
// deleted within the check); without it the check would prove nothing
// about the admission path itself. When the environment carries a
// cluster instance binding, the check refuses to run that mutation
// against any other instance.
func (l Layer) Verify(ctx context.Context, env layers.Env) error {
	return l.verify(ctx, env, probeTimeout)
}

// verify is Verify with an explicit budget. The budget bounds the
// reads; each probe derives its own budget of the same size, so one
// failing namespace cannot starve the next. Verify passes the
// production bound (probeTimeout); the bounded-read regression test
// drives this helper with a short budget, which is why the budget is a
// parameter and not a setting.
func (l Layer) verify(ctx context.Context, env layers.Env, budget time.Duration) error {
	s, err := decodeSettings(env)
	if err != nil {
		return err
	}
	// One load of the kubeconfig builds the one client set this check
	// uses — the same one-load, no-redirect rule as Apply: a kubeconfig
	// rewritten between two loads cannot split the check's reads from
	// its probe's POST.
	cfg, err := kube.RESTConfig(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("loading the kubeconfig: %w", err)
	}
	client, err := kube.NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	readCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	// The mutating part of this check is bound to the cluster instance
	// the embedding validated. When the environment carries that
	// binding, the cluster under this kubeconfig must still be it: a
	// kubeconfig replaced after the embedding's identity check — the
	// same-name replacement — would turn every probe pod into a
	// mutation of an unverified cluster, and a NotFound on the recorded
	// cluster proves nothing about what the replacement received. The
	// identity is read through the same one loaded config, so the
	// binding and the probe cannot disagree about which cluster they
	// describe.
	if env.ClusterInstanceID != "" {
		live, err := kube.ClusterInstanceID(readCtx, client.Clientset)
		if err != nil {
			return fmt.Errorf("binding the check to the cluster instance: %w", err)
		}
		if live != env.ClusterInstanceID {
			return fmt.Errorf("the kubeconfig points at cluster instance %s, but this check was bound to %s: the cluster was replaced while it was being verified — delete and re-create, then verify again", live, env.ClusterInstanceID)
		}
	}

	var problems []string
	image := ""
	templateFingerprint := ""
	deployment, err := client.Clientset.AppsV1().Deployments(webhookNamespace).Get(readCtx, webhookDeployment, metav1.GetOptions{})
	if err != nil {
		problems = append(problems, fmt.Sprintf("reading the %s/%s Deployment: %v (was the %s layer applied?)", webhookNamespace, webhookDeployment, err, Name))
	} else if available, why := kube.DeploymentAvailable(deployment); !available {
		problems = append(problems, fmt.Sprintf("the %s/%s Deployment is not Available: %s", webhookNamespace, webhookDeployment, why))
	} else if containers := deployment.Spec.Template.Spec.Containers; len(containers) == 0 {
		problems = append(problems, fmt.Sprintf("the %s/%s Deployment declares no containers", webhookNamespace, webhookDeployment))
	} else {
		image = containers[0].Image
		templateFingerprint = deployment.Spec.Template.Annotations[tlsFingerprintAnnotation]
	}

	var secretCA []byte
	var secretData map[string][]byte
	secret, err := client.Clientset.CoreV1().Secrets(webhookNamespace).Get(readCtx, webhookSecret, metav1.GetOptions{})
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("reading the %s/%s secret: %v (was the %s layer applied?)", webhookNamespace, webhookSecret, err, Name))
	default:
		if err := validateSecretData(secret.Data, time.Now()); err != nil {
			problems = append(problems, fmt.Sprintf("the %s/%s secret is not usable: %v", webhookNamespace, webhookSecret, err))
		} else {
			secretCA = secret.Data[secretCAKey]
			secretData = secret.Data
		}
	}

	// The registration must be verified to be exactly what the layer
	// renders before anything probes it. A probe pod created in a
	// watched namespace proves pods THERE are mutated; it says nothing
	// about the namespaces a drifted selector also matches, nor about
	// the objects a drifted objectSelector or matchCondition excludes:
	// one matching the probe's own static label mutates the probe
	// while ordinary pods in the same namespace pass unmutated, and
	// every positive probe still passes. An empty or removed
	// namespaceSelector matches every namespace, a broadened one or an
	// extra webhook entry mutates pod creates the layer never asked
	// for. Only a registration that matches the layer's configuration
	// exactly (registrationVerified) earns the probe.
	registrationVerified := false
	mwc, err := client.Clientset.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(readCtx, webhookConfig, metav1.GetOptions{})
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("reading the %s MutatingWebhookConfiguration: %v (was the %s layer applied?)", webhookConfig, err, Name))
	case len(mwc.Webhooks) == 0:
		problems = append(problems, fmt.Sprintf("the %s MutatingWebhookConfiguration carries no webhooks", webhookConfig))
	default:
		if secretCA != nil && string(mwc.Webhooks[0].ClientConfig.CABundle) != string(secretCA) {
			problems = append(problems, fmt.Sprintf("the %s MutatingWebhookConfiguration's caBundle does not match the %s secret's CA: the apiserver would not trust the certificate the webhook serves", webhookConfig, webhookSecret))
		}
		if drift := registrationDriftProblems(mwc, s); len(drift) > 0 {
			problems = append(problems, drift...)
		} else {
			registrationVerified = true
		}
	}

	// The pod template must carry the fingerprint of the secret's
	// material: the running webhook's pods were rolled from that
	// template, so a mismatch means they serve material the
	// configuration no longer trusts — the interrupted-rollout state a
	// rewritten-but-never-rolled secret leaves behind.
	if secretData != nil && image != "" && templateFingerprint != tlsFingerprint(secretData) {
		problems = append(problems, fmt.Sprintf("the %s/%s pod template carries TLS fingerprint %q, want the %s secret's %q: the webhook may be serving material the configuration no longer trusts (re-run create to roll it)", webhookNamespace, webhookDeployment, templateFingerprint, webhookSecret, tlsFingerprint(secretData)))
	}

	switch {
	case image == "":
		if len(problems) > 0 {
			// The deployment read already failed; probing without its
			// image would only add a less specific failure.
			problems = append(problems, "the admission probe did not run: no webhook image to probe with")
		}
	case !registrationVerified:
		problems = append(problems, "the admission probe did not run: the MutatingWebhookConfiguration was not verified to match the layer's configuration, and a probe that passes in the watched namespaces proves nothing about what else a drifted registration mutates")
	default:
		for _, ns := range s.WatchedNamespaces {
			if err := runProbe(ctx, client.Clientset, s.RunAsUser, image, ns, budget); err != nil {
				problems = append(problems, err.Error())
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d scc-admission check(s) failed: %s (was the %s layer applied?)", len(problems), strings.Join(problems, "; "), Name)
	}
	return nil
}
