//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/kube"
	"github.com/guzalv/kind-ocp/internal/layers/sccadmission"
	"github.com/guzalv/kind-ocp/kindocp"
)

// The GVRs and names this test reads directly, independently of the
// layer's own checks.
const (
	sccWebhookNamespace = "scc-admission"
	sccWatchedNamespace = "openshift-compliance"
	sccNameLabel        = "kubernetes.io/metadata.name"
)

// TestCreateWithSCCAdmissionLayer exercises increment 6's layer end to
// end on a real Docker daemon: create with the scc-admission profile
// (the layer has no hard dependencies, so it runs alone), verify
// passes, and the layer's postconditions hold independently of its own
// Verify — the MutatingWebhookConfiguration scoped by
// namespaceSelector with a caBundle matching the secret's CA, the
// deployment's pod template carrying the secret's TLS fingerprint, and
// the plan's check live: a pod with runAsNonRoot and no runAsUser in a
// watched namespace comes back mutated with the configured UID and
// reaches Running, while the same pod in an unwatched namespace is
// admitted untouched. A re-run of create reconciles with the TLS
// secret kept, and switching watchedNamespaces re-scopes the webhook:
// only the new namespace is mutated, the retired configuration's
// verify FAILs naming the unmutated probe, and the next create with it
// converges the scope back. A matchCondition planted by a separate
// field manager survives the layer's server-side apply, and the next
// create fails on the stored registration's shape with the field
// surviving and no probe pod remaining (the direct zero-probe-POST
// proof is the unit regression's call trace), naming the drift and the
// recovery, which converges.
func TestCreateWithSCCAdmissionLayer(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"
	stateRoot := filepath.Join(t.TempDir(), "state")

	increment6 := []string{sccadmission.Name}
	tweak := func(cfg *config.Config) {
		cfg.Profiles = map[string]config.Profile{"increment-6": {Layers: increment6}}
		cfg.Profile = "increment-6"
	}
	client, _ := testClientAt(t, prefix, stateRoot, tweak, nil)
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	// 1. Create with the layer applied. This run pays the docker build
	// of the webhook image (the digest-pinned golang builder pull
	// included) and the image load into the kind node.
	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(records) != 1 || records[0].Name != cluster {
		t.Fatalf("records = %+v, want one record for %s", records, cluster)
	}
	if records[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("record status = %s, want complete", records[0].Status)
	}
	if !slices.Equal(records[0].Layers, increment6) {
		t.Fatalf("record layers = %v, want %v", records[0].Layers, increment6)
	}

	// 2. Verify passes, including the layer's live probe check.
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment6, "after create")

	// 3. The layer's postconditions, read through the exported
	// kubeconfig, independently of its own Verify.
	kcPath, err := client.Kubeconfig(ctx, cluster)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	kubeClient, err := kube.NewClient(kcPath)
	if err != nil {
		t.Fatalf("building kube client: %v", err)
	}
	cs := kubeClient.Clientset

	mwc, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, sccadmission.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the MutatingWebhookConfiguration: %v", err)
	}
	if len(mwc.Webhooks) != 1 {
		t.Fatalf("the configuration carries %d webhooks, want one", len(mwc.Webhooks))
	}
	webhook := mwc.Webhooks[0]
	if webhook.FailurePolicy == nil || *webhook.FailurePolicy != "Ignore" {
		t.Errorf("failurePolicy = %v, want Ignore (the documented trade-off)", webhook.FailurePolicy)
	}
	// The scope is the built-in name label matched against the
	// configured list — not a label the layer leaves behind on
	// namespaces that leave the list.
	selector := webhook.NamespaceSelector
	if selector == nil || len(selector.MatchLabels) != 0 || len(selector.MatchExpressions) != 1 {
		t.Fatalf("namespaceSelector = %+v, want one match expression and no match labels", selector)
	}
	expr := selector.MatchExpressions[0]
	if expr.Key != sccNameLabel || expr.Operator != "In" || !slices.Equal(expr.Values, []string{sccWatchedNamespace}) {
		t.Errorf("match expression = %s %s %v, want the built-in name label matched with In against [%s]", expr.Key, expr.Operator, expr.Values, sccWatchedNamespace)
	}
	secret, err := cs.CoreV1().Secrets(sccWebhookNamespace).Get(ctx, "scc-admission-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the TLS secret: %v", err)
	}
	if string(webhook.ClientConfig.CABundle) != string(secret.Data["ca.crt"]) {
		t.Error("the caBundle does not match the secret's CA: the apiserver would not trust the webhook's certificate")
	}
	// The namespace the layer watches exists (created if absent).
	if _, err := cs.CoreV1().Namespaces().Get(ctx, sccWatchedNamespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("reading the watched namespace: %v", err)
	}
	deployment, err := cs.AppsV1().Deployments(sccWebhookNamespace).Get(ctx, "scc-admission-webhook", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the webhook deployment: %v", err)
	}
	if available, why := kube.DeploymentAvailable(deployment); !available {
		t.Errorf("the webhook deployment is not Available: %s", why)
	}
	webhookImage := deployment.Spec.Template.Spec.Containers[0].Image
	if !strings.Contains(webhookImage, "localhost/kind-ocp/scc-admission:sha256-") {
		t.Errorf("webhook image = %q, want the locally built, content-hashed reference", webhookImage)
	}
	if deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy != corev1.PullNever {
		t.Errorf("imagePullPolicy = %q, want Never (the image exists only on the node)", deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy)
	}
	// The pod template carries the fingerprint of the secret's TLS
	// material — computed here independently of the layer — so the
	// pods the Deployment rolls serve exactly what the configuration
	// trusts.
	h := sha256.New()
	for _, key := range []string{"ca.crt", "tls.crt", "tls.key"} {
		_, _ = h.Write([]byte(key))
		_, _ = h.Write(secret.Data[key])
	}
	if got := deployment.Spec.Template.Annotations["scc-admission.kind-ocp.io/tls-sha256"]; got != hex.EncodeToString(h.Sum(nil)) {
		t.Errorf("pod template TLS fingerprint = %q, want the secret's %q", got, hex.EncodeToString(h.Sum(nil)))
	}

	// The plan's verify check, driven independently of the layer: the
	// probe pod in the watched namespace is admitted mutated and runs.
	runAsNonRoot := true
	probe := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "it-scc-probe-watched", Labels: map[string]string{"app": "it-scc-probe"}},
		Spec: corev1.PodSpec{
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot},
			RestartPolicy:   corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           webhookImage,
				ImagePullPolicy: corev1.PullNever,
				Args:            []string{"probe"},
			}},
		},
	}
	created, err := cs.CoreV1().Pods(sccWatchedNamespace).Create(ctx, probe, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the probe pod in the watched namespace: %v", err)
	}
	defer func() {
		_ = cs.CoreV1().Pods(sccWatchedNamespace).Delete(context.Background(), probe.Name, metav1.DeleteOptions{})
	}()
	if uid := created.Spec.SecurityContext.RunAsUser; uid == nil || *uid != 1001 {
		t.Fatalf("the probe pod was admitted with runAsUser %v, want the webhook to have injected 1001", uid)
	}
	if err := kube.WaitForPodRunning(ctx, cs, sccWatchedNamespace, probe.Name, 2*time.Minute); err != nil {
		t.Fatalf("the probe pod in the watched namespace did not run: %v", err)
	}
	live, err := cs.CoreV1().Pods(sccWatchedNamespace).Get(ctx, probe.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sc := live.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 1001 ||
		sc.RunAsGroup == nil || *sc.RunAsGroup != 1001 || sc.FSGroup == nil || *sc.FSGroup != 1001 {
		t.Errorf("the running probe carries securityContext %+v, want runAsUser, runAsGroup, and fsGroup all 1001", sc)
	}

	// The same pod shape outside the watched namespaces is admitted
	// untouched: the webhook is scoped by the namespaceSelector, not
	// cluster-wide.
	unwatched := probe.DeepCopy()
	unwatched.Name = "it-scc-probe-unwatched"
	unwatchedCreated, err := cs.CoreV1().Pods("default").Create(ctx, unwatched, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the probe pod in the default namespace: %v", err)
	}
	defer func() {
		_ = cs.CoreV1().Pods("default").Delete(context.Background(), unwatched.Name, metav1.DeleteOptions{})
	}()
	if uid := unwatchedCreated.Spec.SecurityContext.RunAsUser; uid != nil {
		t.Errorf("the pod in the default namespace was mutated to runAsUser %d; the webhook must be scoped to watched namespaces", *uid)
	}

	// 4. Re-running create reconciles: the docker build is cached, the
	// TLS secret is kept (the PoC's stale-certificate bug), and verify
	// still passes.
	caBefore := string(secret.Data["ca.crt"])
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	kept, err := cs.CoreV1().Secrets(sccWebhookNamespace).Get(ctx, "scc-admission-tls", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading the TLS secret: %v", err)
	}
	if string(kept.Data["ca.crt"]) != caBefore {
		t.Error("the re-run replaced a still-valid TLS secret; it must be kept so the running webhook keeps serving what the configuration trusts")
	}
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after reconcile: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment6, "after reconcile")

	// 5. The namespace-switch regression: changing watchedNamespaces
	// re-scopes the webhook to the new list. Only the new namespace is
	// mutated afterwards — the old one is admitted unmutated even
	// though it was watched before (the scope is the configuration,
	// not a label to go stale) — and a verify run against the old
	// configuration FAILs naming the drifted scope, converging back
	// on the next create with it.
	const switchedNamespace = "it-scc-switched"
	var sccSettings struct {
		SCC yaml.Node `yaml:"scc-admission"`
	}
	if err := yaml.Unmarshal([]byte("scc-admission:\n  watchedNamespaces: ["+switchedNamespace+"]\n"), &sccSettings); err != nil {
		t.Fatal(err)
	}
	switchedClient, _ := testClientAt(t, prefix, stateRoot, func(cfg *config.Config) {
		cfg.Profiles = map[string]config.Profile{"increment-6": {Layers: increment6}}
		cfg.Profile = "increment-6"
		cfg.Layers = map[string]yaml.Node{sccadmission.Name: sccSettings.SCC}
	}, nil)
	if _, err := switchedClient.Create(ctx); err != nil {
		t.Fatalf("create after the namespace switch: %v", err)
	}
	results, err = switchedClient.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after the namespace switch: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment6, "after the namespace switch")

	// The switched selector, read straight from the configuration.
	switchedMWC, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, sccadmission.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	switchedExpr := switchedMWC.Webhooks[0].NamespaceSelector.MatchExpressions[0]
	if !slices.Equal(switchedExpr.Values, []string{switchedNamespace}) {
		t.Fatalf("namespaceSelector values after the switch = %v, want [%s]", switchedExpr.Values, switchedNamespace)
	}

	// Only the new namespace is mutated: a probe pod there is admitted
	// with the UID injected and runs, while the same pod in the
	// previously watched namespace comes back unmutated. (The unmutated
	// pod cannot run on the kubelet — CreateContainerError — so its
	// create response is the check; it is deleted right away.)
	switchedProbe := probe.DeepCopy()
	switchedProbe.Name = "it-scc-probe-switched"
	switchedCreated, err := cs.CoreV1().Pods(switchedNamespace).Create(ctx, switchedProbe, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the probe pod in the switched namespace: %v", err)
	}
	defer func() {
		_ = cs.CoreV1().Pods(switchedNamespace).Delete(context.Background(), switchedProbe.Name, metav1.DeleteOptions{})
	}()
	if uid := switchedCreated.Spec.SecurityContext.RunAsUser; uid == nil || *uid != 1001 {
		t.Errorf("the probe pod in the switched namespace was admitted with runAsUser %v, want the injected 1001", uid)
	}
	oldProbe := probe.DeepCopy()
	oldProbe.Name = "it-scc-probe-retired"
	oldCreated, err := cs.CoreV1().Pods(sccWatchedNamespace).Create(ctx, oldProbe, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the probe pod in the previously watched namespace: %v", err)
	}
	if uid := oldCreated.Spec.SecurityContext.RunAsUser; uid != nil {
		t.Errorf("the probe pod in the previously watched namespace was mutated to runAsUser %d; the scope must follow the configuration", *uid)
	}
	if err := cs.CoreV1().Pods(sccWatchedNamespace).Delete(ctx, oldProbe.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the unmutated probe pod: %v", err)
	}
	// The retired namespace is left in place, merely unwatched.
	if _, err := cs.CoreV1().Namespaces().Get(ctx, sccWatchedNamespace, metav1.GetOptions{}); err != nil {
		t.Fatalf("the previously watched namespace was removed: %v (the layer must not delete namespaces)", err)
	}

	// A verify run against the retired configuration FAILs: the
	// registration's scope no longer matches what that configuration
	// watches, and the check names the drifted selector — running its
	// probe against a registration that does not match the verified
	// configuration would prove nothing about the scope the
	// configuration asks about (the shape gate from the security
	// review), so the probe is skipped and the drift named instead.
	corruptResults, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify with the retired configuration: %v", err)
	}
	var layerCheck *kindocp.CheckResult
	for i := range corruptResults {
		if corruptResults[i].Check == "layer:"+sccadmission.Name {
			layerCheck = &corruptResults[i]
		}
	}
	if layerCheck == nil || layerCheck.Status != kindocp.StatusFail {
		t.Fatalf("verify with the retired configuration = %+v, want a FAIL for layer:%s", corruptResults, sccadmission.Name)
	}
	for _, want := range []string{
		"namespaceSelector selects [" + switchedNamespace + "]",
		"want exactly the configured watched namespaces [" + sccWatchedNamespace + "]",
		"the admission probe did not run",
	} {
		if !strings.Contains(layerCheck.Detail, want) {
			t.Errorf("layer:%s FAIL detail = %q, want it to name the drifted scope (%q)", sccadmission.Name, layerCheck.Detail, want)
		}
	}

	// The next create with the retired configuration converges the
	// scope back.
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("converge create after the switch back: %v", err)
	}
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after the switch back: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment6, "after the switch back")

	// 6. The foreign-manager regression: a field another field manager
	// owns — a matchCondition, absent from the layer's manifest —
	// survives the layer's server-side apply, so a create over it must
	// fail on the stored registration's shape instead of trusting the
	// probe. The condition is harmless on purpose (an expression of
	// true narrows no admission), so it proves the postcondition
	// mismatch without breaking the cluster: the probe would still
	// pass, which is exactly why the create cannot be allowed to trust
	// it. The failure names the drift and the recovery, removes
	// nothing itself, and the recovery it names converges.
	current, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, sccadmission.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the MutatingWebhookConfiguration: %v", err)
	}
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "MutatingWebhookConfiguration",
		"metadata":   map[string]any{"name": sccadmission.Name},
		"webhooks": []any{map[string]any{
			"name": current.Webhooks[0].Name,
			"matchConditions": []any{map[string]any{
				"name":       "foreign-manager-condition",
				"expression": "true",
			}},
		}},
	}}
	mwcGVR := schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "mutatingwebhookconfigurations"}
	if _, err := kubeClient.Dynamic.Resource(mwcGVR).Apply(ctx, foreign.GetName(), foreign, metav1.ApplyOptions{FieldManager: "kind-ocp-integration-foreign-manager"}); err != nil {
		t.Fatalf("planting the separately managed matchCondition: %v", err)
	}

	_, err = client.Create(ctx)
	if err == nil {
		t.Fatal("create succeeded over a registration another field manager had drifted, want the drift to fail it")
	}
	for _, want := range []string{
		"is not the registration the layer applies",
		"on cluster " + cluster,
		"matchConditions",
		"never removes fields it does not own",
		// The recovery is scoped: the delete runs through this
		// cluster's kubeconfig, never the ambient one (the exact-path
		// proof is the unit regression's).
		"kubectl --kubeconfig",
		"delete mutatingwebhookconfiguration " + sccadmission.Name,
		"re-run create",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("create = %v, want %q in the error", err, want)
		}
	}
	// A residue check: no probe pod is left in the watched namespace
	// after the failed create. This is not the zero-probe proof — a
	// probe that ran and completed deletes its own pod, so an empty
	// list is consistent with a probe having run; the direct proof
	// that no probe POST was issued is the unit regression's call
	// trace. What this establishes on the real cluster is that the
	// failure left nothing behind.
	probePods, err := cs.CoreV1().Pods(sccWatchedNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app=kind-ocp-scc-admission-probe"})
	if err != nil {
		t.Fatalf("listing probe pods: %v", err)
	}
	if len(probePods.Items) != 0 {
		t.Errorf("probe pods left after the failed create = %d, want 0 (no residue; the zero-probe-POST proof is the unit call trace)", len(probePods.Items))
	}
	// The layer removed nothing: the foreign field is still stored,
	// owned by its manager.
	drifted, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, sccadmission.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading the MutatingWebhookConfiguration: %v", err)
	}
	if n := len(drifted.Webhooks[0].MatchConditions); n != 1 {
		t.Errorf("matchConditions after the failed create = %d, want 1 (the layer must not remove fields another manager owns)", n)
	}

	// The recovery the error names: delete the drifted registration
	// and re-run create, which re-creates it in the layer's shape.
	if err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Delete(ctx, sccadmission.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the drifted registration: %v", err)
	}
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create after the recovery: %v", err)
	}
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after the recovery: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment6, "after the foreign-manager recovery")
	recovered, err := cs.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, sccadmission.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the recovered MutatingWebhookConfiguration: %v", err)
	}
	if n := len(recovered.Webhooks[0].MatchConditions); n != 0 {
		t.Errorf("matchConditions after the recovery = %d, want 0 (the re-created registration carries only the layer's shape)", n)
	}
}
