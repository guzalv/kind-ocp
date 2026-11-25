package kube

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// crdGVR identifies CustomResourceDefinition objects in the API. The
// dynamic client reads them without a dependency on the
// apiextensions-apiserver module.
var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

// pollCondition polls observe every pollInterval until it reports the
// condition met, the timeout elapses, or ctx is done. observe runs under
// the poll context and returns whether the condition holds plus an
// observation for the failure message; an empty observation ("this call
// was aborted before it could observe anything") keeps the previous
// one.
//
// The timeout bounds the whole wait, not just the polling (the poll runs
// under a deadline context), and a condition reported met after the
// deadline — a response that raced cancellation — is rejected, never
// accepted as success. The caller's context ending before the timeout
// reports that cause with the last observation. waitingFor and notMet
// name the condition in the two failure messages: "giving up on
// <waitingFor>: <cause>" and "<notMet> within <timeout>: <observation>".
func pollCondition(
	ctx context.Context,
	timeout time.Duration,
	waitingFor, notMet string,
	observe func(ctx context.Context) (met bool, observation string),
) error {
	var last string
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			// A tick that lands on or past the deadline must not issue
			// a doomed call: the rate limiter and the transport refuse
			// it with their own cancellation text, which would
			// overwrite the last real observation. The deadline is
			// checked by timestamp, not by ctx.Err() — the runtime's
			// timer may not have fired yet when a deadline-aligned
			// tick runs.
			if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
				return false, context.DeadlineExceeded
			}
			met, observation := observe(ctx)
			if observation != "" && ctx.Err() == nil {
				// An observation from a call the wait's own deadline
				// aborted ("context deadline exceeded") says nothing
				// about the condition; keeping it would overwrite the
				// last real one with cancellation text.
				last = observation
			}
			if met && ctx.Err() != nil {
				// The poll helper returns as soon as the callback reports
				// the condition met — even when the context ended during
				// the callback — so an answer that arrives after the
				// deadline is recorded as late and rejected.
				if observation == "" {
					observation = "condition met"
				}
				last = observation + ", but the answer arrived after the wait was over; not accepted"
				return false, nil
			}
			return met, nil
		})
	if err == nil {
		return nil
	}
	if cause := ctx.Err(); cause != nil {
		// The caller's context ended before the wait's timeout did.
		if last != "" {
			return fmt.Errorf("giving up on %s: %w (last observation: %s)", waitingFor, cause, last)
		}
		return fmt.Errorf("giving up on %s: %w", waitingFor, cause)
	}
	if last == "" {
		last = "no observation recorded"
	}
	return fmt.Errorf("%s within %s: %s", notMet, timeout, last)
}

// WaitForCondition is the exported face of the one poll loop kind-ocp's
// waits share: the layer waits below and the CO fitness proof
// (internal/verify/compliance) both poll cluster state on it, so their
// deadline handling — a condition met after the deadline is rejected,
// a caller's cancellation outranks the timeout, and the last real
// observation survives as the failure message — cannot drift between
// consumers. See pollCondition for the contract.
func WaitForCondition(
	ctx context.Context,
	timeout time.Duration,
	waitingFor, notMet string,
	observe func(ctx context.Context) (met bool, observation string),
) error {
	return pollCondition(ctx, timeout, waitingFor, notMet, observe)
}

// WaitForCRDsEstablished polls the named CustomResourceDefinitions until
// every one reports its Established condition True — the apiserver's
// signal that the kind is served — or the timeout elapses. Objects of
// the kinds these CRDs define must not be applied before this passes
// (a ported PoC gotcha): the apiserver rejects them until the CRD is
// established. Transient read errors are retried within the timeout and
// surface as the last observation on failure.
func WaitForCRDsEstablished(ctx context.Context, dyn dynamic.Interface, timeout time.Duration, names []string) error {
	if len(names) == 0 {
		return nil
	}
	return pollCondition(ctx, timeout, "CRD establishment", "CRDs not Established",
		func(ctx context.Context) (bool, string) {
			pending := crdsNotEstablished(ctx, dyn, names)
			if len(pending) == 0 {
				return true, ""
			}
			return false, strings.Join(pending, "; ")
		})
}

// crdsNotEstablished reads the named CustomResourceDefinitions once and
// returns the ones that do not report Established — the single-read
// observation WaitForCRDsEstablished polls. Each returned entry names
// the CRD and why it is not Established, a missing CRD and a failed
// read included: within the wait they all mean "keep waiting", and on
// timeout the last observation becomes the failure message.
func crdsNotEstablished(ctx context.Context, dyn dynamic.Interface, names []string) []string {
	var pending []string
	for _, name := range names {
		crd, err := ReadCRD(ctx, dyn, name)
		if err != nil {
			pending = append(pending, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if established, why := CRDEstablished(crd); !established {
			pending = append(pending, fmt.Sprintf("%s: %s", name, why))
		}
	}
	return pending
}

// ReadCRD reads one CustomResourceDefinition by name — the read behind
// the Established waits and the verify checks that need the CRD object
// itself, not just its condition.
func ReadCRD(ctx context.Context, dyn dynamic.Interface, name string) (*unstructured.Unstructured, error) {
	return dyn.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
}

// CRDEstablished reads a CRD's Established condition, reporting whether
// it is True and, when it is not, why (the condition's status, or that
// the controller has not set it yet).
func CRDEstablished(crd *unstructured.Unstructured) (established bool, why string) {
	conditions, _, err := unstructured.NestedSlice(crd.Object, "status", "conditions")
	if err != nil {
		return false, fmt.Sprintf("reading conditions: %v", err)
	}
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Established" {
			continue
		}
		if m["status"] == "True" {
			return true, ""
		}
		return false, fmt.Sprintf("Established=%v", m["status"])
	}
	return false, "no Established condition yet"
}

// WaitForDeploymentRolledOut polls a Deployment until its current
// generation's rollout is complete — what `kubectl rollout status`
// waits for. Transient read errors are retried within the timeout.
//
// Availability alone is not rollout completion: a Recreate-strategy
// Deployment (OLM's operators use it) reports Available=True from its
// old replica while the replacement has not started, so a waiter that
// stops at the Available condition returns mid-rollout. This wait
// requires the full rollout-complete condition (DeploymentRolledOut).
func WaitForDeploymentRolledOut(ctx context.Context, cs kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	return pollCondition(ctx, timeout,
		fmt.Sprintf("rollout of deployment %s/%s", namespace, name),
		fmt.Sprintf("deployment %s/%s not rolled out", namespace, name),
		func(ctx context.Context) (bool, string) {
			d, err := cs.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err.Error()
			}
			return DeploymentRolledOut(d)
		})
}

// DeploymentRolledOut reports whether a Deployment's current
// generation has finished rolling out, and why not when it has not. It
// mirrors kubectl's rollout-status condition: the generation observed,
// every desired replica updated and available, and no replica left
// outside the updated set — a still-running old replica under Recreate,
// or a terminating one under RollingUpdate's surge. The generation
// guard matters: status fields set for an older generation describe a
// previous rollout, not this one.
func DeploymentRolledOut(d *appsv1.Deployment) (rolledOut bool, why string) {
	if d.Generation > d.Status.ObservedGeneration {
		return false, fmt.Sprintf("waiting for generation %d to be observed (observed %d)",
			d.Generation, d.Status.ObservedGeneration)
	}
	// A nil Spec.Replicas means one, the API's default.
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	switch {
	case d.Status.UpdatedReplicas != desired:
		return false, fmt.Sprintf("waiting for %d/%d updated replicas",
			d.Status.UpdatedReplicas, desired)
	case d.Status.AvailableReplicas != desired:
		return false, fmt.Sprintf("waiting for %d/%d available replicas",
			d.Status.AvailableReplicas, desired)
	case d.Status.Replicas != d.Status.UpdatedReplicas:
		return false, fmt.Sprintf("waiting for %d old replica(s) to go away",
			d.Status.Replicas-d.Status.UpdatedReplicas)
	}
	return true, ""
}

// DeploymentAvailable reports whether a Deployment is Available at its
// current generation, and why not when it is not. The generation guard
// matters: a condition set for an older generation describes a previous
// rollout, not this one. This is the availability assertion for Verify
// checks; rollout waits use DeploymentRolledOut, the stronger
// rollout-complete condition.
func DeploymentAvailable(d *appsv1.Deployment) (available bool, why string) {
	if d.Generation > d.Status.ObservedGeneration {
		return false, fmt.Sprintf("waiting for generation %d to be observed (observed %d)",
			d.Generation, d.Status.ObservedGeneration)
	}
	replicas := ""
	if d.Spec.Replicas != nil {
		replicas = fmt.Sprintf(", %d/%d replicas available", d.Status.AvailableReplicas, *d.Spec.Replicas)
	}
	for _, c := range d.Status.Conditions {
		if c.Type != appsv1.DeploymentAvailable {
			continue
		}
		if c.Status == corev1.ConditionTrue {
			return true, ""
		}
		why = fmt.Sprintf("Available=%s", c.Status)
		if c.Reason != "" {
			why += fmt.Sprintf(" (%s)", c.Reason)
		}
		if c.Message != "" {
			why += ": " + c.Message
		}
		return false, why + replicas
	}
	return false, "no Available condition yet" + replicas
}

// WaitForPodRunning polls one pod until it is Running — the condition
// the scc-admission layer's probe asserts: a pod the kubelet accepts
// runs, one it refuses stays Pending with the refusal named in its
// container statuses. Transient read errors are retried within the
// timeout and surface as the last observation, which includes the
// waiting containers' reasons (CreateContainerError names the
// runAsNonRoot refusal an unmutated probe pod gets).
func WaitForPodRunning(ctx context.Context, cs kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	return pollCondition(ctx, timeout,
		fmt.Sprintf("pod %s/%s running", namespace, name),
		fmt.Sprintf("pod %s/%s not Running", namespace, name),
		func(ctx context.Context) (bool, string) {
			pod, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return false, err.Error()
			}
			return podRunning(pod)
		})
}

// podRunning reports whether a pod is Running and, when it is not, the
// most useful observation: the phase plus every waiting container's
// reason and message.
func podRunning(pod *corev1.Pod) (running bool, observation string) {
	if pod.Status.Phase == corev1.PodRunning {
		return true, ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "phase %s", pod.Status.Phase)
	for i := range pod.Status.ContainerStatuses {
		waiting := pod.Status.ContainerStatuses[i].State.Waiting
		if waiting == nil || waiting.Reason == "" {
			continue
		}
		fmt.Fprintf(&b, ", container %s waiting: %s", pod.Status.ContainerStatuses[i].Name, waiting.Reason)
		if waiting.Message != "" {
			fmt.Fprintf(&b, ": %s", waiting.Message)
		}
	}
	return false, b.String()
}

// WaitForUIDGone polls one object until the UID the caller deleted no
// longer serves under its name, and reports what the deciding read
// saw. A DELETE call's return is a request, not a result — finalizers
// hold objects past it — and a 409 on the delete answers an admission
// refusal with the original still live as well as a replacement
// holding the name, so the proof of a deletion is a read: NotFound or
// a different UID proves the original instance gone (which is also
// why this wait, unlike a name-only one, does not fail when the
// original was deleted and a replacement now holds the name). read
// fetches the object by its name; held reports, once the original is
// gone, whether another instance holds the name (true) or the name is
// free (false). Transient read errors are retried within the timeout
// and surface as the last observation. The polling is pollCondition's
// — the one loop every wait shares, including its guard that rejects
// an answer arriving after the wait was over.
func WaitForUIDGone(
	ctx context.Context,
	read func(ctx context.Context) (metav1.Object, error),
	uid types.UID,
	timeout time.Duration,
) (held bool, err error) {
	err = pollCondition(ctx, timeout,
		"the deleted object going away",
		"the deleted object not gone",
		func(ctx context.Context) (bool, string) {
			obj, err := read(ctx)
			switch {
			case apierrors.IsNotFound(err):
				held = false
				return true, ""
			case err != nil:
				return false, err.Error()
			case obj.GetUID() != uid:
				held = true
				return true, ""
			default:
				observation := "still present"
				if deleted := obj.GetDeletionTimestamp(); deleted != nil {
					observation = fmt.Sprintf("still present (deletion timestamp %s)", deleted.UTC().Format(time.RFC3339))
				}
				return false, observation
			}
		})
	if err != nil {
		return false, err
	}
	return held, nil
}

// GroupVersionServed reports whether the API server serves the given
// group/version (for example "operators.coreos.com/v1alpha1"), by asking
// discovery for the served groups.
func GroupVersionServed(ctx context.Context, disc discovery.DiscoveryInterface, groupVersion string) (bool, error) {
	gv, err := schema.ParseGroupVersion(groupVersion)
	if err != nil {
		return false, fmt.Errorf("parsing group/version %q: %w", groupVersion, err)
	}
	groups, err := discovery.ToDiscoveryInterfaceWithContext(disc).ServerGroupsWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("discovering the server's API groups: %w", err)
	}
	for i := range groups.Groups {
		g := &groups.Groups[i]
		if g.Name != gv.Group {
			continue
		}
		for _, v := range g.Versions {
			if v.GroupVersion == gv.String() {
				return true, nil
			}
		}
	}
	return false, nil
}
