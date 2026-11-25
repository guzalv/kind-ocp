package sccadmission

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestGenerateTLS(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	material, err := generateTLS(now)
	if err != nil {
		t.Fatalf("generateTLS: %v", err)
	}

	caCert, err := parseCertificate(material.caCertPEM)
	if err != nil {
		t.Fatalf("the generated CA: %v", err)
	}
	tlsCert, err := parseCertificate(material.tlsCertPEM)
	if err != nil {
		t.Fatalf("the generated serving certificate: %v", err)
	}
	tlsKey, err := parseECPrivateKey(material.tlsKeyPEM)
	if err != nil {
		t.Fatalf("the generated serving key: %v", err)
	}

	t.Run("the CA is a CA", func(t *testing.T) {
		if !caCert.IsCA {
			t.Error("the CA certificate does not assert IsCA")
		}
		if !caCert.NotBefore.Equal(now.Add(-time.Hour)) || !caCert.NotAfter.Equal(now.Add(certValidity)) {
			t.Errorf("the CA validity runs %s to %s, want one hour of skew tolerance before %s and %s after",
				caCert.NotBefore, caCert.NotAfter, now, certValidity)
		}
	})

	t.Run("the serving certificate covers the service DNS names as SANs", func(t *testing.T) {
		have := map[string]bool{}
		for _, name := range tlsCert.DNSNames {
			have[name] = true
		}
		for _, name := range serviceDNSNames() {
			if !have[name] {
				t.Errorf("the serving certificate carries no SAN for %q (has %v)", name, tlsCert.DNSNames)
			}
		}
		if !strings.Contains(tlsCert.Subject.CommonName, webhookService) {
			t.Errorf("the serving certificate CN = %q, want it derived from the service name", tlsCert.Subject.CommonName)
		}
		if err := tlsCert.VerifyHostname(serviceDNSNames()[1]); err != nil {
			t.Errorf("the serving certificate does not verify its own service name: %v", err)
		}
	})

	t.Run("the serving certificate chains to the CA and matches the key", func(t *testing.T) {
		if err := tlsCert.CheckSignatureFrom(caCert); err != nil {
			t.Errorf("the serving certificate is not signed by the generated CA: %v", err)
		}
		if !tlsKey.PublicKey.Equal(tlsCert.PublicKey) {
			t.Error("the serving key does not match the serving certificate")
		}
		if _, err := tlsCert.Verify(x509.VerifyOptions{
			Roots:         x509.NewCertPool(),
			Intermediates: x509.NewCertPool(),
		}); err == nil {
			// Without the CA in the roots the verification must fail;
			// passing would mean the certificate chains to something
			// else entirely.
			t.Error("the serving certificate verifies with no roots at all")
		}
		roots := x509.NewCertPool()
		roots.AddCert(caCert)
		if _, err := tlsCert.Verify(x509.VerifyOptions{Roots: roots, DNSName: serviceDNSNames()[1]}); err != nil {
			t.Errorf("the serving certificate does not verify against its CA for the service name: %v", err)
		}
	})

	t.Run("each generation is its own keypair", func(t *testing.T) {
		other, err := generateTLS(now)
		if err != nil {
			t.Fatal(err)
		}
		if string(other.tlsCertPEM) == string(material.tlsCertPEM) {
			t.Error("two generations produced the same certificate")
		}
		otherCert, err := parseCertificate(other.tlsCertPEM)
		if err != nil {
			t.Fatal(err)
		}
		if err := otherCert.CheckSignatureFrom(caCert); err == nil {
			t.Error("one generation's certificate verifies against another's CA")
		}
	})
}

func TestValidateSecretData(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	material, err := generateTLS(now)
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string][]byte{
		secretCAKey: material.caCertPEM,
		"tls.crt":   material.tlsCertPEM,
		"tls.key":   material.tlsKeyPEM,
	}

	t.Run("accepts freshly generated material", func(t *testing.T) {
		if err := validateSecretData(valid, now); err != nil {
			t.Fatalf("validateSecretData(generated) = %v, want nil", err)
		}
	})

	t.Run("rejects missing keys", func(t *testing.T) {
		for _, key := range []string{secretCAKey, "tls.crt", "tls.key"} {
			incomplete := map[string][]byte{}
			for k, v := range valid {
				incomplete[k] = v
			}
			delete(incomplete, key)
			if err := validateSecretData(incomplete, now); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("validateSecretData(without %s) = %v, want the missing key named", key, err)
			}
		}
	})

	t.Run("rejects material near or past expiry", func(t *testing.T) {
		old, err := generateTLS(now.Add(-certValidity))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSecretData(map[string][]byte{
			secretCAKey: old.caCertPEM,
			"tls.crt":   old.tlsCertPEM,
			"tls.key":   old.tlsKeyPEM,
		}, now); err == nil || !strings.Contains(err.Error(), "renewal margin") {
			t.Fatalf("validateSecretData(expired) = %v, want the renewal margin named", err)
		}
		// Forty days before expiry the certificate is still accepted;
		// twenty days before it is not — that is the margin.
		withFortyLeft := now.Add(certValidity).Add(-40 * 24 * time.Hour)
		if err := validateSecretData(valid, withFortyLeft); err != nil {
			t.Fatalf("validateSecretData(forty days left) = %v, want nil outside the margin", err)
		}
		withTwentyLeft := now.Add(certValidity).Add(-20 * 24 * time.Hour)
		if err := validateSecretData(valid, withTwentyLeft); err == nil || !strings.Contains(err.Error(), "renewal margin") {
			t.Fatalf("validateSecretData(twenty days left) = %v, want the margin named", err)
		}
	})

	t.Run("rejects a key that does not match the certificate", func(t *testing.T) {
		foreignKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalECPrivateKey(foreignKey)
		if err != nil {
			t.Fatal(err)
		}
		mismatched := map[string][]byte{}
		for k, v := range valid {
			mismatched[k] = v
		}
		mismatched["tls.key"] = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		if err := validateSecretData(mismatched, now); err == nil || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("validateSecretData(foreign key) = %v, want the mismatch named", err)
		}
	})

	t.Run("rejects a certificate from a foreign CA", func(t *testing.T) {
		foreign, err := generateTLS(now)
		if err != nil {
			t.Fatal(err)
		}
		mixed := map[string][]byte{
			secretCAKey: material.caCertPEM,
			"tls.crt":   foreign.tlsCertPEM,
			"tls.key":   foreign.tlsKeyPEM,
		}
		if err := validateSecretData(mixed, now); err == nil || !strings.Contains(err.Error(), "does not verify against the secret's CA") {
			t.Fatalf("validateSecretData(foreign chain) = %v, want the chain break named", err)
		}
	})

	t.Run("rejects a certificate that is not yet valid", func(t *testing.T) {
		// A host clock behind the generation time: both certificates
		// are in the future, and Go's verification rejects the window —
		// exactly what the apiserver's TLS client would do.
		if err := validateSecretData(valid, now.Add(-2*time.Hour)); err == nil || !strings.Contains(err.Error(), "does not verify against the secret's CA") {
			t.Fatalf("validateSecretData(not yet valid) = %v, want the validity window named", err)
		}
	})

	t.Run("rejects a certificate without the server-auth EKU", func(t *testing.T) {
		caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		caTemplate := &x509.Certificate{
			SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "test CA"},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(certValidity),
			KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
		}
		caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		caCert, err := x509.ParseCertificate(caDER)
		if err != nil {
			t.Fatal(err)
		}
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{
			SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "client"},
			DNSNames:  serviceDNSNames(),
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(certValidity),
			KeyUsage: x509.KeyUsageDigitalSignature,
			// Client authentication only: a fine certificate that is
			// not a server certificate.
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}
		leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSecretData(map[string][]byte{
			secretCAKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
			"tls.crt":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
			"tls.key":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
		}, now); err == nil || !strings.Contains(err.Error(), "does not verify against the secret's CA") {
			t.Fatalf("validateSecretData(client-auth only) = %v, want the key usage named", err)
		}
	})

	t.Run("rejects certificates missing a service SAN", func(t *testing.T) {
		caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		caTemplate := &x509.Certificate{
			SerialNumber: randomSerial(),
			Subject:      pkix.Name{CommonName: "test CA"},
			NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(certValidity),
			KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
		}
		caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		caCert, err := x509.ParseCertificate(caDER)
		if err != nil {
			t.Fatal(err)
		}
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{
			SerialNumber: randomSerial(),
			Subject:      pkix.Name{CommonName: "short"},
			DNSNames:     serviceDNSNames()[:2], // missing the .svc forms
			NotBefore:    now.Add(-time.Hour), NotAfter: now.Add(certValidity),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateSecretData(map[string][]byte{
			secretCAKey: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
			"tls.crt":   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
			"tls.key":   pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
		}, now); err == nil || !strings.Contains(err.Error(), "does not verify against the secret's CA") {
			t.Fatalf("validateSecretData(short SANs) = %v, want the missing SAN named", err)
		}
	})

	t.Run("rejects trailing garbage after a PEM block", func(t *testing.T) {
		doubled := map[string][]byte{}
		for k, v := range valid {
			doubled[k] = v
		}
		doubled["tls.crt"] = append(append([]byte{}, valid["tls.crt"]...), valid["tls.crt"]...)
		if err := validateSecretData(doubled, now); err == nil || !strings.Contains(err.Error(), "trailing data") {
			t.Fatalf("validateSecretData(two certificates) = %v, want the trailing data named", err)
		}
	})
}
