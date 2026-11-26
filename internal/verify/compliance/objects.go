package compliance

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/guzalv/kind-ocp/internal/kube"
)

// fieldManager is the proof's server-side-apply ownership identity:
// kind-ocp's prefix plus the proof's own name, distinct from every
// layer's (kube.LayerFieldManager), so the proof's applies own
// exactly the fields it renders and nothing else's.
const fieldManager = kube.FieldManager + "/compliance-proof"

// namespaceObjects renders the two namespaces the proof's objects
// live in: the operator's own and OLM's global catalog namespace.
// Both are converges — on a cluster the layers built they already
// exist, and applying them again is a no-op; applying them anyway
// keeps the proof's failure modes in the waits that follow, where the
// messages say what is missing, instead of in a REST-mapping error.
func namespaceObjects() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		namespaceObject(operatorNamespace),
		namespaceObject(catalogNamespace),
	}
}

func namespaceObject(name string) *unstructured.Unstructured {
	return mustUnstructured(&corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	})
}

// installObjects renders the OLM install: the CatalogSource in olm
// (the global catalog namespace OLM resolves Subscriptions against),
// and the OperatorGroup and Subscription in the operator namespace.
// The CatalogSource points at the proof's own catalog server — the
// Service the catalog objects expose — and not at any registry image:
// the catalog's content is committed in this repo, and the address is
// the only thing that selects it. There is no catalog-image override
// path (a different catalog means a different proof, on a fresh
// cluster). The Subscription carries no config.env — the committed
// catalog's CSV ignores those overrides, and the operator's own
// --platform flag already defaults to OpenShift (see the package
// doc).
func installObjects() []*unstructured.Unstructured {
	return []*unstructured.Unstructured{
		catalogSource(),
		operatorGroup(),
		subscription(),
	}
}

// catalogObjects renders the proof's catalog server: the ConfigMap
// carrying the assembled catalog, the Pod serving it, and the Service
// the CatalogSource addresses. The pod runs the pinned catalog image's
// own opm binary against the committed content — the image contributes
// the server, the repo contributes the catalog — with the catalog
// mounted as one file inside an otherwise-empty directory (see
// catalogServerPod). The pod needs no API access (its service account
// token is not mounted), exposes the port OLM's own catalog servers
// use, and reports readiness only when the gRPC health probe passes,
// which is the signal the proof waits on before it installs anything.
func catalogObjects() ([]*unstructured.Unstructured, error) {
	catalog, err := renderCatalog()
	if err != nil {
		return nil, err
	}
	return []*unstructured.Unstructured{
		catalogConfigMap(catalog),
		catalogService(),
		catalogServerPod(),
	}, nil
}

// catalogConfigMap carries the assembled file-based catalog under one
// key; the serving pod's ConfigMap volume materializes it as one file,
// which is the directory opm serves.
func catalogConfigMap(catalog string) *unstructured.Unstructured {
	return mustUnstructured(&corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: catalogServerName, Namespace: catalogNamespace, Labels: catalogLabels()},
		Data:       map[string]string{catalogDataKey: catalog},
	})
}

// catalogService exposes the serving pod inside the cluster, on the
// port and at the name the CatalogSource's address names.
func catalogService() *unstructured.Unstructured {
	return mustUnstructured(&corev1.Service{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{Name: catalogServerName, Namespace: catalogNamespace, Labels: catalogLabels()},
		Spec: corev1.ServiceSpec{
			Selector: catalogLabels(),
			Ports: []corev1.ServicePort{{
				Name:       "grpc",
				Port:       catalogServerPort,
				TargetPort: intstr.FromInt32(catalogServerPort),
			}},
		},
	})
}

// catalogServerPod runs the pinned catalog image's opm against the
// committed catalog. Command and arguments override the image's own
// entrypoint (which serves the image's baked-in upstream catalog) so
// the same pinned binary serves this repo's content instead; the
// probes mirror what OLM asks of its own catalog pods, so Ready means
// the server answers gRPC health checks. The security context is the
// olm namespace's own PodSecurity standard (restricted, which OLM's
// release labels onto it): the image runs as its own non-root user
// (1001) and needs no capabilities. The catalog reaches the server as
// one subPath-mounted file inside an otherwise-empty directory — a
// whole ConfigMap volume's kubelet-managed layout (the ..data symlink
// and its timestamped directory) reads as duplicate catalog content to
// opm's walker, so the served directory must contain the file and
// nothing else.
func catalogServerPod() *unstructured.Unstructured {
	probe := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"grpc_health_probe", "-addr=:50051", "-rpc-timeout=5s"}},
		},
		InitialDelaySeconds: 5,
		TimeoutSeconds:      5,
		PeriodSeconds:       10,
		FailureThreshold:    30,
	}
	noToken := false
	runAsNonRoot := true
	noEscalation := false
	return mustUnstructured(&corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: catalogServerName, Namespace: catalogNamespace, Labels: catalogPodLabels()},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &noToken,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &runAsNonRoot,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "catalog",
				Image:           DefaultCatalogServerImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/bin/opm"},
				Args:            []string{"serve", catalogMountPath},
				Ports:           []corev1.ContainerPort{{Name: "grpc", ContainerPort: catalogServerPort}},
				ReadinessProbe:  probe,
				LivenessProbe:   probe,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					RunAsNonRoot:             &runAsNonRoot,
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "serving", MountPath: catalogMountPath},
					{Name: "catalog", MountPath: catalogMountPath + "/" + catalogDataKey, SubPath: catalogDataKey, ReadOnly: true},
				},
			}},
			Volumes: []corev1.Volume{
				{Name: "serving", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "catalog", VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: catalogServerName},
					},
				}},
			},
		},
	})
}

// catalogLabels is the selector tying the Service to the serving pod.
func catalogLabels() map[string]string {
	return map[string]string{"app": catalogServerName}
}

// catalogPodLabels is the serving pod's own label set: the selector
// labels plus the label OLM's release uses to admit catalog servers
// through its own ingress policy. The olm namespace is default-deny
// (OLM's release installs that policy beside one that allows ingress
// on the catalog port to pods labeled olm.catalogSource), so a server
// without the label is unreachable from the catalog operator and every
// connection times out — the label is OLM's documented extension point
// for exactly this shape, and its value is OLM's own convention: the
// CatalogSource's name.
func catalogPodLabels() map[string]string {
	labels := catalogLabels()
	labels["olm.catalogSource"] = catalogSourceName
	return labels
}

func catalogSource() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata":   map[string]any{"name": catalogSourceName, "namespace": catalogNamespace},
		"spec": map[string]any{
			"sourceType":  "grpc",
			"address":     catalogAddress(),
			"displayName": "Compliance Operator (kind-ocp proof)",
		},
	}}
}

func operatorGroup() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operators.coreos.com/v1",
		"kind":       "OperatorGroup",
		"metadata":   map[string]any{"name": operatorGroupName, "namespace": operatorNamespace},
		"spec":       map[string]any{"targetNamespaces": []string{operatorNamespace}},
	}}
}

// subscription renders the OLM Subscription. Its package and channel
// are the catalog's own constants (catalog.go), so the Subscription
// cannot drift from the catalog the proof emits; its source is the
// proof's own catalog server; and it carries no config.env (see the
// package doc).
func subscription() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]any{"name": subscriptionName, "namespace": operatorNamespace},
		"spec": map[string]any{
			"channel":             catalogChannel,
			"name":                catalogPackageName,
			"source":              catalogSourceName,
			"sourceNamespace":     catalogNamespace,
			"installPlanApproval": "Automatic",
		},
	}}
}

// scanObjects renders the proof's scan: one ScanSetting of its own —
// raw result storage off, so the scan lays down no PVC and no result
// server, and the results arrive as ComplianceCheckResults carried by
// ConfigMaps — and one ScanSettingBinding tying the ocp4-cis Profile
// to it. The binding's name is the ComplianceSuite's name (the
// binding controller names the suite after the binding), which is the
// label the results carry; the proof counts exactly its own scan's
// results and none of anything else's.
func scanObjects() []*unstructured.Unstructured {
	apiGroup := "compliance.openshift.io/v1alpha1"
	return []*unstructured.Unstructured{
		{
			Object: map[string]any{
				"apiVersion": apiGroup,
				"kind":       "ScanSetting",
				"metadata":   map[string]any{"name": scanSettingName, "namespace": operatorNamespace},
				// No schedule: the suite runs once, which is all the
				// proof needs. No roles: ocp4-cis is a platform
				// profile, and node scans cannot run on kind nodes
				// anyway (BRIEF §6).
				"rawResultStorage": map[string]any{"enabled": false},
			},
		},
		{
			Object: map[string]any{
				"apiVersion": apiGroup,
				"kind":       "ScanSettingBinding",
				"metadata":   map[string]any{"name": bindingName, "namespace": operatorNamespace},
				"profiles": []any{map[string]any{
					"name":     profileName,
					"kind":     "Profile",
					"apiGroup": apiGroup,
				}},
				"settingsRef": map[string]any{
					"name":     scanSettingName,
					"kind":     "ScanSetting",
					"apiGroup": apiGroup,
				},
			},
		},
	}
}

// mustUnstructured converts one typed object to the unstructured form
// the apply path takes. It cannot fail for the objects this package
// renders, so the callers stay linear.
func mustUnstructured(obj any) *unstructured.Unstructured {
	converted, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(fmt.Sprintf("compliance: rendering %T: %v", obj, err))
	}
	return &unstructured.Unstructured{Object: converted}
}

// servingCertValidity is how long the proof's certificate stays
// valid. The secret is created once, on a fresh cluster, and never
// renewed or replaced (the proof is one-shot), so the value only has
// to comfortably outlive the disposable cluster it serves.
const servingCertValidity = 10 * 365 * 24 * time.Hour

// servingCertDNSNames are the names the operator's metrics Service
// certificate would cover on a real cluster. The operator only
// requires the secret to exist; a real certificate costs nothing and
// keeps the secret honest against any version that reads it.
func servingCertDNSNames() []string {
	return []string{
		"metrics",
		"metrics." + operatorNamespace,
		"metrics." + operatorNamespace + ".svc",
		"metrics." + operatorNamespace + ".svc.cluster.local",
	}
}

// requireFreshCluster is the proof's one-shot guard: it refuses a
// cluster that already carries the serving-cert secret. That secret
// is the proof's marker — nothing else creates it (a real cluster's
// service-ca injects it; kind has no service-ca), and the proof
// creates it before anything that could start the operator, so a
// cluster carrying it has carried the proof before. A second proof
// there would satisfy every wait with the first run's stale CSV,
// READY binding, and PASS/FAIL results, and report a scan it never
// started — so it is refused, and the error names the recovery: the
// proof's clusters are disposable by contract.
func requireFreshCluster(ctx context.Context, cs kubernetes.Interface) error {
	_, err := cs.CoreV1().Secrets(operatorNamespace).Get(ctx, servingCertSecret, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("the %s/%s secret already exists: the compliance proof is one-shot on a fresh cluster, and this one has carried it before — an earlier run's Subscription, CSV, scan binding, and results would satisfy every wait without a new scan; delete and re-create the cluster, or prove a different one", operatorNamespace, servingCertSecret)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading the %s/%s secret: %w", operatorNamespace, servingCertSecret, err)
	}
	return nil
}

// createServingCert creates the serving-cert secret the operator's
// startup requires, with a Go-generated self-signed certificate (the
// PoC ran openssl on the host — here the material never touches a
// file). Created, never renewed or replaced: requireFreshCluster has
// already established the secret absent, and a create that still
// collides fails the proof — the safe direction.
func createServingCert(ctx context.Context, cs kubernetes.Interface) error {
	certPEM, keyPEM, err := generateServingCert(time.Now())
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
		ObjectMeta: metav1.ObjectMeta{Name: servingCertSecret, Namespace: operatorNamespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
		},
	}
	if _, err := cs.CoreV1().Secrets(operatorNamespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the %s/%s secret: %w", operatorNamespace, servingCertSecret, err)
	}
	return nil
}

// validateServingCert reports whether a keypair is usable: the
// certificate parses, is properly self-signed, is currently valid,
// and matches the private key. The proof itself never re-reads the
// secret it creates — this is the tests' oracle for the secret the
// proof lands.
func validateServingCert(data map[string][]byte, now time.Time) error {
	certPEM, keyPEM := data[corev1.TLSCertKey], data[corev1.TLSPrivateKeyKey]
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return errors.New("the secret carries no certificate or no key")
	}
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return errors.New("the secret's certificate does not decode as PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parsing the secret's certificate: %w", err)
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		return fmt.Errorf("the secret's certificate is not properly self-signed: %w", err)
	}
	if now.Before(cert.NotBefore) {
		return errors.New("the secret's certificate is not valid yet")
	}
	if now.After(cert.NotAfter) {
		return errors.New("the secret's certificate has expired")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return errors.New("the secret's key does not decode as PEM")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("parsing the secret's key: %w", err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		return errors.New("the secret's certificate does not match its key")
	}
	return nil
}

// generateServingCert builds one self-signed serving keypair for the
// metrics Service's DNS names, in the PEM forms the kubernetes.io/tls
// secret carries. No CA, no files on disk: the operator requires the
// secret's existence, and a real self-signed certificate serves any
// version that reads it.
func generateServingCert(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generating the serving-cert key: %w", err)
	}
	dnsNames := servingCertDNSNames()
	// notBefore reaches an hour back to tolerate clock skew between
	// the host that generates and the workload that reads.
	template := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: dnsNames[2]},
		DNSNames:              dnsNames,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(servingCertValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true, // self-signed: the certificate is its own issuer
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating the serving certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding the serving key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		nil
}

// randomSerial returns a fresh certificate serial number. x509
// rejects a nil serial, and sequential serials are a fingerprinting
// gift, so the number is random.
func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 120)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		// crypto/rand never fails on the platforms kind-ocp runs on;
		// the clock keeps distinct-enough serials flowing instead.
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
