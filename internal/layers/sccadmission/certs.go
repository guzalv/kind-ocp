package sccadmission

import (
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
)

// The certificate lifetimes. The CA and the serving certificate are
// valid for ten years and replaced when less than a month remains:
// these clusters are disposable, the regeneration path exists, and a
// long-lived certificate is one less thing a re-run has to churn.
const (
	certValidity  = 10 * 365 * 24 * time.Hour
	renewalMargin = 30 * 24 * time.Hour

	caCommonName = "kind-ocp scc-admission CA"
)

// tlsMaterial is one freshly generated CA and serving keypair, in the
// PEM forms the Secret and the MutatingWebhookConfiguration carry. The
// CA's private key never leaves the host: only the CA certificate is
// bundled into the cluster (as the caBundle the apiserver trusts and
// the ca.crt copy inside the secret, which a later run reads to keep
// the configuration and a still-valid certificate in sync).
type tlsMaterial struct {
	caCertPEM  []byte
	tlsCertPEM []byte
	tlsKeyPEM  []byte
}

// generateTLS builds a fresh CA and a serving certificate for the
// webhook's service DNS names, signed by that CA. This is the PoC's
// openssl pipeline (CA, key, CSR, sign) as plain Go — no host openssl,
// no files on disk, nothing to clean up.
func generateTLS(now time.Time) (tlsMaterial, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsMaterial{}, fmt.Errorf("generating the CA key: %w", err)
	}
	// notBefore reaches an hour back to tolerate clock skew between the
	// host that generates and the apiserver that verifies.
	caTemplate := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: caCommonName},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tlsMaterial{}, fmt.Errorf("creating the CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tlsMaterial{}, fmt.Errorf("parsing the CA certificate: %w", err)
	}

	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tlsMaterial{}, fmt.Errorf("generating the serving key: %w", err)
	}
	dnsNames := serviceDNSNames()
	tlsTemplate := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: dnsNames[1]},
		DNSNames:     dnsNames,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	tlsDER, err := x509.CreateCertificate(rand.Reader, tlsTemplate, caCert, &tlsKey.PublicKey, caKey)
	if err != nil {
		return tlsMaterial{}, fmt.Errorf("creating the serving certificate: %w", err)
	}
	tlsKeyDER, err := x509.MarshalECPrivateKey(tlsKey)
	if err != nil {
		return tlsMaterial{}, fmt.Errorf("encoding the serving key: %w", err)
	}
	return tlsMaterial{
		caCertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tlsCertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsDER}),
		tlsKeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: tlsKeyDER}),
	}, nil
}

// randomSerial returns a fresh certificate serial number. x509 rejects
// a nil serial, and sequential serials are a fingerprinting gift, so
// the serial is random like the keys.
func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(1)
	}
	return serial
}

// validateSecretData checks an existing secret's TLS material against
// everything the webhook needs from it: the CA certificate and the
// serving pair parse, the key matches the certificate, the certificate
// verifies against the secret's CA for the service's DNS name as a
// server certificate at the current time (Go's own chain, validity
// window, SAN, and EKU verification — the same checks the apiserver's
// TLS client makes), and both certificates are outside the renewal
// margin. The first problem found is returned; the caller regenerates.
func validateSecretData(data map[string][]byte, now time.Time) error {
	caCertPEM, ok := data[secretCAKey]
	if !ok || len(caCertPEM) == 0 {
		return errors.New("the secret carries no " + secretCAKey)
	}
	tlsCertPEM, ok := data["tls.crt"]
	if !ok || len(tlsCertPEM) == 0 {
		return errors.New("the secret carries no tls.crt")
	}
	tlsKeyPEM, ok := data["tls.key"]
	if !ok || len(tlsKeyPEM) == 0 {
		return errors.New("the secret carries no tls.key")
	}
	caCert, err := parseCertificate(caCertPEM)
	if err != nil {
		return fmt.Errorf("the CA certificate: %w", err)
	}
	if !caCert.IsCA {
		return errors.New("the CA certificate does not assert IsCA")
	}
	tlsCert, err := parseCertificate(tlsCertPEM)
	if err != nil {
		return fmt.Errorf("the serving certificate: %w", err)
	}
	tlsKey, err := parseECPrivateKey(tlsKeyPEM)
	if err != nil {
		return fmt.Errorf("the serving key: %w", err)
	}
	// The key match is the one thing x509 verification cannot check:
	// a certificate can chain, name, and time out perfectly while the
	// secret pairs it with a different key's PEM.
	keyPub, keyIsEC := tlsKey.Public().(*ecdsa.PublicKey)
	certPub, certIsEC := tlsCert.PublicKey.(*ecdsa.PublicKey)
	if !keyIsEC || !certIsEC || !keyPub.Equal(certPub) {
		return errors.New("the serving key does not match the serving certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	// Every service DNS name must verify, not just one: the apiserver
	// dials the Service under a resolver-dependent form of the name,
	// and the certificate's job is to cover them all. Each check is
	// Go's own chain, validity-window, SAN, and EKU verification — the
	// same checks the apiserver's TLS client makes.
	for _, name := range serviceDNSNames() {
		if _, err := tlsCert.Verify(x509.VerifyOptions{
			DNSName:     name,
			Roots:       roots,
			CurrentTime: now,
			KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			return fmt.Errorf("the serving certificate does not verify against the secret's CA as a server certificate for %s: %w", name, err)
		}
	}
	for _, cert := range []*x509.Certificate{caCert, tlsCert} {
		if now.Add(renewalMargin).After(cert.NotAfter) {
			return fmt.Errorf("the certificate for %q expires at %s, inside the %s renewal margin", cert.Subject.CommonName, cert.NotAfter.Format(time.DateOnly), renewalMargin)
		}
	}
	return nil
}

// parseCertificate decodes exactly one PEM CERTIFICATE block.
func parseCertificate(pemBytes []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE PEM block")
	}
	if err := onlyWhitespace(rest); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(block.Bytes)
}

// parseECPrivateKey decodes exactly one PEM EC PRIVATE KEY block.
func parseECPrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, errors.New("no EC PRIVATE KEY PEM block")
	}
	if err := onlyWhitespace(rest); err != nil {
		return nil, err
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// onlyWhitespace rejects trailing non-whitespace data after a PEM
// block: a second certificate or key in the same field is not
// material this layer wrote, and reading only the first block would
// silently trust half of it.
func onlyWhitespace(rest []byte) error {
	for _, b := range rest {
		switch b {
		case ' ', '\t', '\r', '\n':
		default:
			return errors.New("trailing data after the PEM block")
		}
	}
	return nil
}
