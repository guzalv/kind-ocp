package compliance

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/stackrox/kind-ocp/internal/kube"
)

// The custom resources the proof reads, as resource paths. The kinds
// have no typed Go types in this module (adding the operator's API as
// a dependency would drag its whole tree for a handful of status
// reads), so the proof reads them through the dynamic client — the
// same pattern the platform-objects layer uses for OpenShift kinds.
var (
	subscriptionGVR = schema.GroupVersionResource{
		Group: "operators.coreos.com", Version: "v1alpha1", Resource: "subscriptions",
	}
	csvGVR = schema.GroupVersionResource{
		Group: "operators.coreos.com", Version: "v1alpha1", Resource: "clusterserviceversions",
	}
	profileBundleGVR = schema.GroupVersionResource{
		Group: "compliance.openshift.io", Version: "v1alpha1", Resource: "profilebundles",
	}
	scanSettingBindingGVR = schema.GroupVersionResource{
		Group: "compliance.openshift.io", Version: "v1alpha1", Resource: "scansettingbindings",
	}
	checkResultGVR = schema.GroupVersionResource{
		Group: "compliance.openshift.io", Version: "v1alpha1", Resource: "compliancecheckresults",
	}
)

// The labels the Compliance Operator stamps on the objects the proof
// reads (its API constants, mirrored): the check-status label carries
// a result's status, and the suite label ties a result to the
// ComplianceSuite — named after the binding — that produced it.
const (
	checkStatusLabel = "compliance.openshift.io/check-status"
	suiteLabel       = "compliance.openshift.io/suite"
)

// The check statuses the proof counts. PASS and FAIL are the two
// outcomes of a real OVAL evaluation; everything else —
// NOT-APPLICABLE, INFO, MANUAL, ERROR — is a result the proof does
// not count as evidence.
const (
	checkStatusPass = "PASS"
	checkStatusFail = "FAIL"
)

// waitForCatalogServer waits for the proof's catalog server pod to
// report Ready — the readiness probe is the gRPC health check, so
// Ready is the first moment the catalog is actually servable. The
// observation carries the pod's phase and every waiting container's
// reason, so an image that will not pull or a catalog opm refuses to
// load names itself in the failure.
func waitForCatalogServer(ctx context.Context, cs kubernetes.Interface, timeout time.Duration) error {
	return kube.WaitForCondition(ctx, timeout,
		fmt.Sprintf("the catalog server pod %s/%s becoming Ready", catalogNamespace, catalogServerName),
		fmt.Sprintf("the catalog server pod %s/%s did not become Ready — the proof's catalog cannot be served; read the pod's events and logs (an opm that refuses the catalog exits, and the readiness probe never passes)", catalogNamespace, catalogServerName),
		func(ctx context.Context) (bool, string) {
			pod, err := cs.CoreV1().Pods(catalogNamespace).Get(ctx, catalogServerName, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Sprintf("reading the catalog server pod: %v", err)
			}
			for i := range pod.Status.Conditions {
				cond := pod.Status.Conditions[i]
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return true, ""
				}
			}
			return false, podObservation(pod)
		})
}

// podObservation describes a pod that is not Ready: its phase, every
// waiting container's reason and message, and restart counts when a
// container is cycling.
func podObservation(pod *corev1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "phase %s", pod.Status.Phase)
	for i := range pod.Status.ContainerStatuses {
		status := pod.Status.ContainerStatuses[i]
		if waiting := status.State.Waiting; waiting != nil && waiting.Reason != "" {
			fmt.Fprintf(&b, ", container %s waiting: %s", status.Name, waiting.Reason)
			if waiting.Message != "" {
				fmt.Fprintf(&b, ": %s", waiting.Message)
			}
		}
		if status.RestartCount > 0 {
			fmt.Fprintf(&b, ", container %s restarted %d time(s)", status.Name, status.RestartCount)
		}
	}
	return b.String()
}

// waitForResolvedCSV waits for the Subscription to resolve an
// installable ClusterServiceVersion and returns its name. The PoC's
// fallback is ported whole: status.currentCSV may name a CSV the
// catalog never creates (a channel-head/CSV-name mismatch in the
// upstream catalog), so when the named CSV is absent the resolution
// falls back to the compliance-operator CSV OLM actually installed,
// and the log names which path was taken. The fallback matches on the
// csvNamePrefix shape and picks deterministically (sorted, first)
// where the PoC took whatever the API listed first.
func waitForResolvedCSV(ctx context.Context, dyn dynamic.Interface, log *slog.Logger, timeout time.Duration) (string, error) {
	var viaFallback bool
	var csv string
	err := kube.WaitForCondition(ctx, timeout,
		fmt.Sprintf("the %s Subscription resolving a CSV", subscriptionName),
		fmt.Sprintf("the %s Subscription did not resolve an installable CSV", subscriptionName),
		func(ctx context.Context) (bool, string) {
			var observation string
			csv, viaFallback, observation = resolveCSV(ctx, dyn)
			return csv != "", observation
		})
	if err != nil {
		return "", err
	}
	if viaFallback {
		log.Info("the Subscription's currentCSV never materialized; falling back to the CSV OLM installed", "csv", csv)
	}
	return csv, nil
}

// resolveCSV is one resolution observation: the Subscription's
// currentCSV when that CSV exists, else the first
// compliance-operator-prefixed CSV in the namespace, else nothing. An
// empty name with a non-empty observation means "keep waiting"; the
// observation becomes the failure message on timeout.
func resolveCSV(ctx context.Context, dyn dynamic.Interface) (csv string, viaFallback bool, observation string) {
	sub, err := dyn.Resource(subscriptionGVR).Namespace(operatorNamespace).Get(ctx, subscriptionName, metav1.GetOptions{})
	if err != nil {
		return "", false, fmt.Sprintf("reading the %s Subscription: %v", subscriptionName, err)
	}
	installed, listErr := listCSVs(ctx, dyn)
	if listErr != nil {
		return "", false, listErr.Error()
	}
	current, _, err := unstructured.NestedString(sub.Object, "status", "currentCSV")
	if err != nil {
		return "", false, fmt.Sprintf("reading the %s Subscription's status.currentCSV: %v", subscriptionName, err)
	}
	if current != "" && containsName(installed, current) {
		return current, false, ""
	}
	if fallback := firstPrefixed(installed, csvNamePrefix); fallback != "" {
		return fallback, true, ""
	}
	switch {
	case current != "":
		return "", false, fmt.Sprintf("the Subscription resolved currentCSV %q but no such CSV exists yet, and no %s* CSV is installed (current CSVs: %s)", current, csvNamePrefix, describeNames(installed))
	default:
		return "", false, fmt.Sprintf("the Subscription has no status.currentCSV yet, and no %s* CSV is installed", csvNamePrefix)
	}
}

// listCSVs reads the operator namespace's ClusterServiceVersions once,
// in a stable order.
func listCSVs(ctx context.Context, dyn dynamic.Interface) ([]string, error) {
	list, err := dyn.Resource(csvGVR).Namespace(operatorNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing the ClusterServiceVersions in %s: %w", operatorNamespace, err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].GetName())
	}
	sort.Strings(names)
	return names, nil
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func firstPrefixed(names []string, prefix string) string {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return n
		}
	}
	return ""
}

func describeNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// waitForCSVSucceeded waits for the named ClusterServiceVersion to
// reach phase Succeeded — OLM's signal that the install plan applied
// the operator's objects. Fresh deadline by construction: this wait
// starts only after the resolution wait returned, so it can never
// inherit a spent budget (the PoC bug).
func waitForCSVSucceeded(ctx context.Context, dyn dynamic.Interface, csv string, timeout time.Duration) error {
	return kube.WaitForCondition(ctx, timeout,
		fmt.Sprintf("the %s CSV reaching phase Succeeded", csv),
		fmt.Sprintf("the %s CSV did not reach phase Succeeded", csv),
		func(ctx context.Context) (bool, string) {
			obj, err := dyn.Resource(csvGVR).Namespace(operatorNamespace).Get(ctx, csv, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Sprintf("reading the %s CSV: %v", csv, err)
			}
			phase, _, err := unstructured.NestedString(obj.Object, "status", "phase")
			if err != nil {
				return false, fmt.Sprintf("reading the %s CSV's status.phase: %v", csv, err)
			}
			if phase == "Succeeded" {
				return true, ""
			}
			why := phase
			if why == "" {
				why = "none yet"
			}
			return false, fmt.Sprintf("phase %s", why)
		})
}

// waitForProfileBundle waits for one operator-created ProfileBundle to
// report dataStreamStatus VALID: the content image pulled and parsed,
// the Profile objects the proof's binding references created. Any
// other status — including the operator's own ERROR — is an
// observation the wait reports on timeout.
func waitForProfileBundle(ctx context.Context, dyn dynamic.Interface, bundle string, timeout time.Duration) error {
	return kube.WaitForCondition(ctx, timeout,
		fmt.Sprintf("the %s ProfileBundle becoming VALID", bundle),
		fmt.Sprintf("the %s ProfileBundle did not become VALID", bundle),
		func(ctx context.Context) (bool, string) {
			obj, err := dyn.Resource(profileBundleGVR).Namespace(operatorNamespace).Get(ctx, bundle, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Sprintf("reading the %s ProfileBundle: %v", bundle, err)
			}
			status, _, err := unstructured.NestedString(obj.Object, "status", "dataStreamStatus")
			if err != nil {
				return false, fmt.Sprintf("reading the %s ProfileBundle's status.dataStreamStatus: %v", bundle, err)
			}
			if status == "VALID" {
				return true, ""
			}
			why := status
			if why == "" {
				why = "none yet"
			}
			return false, fmt.Sprintf("dataStreamStatus %s", why)
		})
}

// waitForBindingReady waits for the proof's ScanSettingBinding to
// reach phase READY: the binding controller found the ocp4-cis
// Profile and created the ComplianceSuite (named after the binding).
// INVALID is terminal — the profile the binding names is missing —
// and is reported as an observation like any other, so the timeout
// message carries it.
func waitForBindingReady(ctx context.Context, dyn dynamic.Interface, timeout time.Duration) error {
	return kube.WaitForCondition(ctx, timeout,
		fmt.Sprintf("the %s ScanSettingBinding becoming READY", bindingName),
		fmt.Sprintf("the %s ScanSettingBinding did not become READY", bindingName),
		func(ctx context.Context) (bool, string) {
			obj, err := dyn.Resource(scanSettingBindingGVR).Namespace(operatorNamespace).Get(ctx, bindingName, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Sprintf("reading the %s ScanSettingBinding: %v", bindingName, err)
			}
			phase, _, err := unstructured.NestedString(obj.Object, "status", "phase")
			if err != nil {
				return false, fmt.Sprintf("reading the %s ScanSettingBinding's status.phase: %v", bindingName, err)
			}
			if phase == "READY" {
				return true, ""
			}
			why := phase
			if why == "" {
				why = "none yet"
			}
			return false, fmt.Sprintf("phase %s", why)
		})
}

// waitForScanResults is the proof's conclusion: the ocp4-cis scan
// produced at least one real PASS and at least one real FAIL
// ComplianceCheckResult, attributed to the proof's own suite by the
// label the operator stamps. Both are required — a PASS proves the
// platform layers carry the objects the OVAL checks read, a FAIL
// proves the evaluation actually ran against them (an all-PASS result
// on a fresh cluster would mean the checks matched nothing, and an
// all-NOT-APPLICABLE result means the platform layers are missing,
// which is exactly the failure the PoC's Central-side assertion
// guarded against).
func waitForScanResults(ctx context.Context, dyn dynamic.Interface, log *slog.Logger, timeout time.Duration) error {
	var lastPass, lastFail int
	err := kube.WaitForCondition(ctx, timeout,
		"the ocp4-cis scan producing a PASS and a FAIL result",
		"the ocp4-cis scan did not produce both a PASS and a FAIL ComplianceCheckResult",
		func(ctx context.Context) (bool, string) {
			list, err := dyn.Resource(checkResultGVR).Namespace(operatorNamespace).List(ctx, metav1.ListOptions{
				LabelSelector: suiteLabel + "=" + bindingName,
			})
			if err != nil {
				return false, fmt.Sprintf("listing the scan's ComplianceCheckResults: %v", err)
			}
			pass, fail, other := countCheckResults(list.Items)
			lastPass, lastFail = pass, fail
			if pass >= 1 && fail >= 1 {
				return true, ""
			}
			return false, fmt.Sprintf("%d PASS, %d FAIL, %d other result(s) so far", pass, fail, other)
		})
	if err == nil {
		log.Info("the ocp4-cis scan produced real results", "pass", lastPass, "fail", lastFail)
		return nil
	}
	// The failure text carries the hint the all-N/A shape needs: the
	// checks read the config.openshift.io objects the platform layers
	// install, so a scan that produces no PASS and no FAIL usually
	// means those layers were never applied.
	return fmt.Errorf("%w — an all-NOT-APPLICABLE scan usually means the OpenShift platform layers are missing (was the cluster created with the full profile?)", err)
}

// countCheckResults counts one result list's statuses: PASS, FAIL,
// and everything else. Pure, so the counting is table-tested without
// an apiserver.
func countCheckResults(items []unstructured.Unstructured) (pass, fail, other int) {
	for i := range items {
		switch items[i].GetLabels()[checkStatusLabel] {
		case checkStatusPass:
			pass++
		case checkStatusFail:
			fail++
		default:
			other++
		}
	}
	return pass, fail, other
}
