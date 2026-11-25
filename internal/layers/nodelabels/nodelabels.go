// Package nodelabels implements the node-labels layer: it labels every
// control-plane node with node-role.kubernetes.io/master, the node-role
// label that operator Deployments created through OLM — OpenShift's own
// operators and the Compliance Operator among them — schedule against.
//
// Ported from one line of the PoC's install_co_olm (stackrox PR
// #23841): `kubectl label node -l node-role.kubernetes.io/control-plane
// node-role.kubernetes.io/master=""`. OpenShift's control-plane nodes
// carry both role labels; kind's control-plane node carries only the
// control-plane one (kubeadm dropped the master label), so an OLM
// operator whose CSV selects the master role has nowhere to schedule.
// The layer bridges that gap, which is why it runs before olm in the
// registration order.
//
// The desired state is the label with the empty-string value — the
// exact pair the CSV nodeSelectors that schedule OpenShift's operators
// (the Compliance Operator among them) match. A node carrying a
// non-empty value schedules those Deployments as little as an
// unlabeled one, so Apply and Verify enforce the value, not just the
// key's presence.
//
// The label is applied as a server-side apply through client-go's
// typed Node apply: a partial Node configuration carrying only the
// name and the label, so the apply claims exactly the
// metadata.labels master key under kind-ocp's field manager (with
// force, the same apply identity kube.Client.Apply uses) and touches
// nothing else on the node. The typed path needs no discovery or REST
// mapping — Node is a built-in kind with a fixed resource path.
//
// The layer has no settings: the two label keys and the value are
// fixed by the OpenShift compatibility contract, and no documented
// use case overrides them. Verify's read is bounded by the fixed
// readTimeout.
package nodelabels

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	applycorev1 "k8s.io/client-go/applyconfigurations/core/v1"

	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/layers"
)

// Name is the layer's stable identifier, as referenced by profiles and
// the config file.
const Name = "node-labels"

// The two node-role labels: the selector that finds kind's
// control-plane nodes, and the OpenShift role label the layer adds to
// them.
const (
	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
	masterLabel       = "node-role.kubernetes.io/master"
)

// readTimeout bounds Verify's node read. A fixed constant, not a
// setting: AC-3.1 requires bounded waits, and no documented use case
// overrides a budget.
const readTimeout = 2 * time.Minute

// Layer labels control-plane nodes with the master role. It is
// stateless; construct once.
type Layer struct{}

// Name returns "node-labels".
func (Layer) Name() string { return Name }

// Apply converges the cluster to the layer's desired state: every
// control-plane node carries the master role label with the
// empty-string value. Nodes that already carry it are left alone — a
// re-run after a partial failure converges only what deviates, and the
// server-side apply of an unchanged label would be a no-op anyway.
func (l Layer) Apply(ctx context.Context, env layers.Env) error {
	client, err := kube.NewClient(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	nodes, err := controlPlaneNodes(ctx, client)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("no nodes carry the %s label; a kind cluster always has a control-plane node", controlPlaneLabel)
	}
	var unlabeled []string
	for i := range nodes {
		// The same presence-and-value check Verify enforces: a missing
		// label and a non-empty one are both deviations from the
		// converged master="" pair.
		if value, ok := nodes[i].Labels[masterLabel]; !ok || value != "" {
			unlabeled = append(unlabeled, nodes[i].Name)
		}
	}
	if len(unlabeled) == 0 {
		env.Log.Info("control-plane nodes already carry the master role label", "nodes", len(nodes))
		return nil
	}
	env.Log.Info("labeling control-plane nodes with the master role", "nodes", strings.Join(unlabeled, ", "))
	for _, name := range unlabeled {
		if _, err := client.Clientset.CoreV1().Nodes().Apply(ctx, masterLabelApply(name),
			metav1.ApplyOptions{FieldManager: kube.LayerFieldManager(Name), Force: true}); err != nil {
			return fmt.Errorf("labeling control-plane node %s: %w", name, err)
		}
	}
	return nil
}

// Verify checks the layer's postconditions and mutates nothing: every
// control-plane node carries the master role label with the
// empty-string value. The read is bounded by readTimeout.
func (l Layer) Verify(ctx context.Context, env layers.Env) error {
	return l.verify(ctx, env, readTimeout)
}

// verify is Verify with an explicit read budget. Verify passes the
// production bound (readTimeout); the bounded-read regression test
// drives this helper with a short budget, which is why the budget is a
// parameter and not a setting.
func (l Layer) verify(ctx context.Context, env layers.Env, readBudget time.Duration) error {
	client, err := kube.NewClient(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	// The read runs under a deadline context: the caller's context can
	// be unbounded (the CLI passes context.Background), so an
	// apiserver that wedges the response must not outlive the budget.
	readCtx, cancel := context.WithTimeout(ctx, readBudget)
	nodes, err := controlPlaneNodes(readCtx, client)
	cancel()
	if err != nil {
		return err
	}
	return checkMasterLabel(nodes)
}

// controlPlaneNodes lists the cluster's control-plane nodes, selected
// the way the PoC selected them: by the control-plane role label.
func controlPlaneNodes(ctx context.Context, client *kube.Client) ([]corev1.Node, error) {
	nodes, err := client.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: controlPlaneLabel})
	if err != nil {
		return nil, fmt.Errorf("listing control-plane nodes: %w", err)
	}
	return nodes.Items, nil
}

// checkMasterLabel reports whether every node in the list carries the
// master role label with the empty-string value, naming the ones that
// do not and how each one deviates.
func checkMasterLabel(nodes []corev1.Node) error {
	if len(nodes) == 0 {
		return errors.New("no nodes carry the control-plane label; a kind cluster always has one")
	}
	var bad []string
	for i := range nodes {
		value, ok := nodes[i].Labels[masterLabel]
		switch {
		case !ok:
			bad = append(bad, nodes[i].Name+" (label missing)")
		case value != "":
			bad = append(bad, fmt.Sprintf("%s (label value %q)", nodes[i].Name, value))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("control-plane nodes not carrying %s=\"\": %s — re-run create to converge them",
			masterLabel, strings.Join(bad, ", "))
	}
	return nil
}

// masterLabelApply builds the typed apply configuration that converges
// one node's master role label: a partial Node carrying only the name
// and that one label, so the server-side apply claims exactly the
// metadata.labels key.
func masterLabelApply(name string) *applycorev1.NodeApplyConfiguration {
	return applycorev1.Node(name).WithLabels(map[string]string{masterLabel: ""})
}
