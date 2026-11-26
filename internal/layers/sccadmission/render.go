package sccadmission

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/guzalv/kind-ocp/assets"
	"github.com/guzalv/kind-ocp/internal/kube"
)

// The names of everything the layer installs. Fixed, not settings: they
// are the layer's identity inside one cluster, and the TLS secret's DNS
// names (serviceDNSNames) derive from them.
const (
	webhookNamespace  = "scc-admission"
	webhookDeployment = "scc-admission-webhook"
	webhookService    = "scc-admission-webhook"
	webhookSecret     = "scc-admission-tls"
	webhookConfig     = "scc-admission"
	webhookName       = "scc-admission.kind-ocp.io"

	// namespaceNameLabel is the label every Kubernetes apiserver stamps
	// on every namespace with the namespace's own name (GA since 1.21).
	// The MutatingWebhookConfiguration selects on it, so the effective
	// scope is exactly the configured watchedNamespaces list — a
	// namespace removed from that list stops being mutated the moment
	// the configuration is re-applied, with no label to go stale.
	namespaceNameLabel = "kubernetes.io/metadata.name"

	// tlsFingerprintAnnotation carries a SHA-256 fingerprint of the TLS
	// secret's material on the webhook Deployment's pod template. Every
	// Apply re-renders it from the secret the run settled on, so a
	// changed secret changes the template and the Deployment controller
	// rolls the pods to remount it — including the case where an earlier
	// run rewrote the secret but died before the roll completed.
	tlsFingerprintAnnotation = "scc-admission.kind-ocp.io/tls-sha256"

	containerPort    = int32(8443)
	webhookBinary    = "/scc-admission-webhook"
	secretCAKey      = "ca.crt"
	envRunAsUserName = "SCC_RUN_AS_USER" // the env the webhook reads its UID from
)

// webhookMutatePath and webhookPort are the admission route the
// registration's service reference and the Service itself must agree
// on; one pair of constants feeds both renders and the drift check so
// they cannot fall out of step.
const (
	webhookMutatePath = "/mutate"
	webhookPort       = int32(443)
)

// The layer's default settings, as documented in the package doc.
const (
	defaultWatchedNamespace = "openshift-compliance"
	defaultRunAsUser        = int64(1001)
)

// DefaultBuilderImage is the pinned builder stage of the webhook
// image's docker build — the only pinned external input of the image
// (the source comes from this repo, embedded). The digest is the
// multi-arch index of golang:1.27.4-alpine, the Go toolchain this
// module builds with (go.mod), so the webhook compiles inside the
// image with the same toolchain that tests it. Bump it together with
// go.mod's Go version; the image tag is derived from the rendered
// Dockerfile, so a bump rolls the webhook automatically.
const DefaultBuilderImage = "docker.io/library/golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280"

// serviceDNSNames are the names the webhook's serving certificate must
// cover. The apiserver dials the Service's DNS name; the longer forms
// cost nothing and keep the certificate valid against resolvers that
// expand differently.
func serviceDNSNames() []string {
	fqdn := webhookService + "." + webhookNamespace
	return []string{
		webhookService,
		fqdn,
		fqdn + ".svc",
		fqdn + ".svc.cluster.local",
	}
}

// namespaceObjects renders the namespaces the layer owns: the webhook's
// own, plus every watched namespace (created if absent — the namespaces
// the Compliance Operator will deploy into exist before anything deploys
// into them). The webhook's own namespace is never rendered twice. A
// namespace that leaves the configured list is left in place, merely
// unwatched: the layer never deletes a namespace, which may hold a
// consumer's workloads.
func namespaceObjects(s settings) []*unstructured.Unstructured {
	objs := []*unstructured.Unstructured{namespaceObject(webhookNamespace)}
	for _, ns := range s.WatchedNamespaces {
		if ns == webhookNamespace {
			continue
		}
		objs = append(objs, namespaceObject(ns))
	}
	return objs
}

func namespaceObject(name string) *unstructured.Unstructured {
	return mustUnstructured(&corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	})
}

// deployment renders the webhook Deployment. The image is the locally
// built, locally loaded one, so it is never pulled: imagePullPolicy
// Never fails loudly (ErrImageNeverPull) if the load did not happen,
// instead of pulling some unrelated image a registry might serve under
// the name. The pod template carries tlsFingerprint — the SHA-256 of the
// TLS material the secret holds — so the Deployment controller rolls the
// pods whenever that material changes; the layer never deletes pods
// itself. The webhook itself runs non-root, without privileges it does
// not use, on a read-only root filesystem — the scratch image has
// nothing to write anyway — and without a ServiceAccount token: the
// webhook serves admission reviews and never calls the Kubernetes API,
// so the default token the apiserver would otherwise mount is a live
// cluster credential nothing uses.
func deployment(s settings, imageRef, tlsFingerprint string) *unstructured.Unstructured {
	labels := map[string]string{"app": webhookDeployment}
	runAsNonRoot := true
	runAsUser := int64(65534) // nobody: the webhook needs no privileges
	allowEscalation := false
	readOnlyRoot := true
	return mustUnstructured(&appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: webhookDeployment, Namespace: webhookNamespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{tlsFingerprintAnnotation: tlsFingerprint},
				},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptrTo(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: &runAsNonRoot,
						RunAsUser:    &runAsUser,
					},
					Containers: []corev1.Container{{
						Name:            "webhook",
						Image:           imageRef,
						ImagePullPolicy: corev1.PullNever,
						Ports:           []corev1.ContainerPort{{Name: "webhook", ContainerPort: containerPort}},
						Env:             []corev1.EnvVar{{Name: envRunAsUserName, Value: strconv.FormatInt(s.RunAsUser, 10)}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/healthz",
									Port:   intstr.FromInt32(containerPort),
									Scheme: corev1.URISchemeHTTPS,
								},
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: &allowEscalation,
							ReadOnlyRootFilesystem:   &readOnlyRoot,
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: "tls", MountPath: "/tls", ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{
						Name: "tls",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{SecretName: webhookSecret},
						},
					}},
				},
			},
		},
	})
}

func service() *unstructured.Unstructured {
	return mustUnstructured(&corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: webhookService, Namespace: webhookNamespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": webhookDeployment},
			Ports:    []corev1.ServicePort{{Port: webhookPort, TargetPort: intstr.FromInt32(containerPort)}},
		},
	})
}

// mutatingWebhookConfiguration renders the admission registration: pod
// CREATE operations in the configured watched namespaces, routed to the
// webhook's Service, trusting the CA that signed the webhook's serving
// certificate. The namespaceSelector matches the built-in name label
// against the configured list, so the scope is the list — nothing else.
// See the package doc for the failurePolicy trade-off.
func mutatingWebhookConfiguration(s settings, caCertPEM []byte) *unstructured.Unstructured {
	return mustUnstructured(&admissionregv1.MutatingWebhookConfiguration{
		TypeMeta:   metav1.TypeMeta{APIVersion: "admissionregistration.k8s.io/v1", Kind: "MutatingWebhookConfiguration"},
		ObjectMeta: metav1.ObjectMeta{Name: webhookConfig},
		Webhooks: []admissionregv1.MutatingWebhook{{
			Name:                    webhookName,
			AdmissionReviewVersions: []string{"v1"},
			SideEffects:             ptrTo(admissionregv1.SideEffectClassNone),
			FailurePolicy:           ptrTo(admissionregv1.Ignore),
			NamespaceSelector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      namespaceNameLabel,
					Operator: metav1.LabelSelectorOpIn,
					Values:   append([]string(nil), s.WatchedNamespaces...),
				}},
			},
			Rules: []admissionregv1.RuleWithOperations{{
				Operations: []admissionregv1.OperationType{admissionregv1.Create},
				Rule: admissionregv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"pods"},
					Scope:       ptrTo(admissionregv1.NamespacedScope),
				},
			}},
			ClientConfig: admissionregv1.WebhookClientConfig{
				Service: &admissionregv1.ServiceReference{
					Name:      webhookService,
					Namespace: webhookNamespace,
					Path:      ptrTo(webhookMutatePath),
					Port:      ptrTo(webhookPort),
				},
				CABundle: caCertPEM,
			},
		}},
	})
}

func ptrTo[T any](v T) *T { return &v }

// registrationDriftProblems reports every way the stored
// MutatingWebhookConfiguration's registration drifts from the shape the
// layer renders, as check-failure detail lines. The comparison covers
// the fields that decide the registration's reach — the entry count
// and name, the namespaceSelector (the admission scope), the
// objectSelector and matchConditions (both evaluated against the
// incoming request before the webhook is called, both able to narrow
// which creates reach it), the rules (which operations on which
// resources), and the service the entry routes to — but not the fields
// a healthy apiserver defaults on storage (timeoutSeconds,
// matchPolicy, the empty objectSelector), which no rendered object
// carries. The caBundle is compared separately, against the live
// secret. An empty return means the registration is exactly the
// layer's, and only then does the check's probe pod say anything about
// the scope it proves: a probe that passes in the watched namespaces
// is silent about what a drifted registration mutates elsewhere.
func registrationDriftProblems(mwc *admissionregv1.MutatingWebhookConfiguration, s settings) []string {
	var problems []string
	if len(mwc.Webhooks) != 1 {
		return append(problems, fmt.Sprintf("the %s MutatingWebhookConfiguration carries %d webhook entries, want exactly the one the layer applies (%s): the apiserver evaluates every entry, so an extra one mutates pod creates the layer never asked for", webhookConfig, len(mwc.Webhooks), webhookName))
	}
	wh := mwc.Webhooks[0]
	if wh.Name != webhookName {
		problems = append(problems, fmt.Sprintf("the %s MutatingWebhookConfiguration's webhook entry is named %q, want %q", webhookConfig, wh.Name, webhookName))
	}

	// The namespaceSelector is the admission scope. An empty selector
	// matches every namespace; only the single built-in-name match
	// against the configured list is the layer's scope.
	selector := wh.NamespaceSelector
	switch {
	case selector == nil || (len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0):
		problems = append(problems, fmt.Sprintf("the %s webhook's namespaceSelector is empty, which matches every namespace: pod creates anywhere would receive the injected UID", webhookName))
	case len(selector.MatchLabels) != 0 || len(selector.MatchExpressions) != 1:
		problems = append(problems, fmt.Sprintf("the %s webhook's namespaceSelector %+v is not the single built-in-name match the layer renders, want exactly the configured watched namespaces %v", webhookName, *selector, s.WatchedNamespaces))
	default:
		expr := selector.MatchExpressions[0]
		got := append([]string(nil), expr.Values...)
		want := append([]string(nil), s.WatchedNamespaces...)
		slices.Sort(got)
		slices.Sort(want)
		if expr.Key != namespaceNameLabel || expr.Operator != metav1.LabelSelectorOpIn || !slices.Equal(got, want) {
			problems = append(problems, fmt.Sprintf("the %s webhook's namespaceSelector selects %v, want exactly the configured watched namespaces %v: a broader selector mutates pod creates outside the configured scope, a narrower one leaves a watched namespace unmutated", webhookName, expr.Values, s.WatchedNamespaces))
		}
	}

	// The objectSelector is evaluated against the object the create
	// carries, before the webhook is called. The layer renders none
	// (the API defaults it to the empty selector, which matches every
	// object), so nil and empty are both the layer's shape — but a
	// nonempty one can match the probe's own static label while
	// ordinary pods in the same watched namespace pass unmutated, and
	// every probe would still pass.
	if sel := wh.ObjectSelector; sel != nil && (len(sel.MatchLabels) != 0 || len(sel.MatchExpressions) != 0) {
		problems = append(problems, fmt.Sprintf("the %s webhook's objectSelector %+v is not empty, which the layer never renders: it can select the probe's own pods while ordinary pod creates in the watched namespaces pass unmutated, and a probe that passes would prove nothing about them", webhookName, *sel))
	}

	// matchConditions are CEL expressions evaluated before the webhook
	// is called; the layer renders none. A condition can admit the
	// probe's creates while refusing ordinary ones — the same
	// false-pass shape a nonempty objectSelector produces.
	if n := len(wh.MatchConditions); n != 0 {
		problems = append(problems, fmt.Sprintf("the %s webhook carries %d matchConditions, want none: a condition is evaluated before the webhook is called and can admit the probe's creates while ordinary pod creates in the watched namespaces pass unmutated", webhookName, n))
	}

	// The rules are the admission trigger: pod CREATE in the core
	// group, namespaced, and nothing else.
	rulesOK := len(wh.Rules) == 1
	if rulesOK {
		r := wh.Rules[0]
		rulesOK = len(r.Operations) == 1 && r.Operations[0] == admissionregv1.Create &&
			slices.Equal(r.APIGroups, []string{""}) &&
			slices.Equal(r.APIVersions, []string{"v1"}) &&
			slices.Equal(r.Resources, []string{"pods"}) &&
			r.Scope != nil && *r.Scope == admissionregv1.NamespacedScope
	}
	if !rulesOK {
		problems = append(problems, fmt.Sprintf("the %s webhook's rules are %+v, want exactly pod CREATE in the core group (namespaced pods): a drifted rule mutates operations or resources the layer never asked for", webhookName, wh.Rules))
	}

	// The entry must route to the layer's own service: a re-routed
	// registration sends the admission calls somewhere else.
	if svc := wh.ClientConfig.Service; svc == nil || svc.Name != webhookService || svc.Namespace != webhookNamespace ||
		svc.Path == nil || *svc.Path != webhookMutatePath || svc.Port == nil || *svc.Port != webhookPort {
		problems = append(problems, fmt.Sprintf("the %s webhook does not route to the layer's service %s/%s at %s, want it to: a re-routed registration sends the admission calls somewhere else", webhookName, webhookNamespace, webhookService, webhookMutatePath))
	}
	return problems
}

// tlsFingerprint derives the deployment annotation from the secret's TLS
// material: a SHA-256 over the three fields in a fixed key order, so any
// change to what the webhook would serve changes the pod template and
// rolls the Deployment. It is computed from the secret's effective data
// — kept or freshly generated — never from what this run did, so a run
// that interrupts after rewriting the secret still converges on retry.
func tlsFingerprint(data map[string][]byte) string {
	h := sha256.New()
	for _, key := range []string{secretCAKey, "tls.crt", "tls.key"} {
		_, _ = h.Write([]byte(key))
		_, _ = h.Write(data[key])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// The builderImage pin, checked as a whole reference. The grammar is the
// image-reference grammar (distribution/reference): an optional registry
// domain with an optional port, a slash-separated lowercase repository
// path, an optional tag, and — mandatory here — a sha256 digest of
// exactly 64 lowercase hex digits. The value is interpolated verbatim
// into the Dockerfile (FROM), so anything that is not one well-formed
// reference, a multiline or whitespace-carrying string included, must be
// refused before docker ever sees it.
var (
	builderNameComponent = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	builderDomain        = `(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])` +
		`(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))*(?::[0-9]+)?`
	builderName  = `(?:` + builderDomain + `/)?` + builderNameComponent + `(?:/` + builderNameComponent + `)*`
	builderTag   = `[\w][\w.-]{0,127}`
	builderImage = regexp.MustCompile(`^(?:` + builderName + `)(?::` + builderTag + `)?@sha256:[0-9a-f]{64}$`)
)

// validateBuilderImage enforces the builderImage pin: exactly one valid
// image reference carrying an exact SHA-256 digest. An unpinned builder
// would float the image this layer's admission webhook runs; a malformed
// one would inject into the Dockerfile the build runs.
func validateBuilderImage(ref string) error {
	if builderImage.MatchString(ref) {
		return nil
	}
	return fmt.Errorf("builderImage must be exactly one digest-pinned image reference (name[:tag]@sha256:<64 lowercase hex digits>), got %q — an unpinned or malformed builder would float or inject into the image this layer's admission webhook runs", ref)
}

// mustUnstructured converts one typed object to the unstructured form
// the apply path takes. It cannot fail for the objects this package
// renders (plain API structs with JSON tags), so the callers stay
// linear; a conversion error would be a programming bug, and panicking
// names it at the source instead of at the first apply.
func mustUnstructured(obj runtime.Object) *unstructured.Unstructured {
	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(fmt.Sprintf("scc-admission: rendering %T: %v", obj, err))
	}
	return &unstructured.Unstructured{Object: converted}
}

// webhookGoMod is the module file of the standalone build context: the
// embedded source plus this file form a dependency-free module, which
// is what lets the builder stage compile it with GOPROXY=off — the
// image build needs no network beyond the pinned builder itself.
const webhookGoMod = `module scc-admission-webhook

go 1.27
`

// dockerfile renders the image build: the digest-pinned golang builder
// compiles the embedded webhook source into a static binary, and the
// scratch stage ships exactly that binary. The hermeticity flags are
// ENV, not build args, so no caller can weaken them: no cgo, no module
// downloads (GOPROXY=off fails loudly on any surprise dependency), no
// toolchain download (GOTOOLCHAIN=local — the pinned image must already
// carry the toolchain the source needs).
func dockerfile(builderImage string) string {
	return fmt.Sprintf(`# Generated by kind-ocp's scc-admission layer; the builder is the pinned
# external input (DefaultBuilderImage), the source is this repo's embedded
# assets/scc-admission.
FROM %s AS build
ENV CGO_ENABLED=0 GOPROXY=off GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod main.go patch.go ./
RUN go build -trimpath -o /out/scc-admission-webhook .
FROM scratch
COPY --from=build /out/scc-admission-webhook /scc-admission-webhook
ENTRYPOINT ["/scc-admission-webhook"]
`, builderImage)
}

// buildContext returns the docker build context: the rendered
// Dockerfile, the standalone module file, and the embedded webhook
// source. The same bytes feed imageRef's hash, so the image reference
// changes exactly when what goes into the image changes.
func buildContext(s settings) map[string][]byte {
	return map[string][]byte{
		"Dockerfile": []byte(dockerfile(s.BuilderImage)),
		"go.mod":     []byte(webhookGoMod),
		"main.go":    []byte(assets.SCCAdmissionWebhookMain),
		"patch.go":   []byte(assets.SCCAdmissionWebhookPatch),
	}
}

// imageRef derives the local image reference from the build inputs.
// The reference carries no registry hostname (localhost/…) because the
// image exists only on the local daemon and the cluster's nodes — it
// is never pulled, and the Deployment's imagePullPolicy says so. The
// tag is the hash of the build context, so the same source under the
// same builder pin is the same image: a source or pin change rolls the
// Deployment through a new tag, and an unchanged re-run is a no-op.
func imageRef(s settings) string {
	h := sha256.New()
	for _, name := range []string{"Dockerfile", "go.mod", "main.go", "patch.go"} {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write(buildContext(s)[name])
	}
	return fmt.Sprintf("localhost/kind-ocp/%s:sha256-%s", Name, hex.EncodeToString(h.Sum(nil))[:16])
}

// materializeContext writes the docker build context — the rendered
// Dockerfile, the standalone module file, and the embedded webhook
// source — into one private temporary directory and returns its path.
// The same bytes feed imageRef's hash, so the image reference changes
// exactly when what goes into the image changes. The directory is the
// layer's own content; the container-runtime invocation that turns it
// into an image belongs to the provider (env.BuildImage), not here —
// no layer invokes a container-runtime CLI directly.
func materializeContext(s settings) (string, error) {
	dir, err := os.MkdirTemp("", "kind-ocp-scc-admission-")
	if err != nil {
		return "", fmt.Errorf("building the scc-admission webhook image: %w", err)
	}
	for name, content := range buildContext(s) {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("building the scc-admission webhook image: writing %s: %w", name, err)
		}
	}
	return dir, nil
}

// probeCleanupTimeout bounds each probe pod's cleanup: the delete and
// the wait for the read that confirms it. It is independent of the
// probe's own budget — the probe may have exhausted or been cancelled,
// and its pod must still be removed — so the cleanup runs under a
// fresh deadline that ignores the probe's cancellation
// (context.WithoutCancel) but never runs unbounded.
const probeCleanupTimeout = 30 * time.Second

// runProbe proves the admission path live from inside one watched
// namespace: it creates pods that run non-root with no UID of their
// own until one comes back from the API carrying an injected UID (the
// create response is the object after admission) — the only observable
// of the apiserver having loaded the MutatingWebhookConfiguration,
// which lands on a delay of its own — then waits for that pod to reach
// Running, the kubelet's acceptance of the injected UID (an unmutated
// pod cannot get there: it fails with CreateContainerError), and
// checks the UID is the configured one.
//
// The timeout bounds the whole probe through a deadline context derived
// once here: every call the probe makes — the create attempts, the
// Running wait, the final read — carries that deadline, so a stalled
// apiserver fails the probe at the budget instead of hanging it. The
// probe pod is deleted with its own short budget on every path after it
// exists, the deletion confirmed by a read that no longer finds it, and
// a cleanup failure fails the probe alongside the probe's own failure —
// both are reported, so a failed probe that also left its pod behind
// names the leftover: a check that left its pod behind reports nothing
// it can be trusted on. The pod is the webhook's own image started with
// an argument (it idles), so the probe pulls nothing.
func runProbe(ctx context.Context, cs kubernetes.Interface, wantUID int64, imageRef, namespace string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	created, _, err := awaitMutation(ctx, cs, imageRef, namespace)
	if err != nil {
		return fmt.Errorf("probing namespace %q: %w", namespace, err)
	}
	probeErr := func() error {
		if err := kube.WaitForPodRunning(ctx, cs, namespace, created.Name, timeout); err != nil {
			return err
		}
		live, err := cs.CoreV1().Pods(namespace).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("re-reading the probe pod: %w", err)
		}
		if uid := live.Spec.SecurityContext.RunAsUser; uid == nil || *uid != wantUID {
			return fmt.Errorf("the probe pod runs with runAsUser %d, want the configured %d", deref(uid), wantUID)
		}
		return nil
	}()
	cleanupErr := deleteProbePod(ctx, cs, namespace, created.Name, created.UID, probeCleanupTimeout)
	if probeErr != nil || cleanupErr != nil {
		return fmt.Errorf("probing namespace %q: %w", namespace, errors.Join(probeErr, cleanupErr))
	}
	return nil
}

// deleteProbePod removes one probe pod under its own short deadline,
// derived from the probe's context values but not its cancellation or
// deadline (the probe is typically over by now, failed or cancelled,
// and its pod still has to go). Removal is proven, not requested: the
// API accepting the delete (a 202 as much as a 200) is not the pod
// being gone, so the cleanup waits, within the same deadline, for a
// read that no longer finds the pod. uid, when known, becomes a delete
// precondition — the pod is removed only if it is still the object
// that was read or created, so a name taken over by anything else is
// never deleted by this cleanup. A pod that is already gone is
// success; anything else is an error the caller surfaces.
func deleteProbePod(parent context.Context, cs kubernetes.Interface, namespace, name string, uid types.UID, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	opts := metav1.DeleteOptions{GracePeriodSeconds: ptrTo(int64(0))} // nothing the probe runs has anything to drain
	if uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := cs.CoreV1().Pods(namespace).Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("cleaning up the probe pod %s/%s: %w", namespace, name, err)
	}
	return waitForPodGone(ctx, cs, namespace, name)
}

// waitForPodGone polls until one read no longer finds the named pod —
// the only proof of deletion — under the cleanup context's own
// deadline. A read that fails without saying NotFound is not proof
// either way: it is retried, the last failure surfacing when the
// deadline runs out. A pod still present after its deletion was
// accepted (a finalizer can hold one indefinitely) fails the wait
// naming the leftover; the wait never strips a finalizer to succeed.
func waitForPodGone(ctx context.Context, cs kubernetes.Interface, namespace, name string) error {
	var last string
	for {
		_, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			last = "the pod is still present after its deletion was accepted"
		case apierrors.IsNotFound(err):
			return nil
		default:
			last = fmt.Sprintf("reading the pod to confirm its deletion: %v", err)
		}
		timer := time.NewTimer(probeRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("cleaning up the probe pod %s/%s: %w (last observation: %s)", namespace, name, ctx.Err(), last)
		case <-timer.C:
		}
	}
}

// probeRetryInterval spaces the admission attempts: the apiserver
// loads MutatingWebhookConfigurations through a watch, which lands on
// a delay of its own (observed at several seconds on a fresh kind
// apiserver), and each attempt costs one create and one delete against
// the local API.
const probeRetryInterval = time.Second

// createProbePod sends one probe pod create as exactly one wire POST.
// Two mechanisms would otherwise re-send it behind the caller's back:
// client-go retries requests automatically — any 5xx or 429 answered
// with a Retry-After header, up to ten times by default — and Go's
// http.Client follows a 307/308 by re-sending the original request and
// body to the redirect target, no retry policy consulted. Either
// makes one typed Create call many POSTs under one name: a later POST
// can succeed and be cleaned up while an earlier one is still
// pending, and that earlier one can then commit after the
// deletion-confirming read, leaving a pod behind a passing probe. The
// probe's create therefore goes through the layer's own clientset —
// the one client set every layer request uses, its transport refusing
// redirects — borrowing its CoreV1 REST client (the same generated
// construction the typed Create issues through) with retries
// disabled: the same request minus the replay. Reads and deletes keep
// the typed methods: a retried read is another observation, and a
// retried delete stays UID-preconditioned and read-confirmed.
func createProbePod(ctx context.Context, cs kubernetes.Interface, namespace string, pod *corev1.Pod) (*corev1.Pod, error) {
	var created corev1.Pod
	err := cs.CoreV1().RESTClient().Post().
		Namespace(namespace).
		Resource("pods").
		Body(pod).
		MaxRetries(0).
		Do(ctx).
		Into(&created)
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// awaitMutation polls the admission path until a freshly created probe
// pod comes back with a UID injected, returning that pod and how many
// attempts it took. Every attempt generates its own ownership token
// and sends its create under a name retained beforehand — as exactly
// one wire POST (createProbePod: no retries, no redirects followed) —
// so a create whose outcome is unknown — the transport dropped it, the
// apiserver answered 5xx or a redirect, the caller's context ended
// mid-request — is resolved by that name and that token before
// another attempt is made. One answer settles it: a
// pod carrying this attempt's token is adopted (and deleted, when it
// came back unmutated, under the UID the read observed). Every other
// answer the resolving read can give settles nothing, because a
// create still in flight can commit after the read — the foreign
// holder the read observed can go away and let it land, and a pod
// that did land can carry no token, admission able to strip it — so a
// free name and a foreign-held name both leave the outcome unknown:
// the probe fails naming the pod instead of retrying under a fresh
// name. A create answered AlreadyExists is the one failure that needs
// no resolution: the API refused it onto a held name, a refused
// create never commits, and the next attempt goes under a fresh name
// — decided on the POST's own response, never on a later read of the
// name. Nothing is deleted without its ownership confirmed by
// the token, and every deletion is preconditioned on the observed
// UID. A pod that comes back unmutated is deleted and the next
// attempt made under a fresh name; a context that ends — the probe
// budget's deadline firing, or the caller aborting the run — fails
// with the attempts counted and the causes named. The budget counts
// as ended by the clock as well: the context's cancellation can lag
// the deadline it carries, and a create that fails inside that gap —
// client-go's rate limiter refuses a request that would wait past
// the deadline, as an ordinary request error, not a context one —
// must not displace the observation the probe has already recorded.
// Transient API errors are retried within the same budget, the last
// one reported.
// The caller's context must carry the probe's deadline; every call
// here, creates included, runs under it, so a stalled apiserver costs
// the budget and not the run.
func awaitMutation(ctx context.Context, cs kubernetes.Interface, imageRef, namespace string) (pod *corev1.Pod, attempts int, err error) {
	deadline, hasDeadline := ctx.Deadline()
	started := time.Now()
	var lastProblem string
	// budgetGone reports whether the probe's budget is over. The
	// context is not the only authority: its cancellation can lag
	// the deadline it carries, while client-go's rate limiter
	// already refuses a request that would wait past that deadline —
	// an ordinary request error, not a context one. A create can
	// fail inside that gap, so the clock decides too.
	budgetGone := func() bool {
		return ctx.Err() != nil || (hasDeadline && !time.Now().Before(deadline))
	}
	for {
		attempts++
		name := probePodName()
		token := probeAttemptToken()
		created, createErr := createProbePod(ctx, cs, namespace, probePod(name, token, imageRef))
		if createErr != nil {
			// A create aborted by the caller's context or the deadline
			// keeps the previous observation: the failure text is not an
			// observation of the admission path, and the budget being
			// gone — by the context or by the clock — is what the
			// give-up returns below report.
			if !budgetGone() || lastProblem == "" {
				lastProblem = fmt.Sprintf("creating the probe pod: %v", createErr)
			}
			if !apierrors.IsAlreadyExists(createErr) {
				// A failed create is not proof that nothing was
				// created: the request can have been persisted before
				// the failure, or still be in flight and commit after
				// it. The attempt is resolved by its retained name and
				// its own token — before any next attempt, and before
				// anything is deleted.
				created, err = resolveCreate(ctx, cs, namespace, name, token)
				if err != nil {
					if !budgetGone() {
						return nil, attempts, fmt.Errorf("creating the probe pod: %v; the attempt could not be resolved: %w", createErr, err)
					}
					// The probe's own context is done — the budget
					// exhausted or the caller aborted — and the attempt's
					// outcome is unknown on top of that. Both facts are
					// reported: the probe's finding, and the pod the
					// unresolved attempt may have left, by name.
					if lastProblem == "" {
						lastProblem = "no observation was recorded"
					}
					cause := ctx.Err()
					if cause == nil {
						// The clock passed the deadline the context has
						// not reported yet (budgetGone above): the budget
						// is over all the same, by the exceeded deadline.
						cause = context.DeadlineExceeded
					}
					return nil, attempts, fmt.Errorf("giving up on the admission probe after %d attempts: %w; the last attempt could not be resolved: %w (last observation: %s)", attempts, cause, err, lastProblem)
				}
			}
			// AlreadyExists is the one create failure that settles
			// itself: the API processed this POST and refused it onto
			// a name something else holds, and a refused create never
			// commits — nothing of this attempt's can be left behind,
			// and the holder is not this attempt's to resolve. The
			// next attempt goes under a fresh name, decided on the
			// POST's own definitive response and never on a later
			// read of the name, which cannot prove what a create
			// still pending will do.
		}
		if created != nil {
			if created.Spec.SecurityContext != nil && created.Spec.SecurityContext.RunAsUser != nil {
				return created, attempts, nil
			}
			lastProblem = "the probe pod was admitted without a runAsUser — the mutating webhook did not intercept its creation (the MutatingWebhookConfiguration is missing, its namespaceSelector does not match, or the webhook call failed and failurePolicy Ignore admitted the pod unmutated)"
			if err := deleteProbePod(ctx, cs, namespace, created.Name, created.UID, probeCleanupTimeout); err != nil {
				return nil, attempts, err
			}
		}
		if err := ctx.Err(); err != nil {
			if lastProblem == "" {
				lastProblem = "no observation was recorded"
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, attempts, fmt.Errorf("the admission path never mutated the probe pod (%d attempts within %s): %s", attempts, time.Since(started).Round(time.Millisecond), lastProblem)
			}
			return nil, attempts, fmt.Errorf("giving up on the admission probe after %d attempts: %w (last observation: %s)", attempts, err, lastProblem)
		}
		pause := probeRetryInterval
		if hasDeadline {
			if remaining := time.Until(deadline); remaining < pause {
				pause = remaining
			}
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, attempts, fmt.Errorf("giving up on the admission probe: %w (last observation: %s)", ctx.Err(), lastProblem)
		case <-timer.C:
		}
	}
}

// probeResolveTimeout bounds the resolution of one ambiguous create.
// The budget is independent of the probe's own — the probe is
// typically over by now, its context cancelled or exhausted, and the
// attempt must still be resolved rather than abandoned — but never
// unbounded. One definitive read resolves an attempt; the budget
// exists for reads that keep failing.
const probeResolveTimeout = 5 * time.Second

// resolveCreate settles one ambiguous probe-pod create by reading the
// name the attempt retained: did the pod that create would have made
// come to exist, and is it this attempt's? The answer comes from one
// read, retried within its own bounded, cancellation-independent
// budget (an apiserver that just failed a create can fail the read
// too). Exactly one outcome settles the attempt:
//
//   - the name is held by a pod carrying this attempt's token: the
//     attempt persisted it, and the pod is returned for the caller to
//     treat exactly as a create's response — mutated, it becomes the
//     probe's pod; unmutated, it is deleted and the admission path
//     retried. Its UID travels with it, so the deletion is
//     preconditioned on the object that was read.
//
// Every other outcome is an error naming the pod, because none of
// them proves the create did not commit:
//
//   - the name is free: a create still in flight can commit after the
//     read, and no number of repeated 404s changes that (they are
//     more absence, not proof).
//   - the name is held by a pod without this attempt's token: the
//     holder can be foreign — and can go away between the read and a
//     pending create's commit, letting the create land behind the
//     read — or it can be the pod this attempt did persist, its token
//     stripped by admission. It is not provably foreign, so it is
//     never deleted, and the attempt is not provably failed, so it is
//     never retried away.
//   - no definitive read within the budget.
//
// The caller fails the probe with the error rather than retrying
// under a fresh name. The pod, if any, is named for inspection
// together with this attempt's ownership token, and the guidance
// conditions any hand removal on that token: a matching name or the
// static probe label alone is not proof of ownership, so the operator
// — not the error text — establishes what the pod is before removing
// it.
func resolveCreate(parent context.Context, cs kubernetes.Interface, namespace, name, attemptToken string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), probeResolveTimeout)
	defer cancel()
	var lastReadErr error
	for {
		pod, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		switch {
		case err == nil:
			if pod.Labels[probeAttemptLabelKey] != attemptToken {
				return nil, fmt.Errorf("the resolving read found a pod under %s/%s that does not carry this attempt's ownership token, which does not settle the attempt either: a foreign holder can go away and let a create still in flight commit after the read, and a pod that did commit can have had its token stripped by admission — the pod is not provably this attempt's, so nothing deletes it: inspect it, and remove it by hand only once you have confirmed it carries this attempt's token %q in label %s, since a matching name or the static probe label alone is not proof; re-running does not revisit it", namespace, name, attemptToken, probeAttemptLabelKey)
			}
			return pod, nil
		case apierrors.IsNotFound(err):
			return nil, fmt.Errorf("the resolving read found no pod under %s/%s, which does not prove the create failed: a create still in flight can commit after the read — if a pod appears under that name, remove it by hand only once you have confirmed it carries this attempt's token %q in label %s, since a matching name or the static probe label alone is not proof; re-running does not revisit it", namespace, name, attemptToken, probeAttemptLabelKey)
		default:
			lastReadErr = err
		}
		timer := time.NewTimer(probeRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if lastReadErr == nil {
				lastReadErr = errors.New("no read completed")
			}
			return nil, fmt.Errorf("reading the probe pod %s/%s within %s: %w (last read: %v) — a pod may exist under that name: remove it by hand only once you have confirmed it carries this attempt's token %q in label %s, since a matching name or the static probe label alone is not proof; re-running does not revisit it", namespace, name, probeResolveTimeout, ctx.Err(), lastReadErr, attemptToken, probeAttemptLabelKey)
		case <-timer.C:
		}
	}
}

func deref(uid *int64) int64 {
	if uid == nil {
		return 0
	}
	return *uid
}

// The probe pod's stamps. The static app label marks the pod as a
// kind-ocp probe for anyone reading the cluster; the per-attempt
// token under probeAttemptLabelKey is what ownership is decided on:
// fresh for every attempt of every run and independent of the pod's
// name, so a pod that merely shares the static label — another
// run's leftover under a colliding name, or a foreign pod a create
// was answered AlreadyExists against — is never this attempt's to
// adopt or delete.
const (
	probeLabelKey        = "app"
	probeLabelValue      = "kind-ocp-scc-admission-probe"
	probeAttemptLabelKey = "scc-admission.kind-ocp.io/probe-attempt"
)

// probePod renders the throwaway probe pod: non-root with no UID of
// its own — the exact shape the webhook exists to admit — running the
// webhook image in its idle mode, never restarted, never pulled, and
// without a ServiceAccount token (the probe never calls the Kubernetes
// API; the create, the reads, and the cleanup are the caller's). The
// static app label identifies it as a probe; the attempt token is the
// ownership marker a later resolution decides on.
func probePod(name, attemptToken, imageRef string) *corev1.Pod {
	runAsNonRoot := true
	return &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			probeLabelKey:        probeLabelValue,
			probeAttemptLabelKey: attemptToken,
		}},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptrTo(false),
			SecurityContext:              &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot},
			RestartPolicy:                corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           imageRef,
				ImagePullPolicy: corev1.PullNever,
				Args:            []string{"probe"}, // the webhook binary idles when started with an argument
			}},
		},
	}
}

// probePodName returns a fresh probe pod name: random because a probe
// from an earlier run may still be terminating under the deterministic
// alternative, and a create against a Terminating object is a conflict
// the probe has no interest in.
func probePodName() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on the platforms kind-ocp runs on; a
		// failed read still yields distinct-enough bytes from a zeroed
		// buffer only by luck, so fall back to the clock.
		return fmt.Sprintf("kind-ocp-scc-probe-%d", time.Now().UnixNano())
	}
	return "kind-ocp-scc-probe-" + hex.EncodeToString(b[:])
}

// probeAttemptToken returns the per-attempt ownership token: fresh
// random bytes for every attempt, independent of the pod's name, so
// ownership of a resolved pod is decided by a value only this attempt
// ever stamped — never by the name it happened to use, and never by
// the static label every probe pod shares.
func probeAttemptToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on the platforms kind-ocp runs on;
		// the clock keeps distinct-enough tokens flowing instead.
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
