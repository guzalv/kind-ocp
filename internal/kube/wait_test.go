package kube

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newFakeDiscovery builds a discovery client serving the given resource
// lists.
func newFakeDiscovery(resources []*metav1.APIResourceList) *fakediscovery.FakeDiscovery {
	return &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{Resources: resources}}
}

// crdObject builds an unstructured CRD carrying an Established condition
// of the given status (none when empty).
func crdObject(name, established string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
	}}
	if established != "" {
		obj.Object["status"] = map[string]any{
			"conditions": []any{map[string]any{"type": "Established", "status": established}},
		}
	}
	return obj
}

// fakeResource answers Get through a function; everything else panics
// via the nil embedded interface.
type fakeResource struct {
	dynamic.ResourceInterface
	get func(ctx context.Context, name string) (*unstructured.Unstructured, error)
}

func (r *fakeResource) Namespace(string) dynamic.ResourceInterface { return r }

func (r *fakeResource) Get(ctx context.Context, name string, _ metav1.GetOptions, _ ...string) (*unstructured.Unstructured, error) {
	return r.get(ctx, name)
}

// fakeDynamic routes every GVR to one fake resource.
type fakeDynamic struct {
	dynamic.Interface
	resource dynamic.NamespaceableResourceInterface
}

func (d *fakeDynamic) Resource(_ schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return d.resource
}

func TestWaitForCRDsEstablished(t *testing.T) {
	t.Run("no names is a no-op", func(t *testing.T) {
		if err := WaitForCRDsEstablished(context.Background(), &fakeDynamic{}, time.Minute, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("established immediately", func(t *testing.T) {
		dyn := &fakeDynamic{resource: &fakeResource{get: func(_ context.Context, name string) (*unstructured.Unstructured, error) {
			return crdObject(name, "True"), nil
		}}}
		err := WaitForCRDsEstablished(context.Background(), dyn, time.Minute,
			[]string{"clusterserviceversions.operators.coreos.com", "catalogsources.operators.coreos.com"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("timeout names the CRD and its condition", func(t *testing.T) {
		dyn := &fakeDynamic{resource: &fakeResource{get: func(_ context.Context, name string) (*unstructured.Unstructured, error) {
			if name == "catalogsources.operators.coreos.com" {
				return crdObject(name, "False"), nil
			}
			return crdObject(name, "True"), nil
		}}}
		err := WaitForCRDsEstablished(context.Background(), dyn, 50*time.Millisecond,
			[]string{"clusterserviceversions.operators.coreos.com", "catalogsources.operators.coreos.com"})
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		for _, substr := range []string{"CRDs not Established", "catalogsources.operators.coreos.com", "Established=False"} {
			if !strings.Contains(err.Error(), substr) {
				t.Errorf("error %q does not mention %q", err, substr)
			}
		}
		if strings.Contains(err.Error(), "clusterserviceversions") {
			t.Errorf("error %q names an already-established CRD", err)
		}
	})

	t.Run("missing condition is reported", func(t *testing.T) {
		dyn := &fakeDynamic{resource: &fakeResource{get: func(_ context.Context, name string) (*unstructured.Unstructured, error) {
			return crdObject(name, ""), nil
		}}}
		err := WaitForCRDsEstablished(context.Background(), dyn, 50*time.Millisecond,
			[]string{"subscriptions.operators.coreos.com"})
		if err == nil || !strings.Contains(err.Error(), "no Established condition yet") {
			t.Fatalf("error = %v, want a missing-condition observation", err)
		}
	})

	t.Run("read errors are retried and reported", func(t *testing.T) {
		calls := 0
		dyn := &fakeDynamic{resource: &fakeResource{get: func(_ context.Context, name string) (*unstructured.Unstructured, error) {
			calls++
			return nil, apierrors.NewNotFound(crdGVR.GroupResource(), name)
		}}}
		// Longer than one poll interval, so the retry is observable.
		err := WaitForCRDsEstablished(context.Background(), dyn, 1500*time.Millisecond,
			[]string{"olmconfigs.operators.coreos.com"})
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("error = %v, want the read error as the observation", err)
		}
		if calls < 2 {
			t.Errorf("read %d times, want the error retried within the timeout", calls)
		}
	})

	t.Run("becomes established after polling", func(t *testing.T) {
		var established atomic.Bool
		dyn := &fakeDynamic{resource: &fakeResource{get: func(_ context.Context, name string) (*unstructured.Unstructured, error) {
			status := "False"
			if established.Load() {
				status = "True"
			}
			return crdObject(name, status), nil
		}}}
		go func() {
			// The CRD flips to Established shortly after the first poll.
			time.Sleep(1500 * time.Millisecond)
			established.Store(true)
		}()
		err := WaitForCRDsEstablished(context.Background(), dyn, 30*time.Second,
			[]string{"olmconfigs.operators.coreos.com"})
		if err != nil {
			t.Fatalf("expected establishment after polling: %v", err)
		}
	})

	t.Run("context cancellation wins", func(t *testing.T) {
		dyn := &fakeDynamic{resource: &fakeResource{get: func(_ context.Context, name string) (*unstructured.Unstructured, error) {
			return crdObject(name, "False"), nil
		}}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := WaitForCRDsEstablished(ctx, dyn, time.Minute, []string{"olmconfigs.operators.coreos.com"})
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

// TestWaitForUIDGone pins the deletion-confirmation primitive directly
// (the nodeport regressions exercise it only through the check's
// cleanup): the two proofs of absence, the timeout's last observation,
// the retry of read errors, the immediate first read, and the two ways
// the wait refuses to be fooled — a cancelled caller context and an
// answer that arrives after the wait was over.
func TestWaitForUIDGone(t *testing.T) {
	const deletedUID = types.UID("uid-deleted")
	// goneObject is the object the caller deleted, optionally with the
	// deletion timestamp a finalizer-holding object serves past its
	// accepted delete.
	goneObject := func(uid types.UID, deleting bool) *corev1.Namespace {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "gone-check", UID: uid}}
		if deleting {
			now := metav1.Now()
			ns.DeletionTimestamp = &now
		}
		return ns
	}
	notFound := func(context.Context) (metav1.Object, error) {
		return nil, apierrors.NewNotFound(corev1.Resource("namespaces"), "gone-check")
	}

	t.Run("not found is gone and the name is free", func(t *testing.T) {
		held, err := WaitForUIDGone(context.Background(), notFound, deletedUID, time.Minute)
		if err != nil || held {
			t.Fatalf("WaitForUIDGone = (held %v, %v), want (false, nil)", held, err)
		}
	})

	t.Run("a different UID is gone with the name held", func(t *testing.T) {
		replacement := func(context.Context) (metav1.Object, error) {
			return goneObject("uid-replacement", false), nil
		}
		held, err := WaitForUIDGone(context.Background(), replacement, deletedUID, time.Minute)
		if err != nil || !held {
			t.Fatalf("WaitForUIDGone = (held %v, %v), want (true, nil)", held, err)
		}
	})

	t.Run("the same UID until the timeout reports the last observation", func(t *testing.T) {
		terminating := func(context.Context) (metav1.Object, error) {
			return goneObject(deletedUID, true), nil
		}
		held, err := WaitForUIDGone(context.Background(), terminating, deletedUID, 100*time.Millisecond)
		if err == nil || held {
			t.Fatalf("WaitForUIDGone = (held %v, %v), want (false, a timeout error)", held, err)
		}
		for _, substr := range []string{"not gone within 100ms", "still present", "deletion timestamp"} {
			if !strings.Contains(err.Error(), substr) {
				t.Errorf("error %q does not mention %q", err, substr)
			}
		}
	})

	t.Run("read errors are retried and the last one reported", func(t *testing.T) {
		calls := 0
		read := func(context.Context) (metav1.Object, error) {
			calls++
			return nil, errors.New("connection reset by peer")
		}
		// Longer than one poll interval, so the retry is observable.
		held, err := WaitForUIDGone(context.Background(), read, deletedUID, 1500*time.Millisecond)
		if err == nil || held {
			t.Fatalf("WaitForUIDGone = (held %v, %v), want (false, the read error as the observation)", held, err)
		}
		if !strings.Contains(err.Error(), "connection reset by peer") {
			t.Errorf("error %q does not carry the read error", err)
		}
		if calls < 2 {
			t.Errorf("read %d times, want the error retried within the timeout", calls)
		}
	})

	t.Run("the first read is immediate", func(t *testing.T) {
		start := time.Now()
		if _, err := WaitForUIDGone(context.Background(), notFound, deletedUID, time.Minute); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if elapsed := time.Since(start); elapsed >= pollInterval {
			t.Errorf("waited %s before deciding, want the first read immediate (poll interval %s)", elapsed, pollInterval)
		}
	})

	t.Run("an answer racing past the deadline is not accepted", func(t *testing.T) {
		// The read lands after the wait was over — a response that
		// raced cancellation. The success it reports must be rejected,
		// which is pollCondition's guard, not a second loop's.
		late := func(ctx context.Context) (metav1.Object, error) {
			time.Sleep(200 * time.Millisecond)
			return nil, apierrors.NewNotFound(corev1.Resource("namespaces"), "gone-check")
		}
		held, err := WaitForUIDGone(context.Background(), late, deletedUID, 100*time.Millisecond)
		if err == nil {
			t.Fatal("expected the late answer to be rejected, got success")
		}
		if held {
			t.Error("held = true, want the late answer to report nothing")
		}
		if !strings.Contains(err.Error(), "not accepted") {
			t.Errorf("error %q does not name the rejected late answer", err)
		}
	})

	t.Run("context cancellation wins", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stillThere := func(context.Context) (metav1.Object, error) {
			return goneObject(deletedUID, false), nil
		}
		held, err := WaitForUIDGone(ctx, stillThere, deletedUID, time.Minute)
		if err == nil || held {
			t.Fatalf("WaitForUIDGone = (held %v, %v), want (false, context.Canceled)", held, err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	})
}

func deployment(mutate func(*appsv1.Deployment)) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "olm-operator", Namespace: "olm", Generation: 1},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(1))},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 1},
	}
	if mutate != nil {
		mutate(d)
	}
	return d
}

func ptr[T any](v T) *T { return &v }

func TestDeploymentAvailable(t *testing.T) {
	t.Run("available at the current generation", func(t *testing.T) {
		d := deployment(func(d *appsv1.Deployment) {
			d.Status.ObservedGeneration = 1
			d.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
			}}
		})
		if ok, why := DeploymentAvailable(d); !ok {
			t.Errorf("DeploymentAvailable = false (%q), want true", why)
		}
	})

	t.Run("condition for an older generation is not available", func(t *testing.T) {
		d := deployment(func(d *appsv1.Deployment) {
			d.Generation = 2
			d.Status.ObservedGeneration = 1
			d.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
			}}
		})
		if ok, why := DeploymentAvailable(d); ok || !strings.Contains(why, "generation") {
			t.Errorf("DeploymentAvailable = (%v, %q), want a stale-generation refusal", ok, why)
		}
	})

	t.Run("unavailable condition carries reason, message, and replicas", func(t *testing.T) {
		d := deployment(func(d *appsv1.Deployment) {
			d.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:    appsv1.DeploymentAvailable,
				Status:  corev1.ConditionFalse,
				Reason:  "MinimumReplicasUnavailable",
				Message: "Deployment does not have minimum availability.",
			}}
		})
		ok, why := DeploymentAvailable(d)
		if ok {
			t.Fatal("DeploymentAvailable = true, want false")
		}
		for _, substr := range []string{"Available=False", "MinimumReplicasUnavailable", "0/1 replicas"} {
			if !strings.Contains(why, substr) {
				t.Errorf("why %q does not mention %q", why, substr)
			}
		}
	})

	t.Run("missing condition", func(t *testing.T) {
		d := deployment(nil)
		if ok, why := DeploymentAvailable(d); ok || !strings.Contains(why, "no Available condition") {
			t.Errorf("DeploymentAvailable = (%v, %q), want a missing-condition refusal", ok, why)
		}
	})
}

func TestDeploymentRolledOut(t *testing.T) {
	rolledOut := func(d *appsv1.Deployment) {
		t.Helper()
		if ok, why := DeploymentRolledOut(d); !ok {
			t.Errorf("DeploymentRolledOut = false (%q), want true", why)
		}
	}

	t.Run("updated, available, no old replicas", func(t *testing.T) {
		rolledOut(deployment(func(d *appsv1.Deployment) {
			d.Status.UpdatedReplicas = 1
			d.Status.AvailableReplicas = 1
			d.Status.Replicas = 1
		}))
	})

	t.Run("scaled to zero", func(t *testing.T) {
		rolledOut(deployment(func(d *appsv1.Deployment) {
			d.Spec.Replicas = ptr(int32(0))
		}))
	})

	t.Run("nil replicas defaults to one", func(t *testing.T) {
		rolledOut(deployment(func(d *appsv1.Deployment) {
			d.Spec.Replicas = nil
			d.Status.UpdatedReplicas = 1
			d.Status.AvailableReplicas = 1
			d.Status.Replicas = 1
		}))
	})

	t.Run("generation not observed", func(t *testing.T) {
		d := deployment(func(d *appsv1.Deployment) {
			d.Generation = 2
			d.Status.ObservedGeneration = 1
		})
		if ok, why := DeploymentRolledOut(d); ok || !strings.Contains(why, "generation") {
			t.Errorf("DeploymentRolledOut = (%v, %q), want a stale-generation refusal", ok, why)
		}
	})

	// The increment-2 review finding, verbatim: a Recreate-strategy
	// Deployment whose pod template changed reports generation=2,
	// observedGeneration=2, replicas=1, updatedReplicas=0,
	// availableReplicas=1, and Available=True while the old replica is
	// still the only one running — the replacement has not started.
	// Availability alone called this rollout complete.
	t.Run("available from the old replica is not rolled out", func(t *testing.T) {
		d := deployment(func(d *appsv1.Deployment) {
			d.Generation = 2
			d.Status.ObservedGeneration = 2
			d.Status.Replicas = 1
			d.Status.UpdatedReplicas = 0
			d.Status.AvailableReplicas = 1
			d.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
			}}
		})
		ok, why := DeploymentRolledOut(d)
		if ok {
			t.Fatal("DeploymentRolledOut = true for the review's mid-rollout status, want false")
		}
		if !strings.Contains(why, "0/1 updated") {
			t.Errorf("why = %q, want the updated-replica shortfall", why)
		}
		// The Available condition must not satisfy the rollout check.
		if available, _ := DeploymentAvailable(d); !available {
			t.Error("fixture sanity: the mid-rollout status must be Available, or the regression tests the wrong thing")
		}
	})

	t.Run("surged deployment with old replica terminating", func(t *testing.T) {
		d := deployment(func(d *appsv1.Deployment) {
			d.Status.UpdatedReplicas = 1
			d.Status.AvailableReplicas = 1
			d.Status.Replicas = 2
		})
		ok, why := DeploymentRolledOut(d)
		if ok || !strings.Contains(why, "old replica") {
			t.Errorf("DeploymentRolledOut = (%v, %q), want an old-replica refusal", ok, why)
		}
	})

	t.Run("updated but not yet available", func(t *testing.T) {
		d := deployment(func(d *appsv1.Deployment) {
			d.Status.UpdatedReplicas = 1
			d.Status.AvailableReplicas = 0
			d.Status.Replicas = 1
		})
		ok, why := DeploymentRolledOut(d)
		if ok || !strings.Contains(why, "0/1 available") {
			t.Errorf("DeploymentRolledOut = (%v, %q), want an available-replica shortfall", ok, why)
		}
	})
}

func TestWaitForDeploymentRolledOut(t *testing.T) {
	t.Run("rolled out immediately", func(t *testing.T) {
		cs := fake.NewSimpleClientset(deployment(func(d *appsv1.Deployment) {
			d.Status.UpdatedReplicas = 1
			d.Status.AvailableReplicas = 1
			d.Status.Replicas = 1
		}))
		if err := WaitForDeploymentRolledOut(context.Background(), cs, "olm", "olm-operator", time.Minute); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	// The waiter must keep polling through the mid-rollout status that
	// is simultaneously Available: the poll returning nil here was the
	// increment-2 review finding.
	t.Run("available old replica is polled through, not accepted", func(t *testing.T) {
		cs := fake.NewSimpleClientset(deployment(func(d *appsv1.Deployment) {
			d.Generation = 2
			d.Status.ObservedGeneration = 2
			d.Status.Replicas = 1
			d.Status.UpdatedReplicas = 0
			d.Status.AvailableReplicas = 1
			d.Status.Conditions = []appsv1.DeploymentCondition{{
				Type:   appsv1.DeploymentAvailable,
				Status: corev1.ConditionTrue,
			}}
		}))
		err := WaitForDeploymentRolledOut(context.Background(), cs, "olm", "olm-operator", 50*time.Millisecond)
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		for _, substr := range []string{"deployment olm/olm-operator not rolled out", "0/1 updated replicas"} {
			if !strings.Contains(err.Error(), substr) {
				t.Errorf("error %q does not mention %q", err, substr)
			}
		}
	})

	t.Run("timeout names the deployment and why", func(t *testing.T) {
		cs := fake.NewSimpleClientset(deployment(func(d *appsv1.Deployment) {
			d.Status.UpdatedReplicas = 1
			d.Status.AvailableReplicas = 0
			d.Status.Replicas = 1
		}))
		err := WaitForDeploymentRolledOut(context.Background(), cs, "olm", "olm-operator", 50*time.Millisecond)
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		for _, substr := range []string{"deployment olm/olm-operator not rolled out", "0/1 available replicas"} {
			if !strings.Contains(err.Error(), substr) {
				t.Errorf("error %q does not mention %q", err, substr)
			}
		}
	})

	t.Run("missing deployment is retried then reported", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		err := WaitForDeploymentRolledOut(context.Background(), cs, "olm", "catalog-operator", 50*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("error = %v, want the read error as the observation", err)
		}
	})

	t.Run("becomes rolled out after polling", func(t *testing.T) {
		d := deployment(nil)
		cs := fake.NewSimpleClientset(d)
		go func() {
			time.Sleep(1500 * time.Millisecond)
			d.Status.UpdatedReplicas = 1
			d.Status.AvailableReplicas = 1
			d.Status.Replicas = 1
			if _, err := cs.AppsV1().Deployments("olm").UpdateStatus(context.Background(), d, metav1.UpdateOptions{}); err != nil {
				t.Error(err)
			}
		}()
		if err := WaitForDeploymentRolledOut(context.Background(), cs, "olm", "olm-operator", 30*time.Second); err != nil {
			t.Fatalf("expected rollout completion after polling: %v", err)
		}
	})

	t.Run("context cancellation wins", func(t *testing.T) {
		cs := fake.NewSimpleClientset(deployment(nil))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := WaitForDeploymentRolledOut(ctx, cs, "olm", "olm-operator", time.Minute)
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

func TestGroupVersionServed(t *testing.T) {
	served := func() *fakediscovery.FakeDiscovery {
		return newFakeDiscovery([]*metav1.APIResourceList{
			{GroupVersion: "operators.coreos.com/v1alpha1", APIResources: []metav1.APIResource{
				{Name: "clusterserviceversions", Kind: "ClusterServiceVersion", Namespaced: true},
			}},
			{GroupVersion: "operators.coreos.com/v1", APIResources: []metav1.APIResource{
				{Name: "olmconfigs", Kind: "OLMConfig", Namespaced: false},
			}},
		})
	}

	t.Run("served group and version", func(t *testing.T) {
		ok, err := GroupVersionServed(context.Background(), served(), "operators.coreos.com/v1alpha1")
		if err != nil || !ok {
			t.Fatalf("GroupVersionServed = (%v, %v), want (true, nil)", ok, err)
		}
	})

	t.Run("group present, version missing", func(t *testing.T) {
		ok, err := GroupVersionServed(context.Background(), served(), "operators.coreos.com/v2")
		if err != nil || ok {
			t.Fatalf("GroupVersionServed = (%v, %v), want (false, nil)", ok, err)
		}
	})

	t.Run("group absent", func(t *testing.T) {
		ok, err := GroupVersionServed(context.Background(), served(), "config.openshift.io/v1")
		if err != nil || ok {
			t.Fatalf("GroupVersionServed = (%v, %v), want (false, nil)", ok, err)
		}
	})

	t.Run("core group without a name", func(t *testing.T) {
		disc := newFakeDiscovery([]*metav1.APIResourceList{
			{GroupVersion: "v1", APIResources: []metav1.APIResource{
				{Name: "namespaces", Kind: "Namespace", Namespaced: false},
			}},
		})
		ok, err := GroupVersionServed(context.Background(), disc, "v1")
		if err != nil || !ok {
			t.Fatalf("GroupVersionServed(v1) = (%v, %v), want (true, nil)", ok, err)
		}
	})

	t.Run("discovery failure is an error", func(t *testing.T) {
		disc := served()
		disc.PrependReactor("get", "group", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("apiserver unreachable")
		})
		_, err := GroupVersionServed(context.Background(), disc, "operators.coreos.com/v1alpha1")
		if err == nil || !strings.Contains(err.Error(), "discovering") {
			t.Fatalf("error = %v, want a discovery error", err)
		}
	})
}
