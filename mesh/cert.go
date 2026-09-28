package mesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const gatewayCertValidity = 10 * 365 * 24 * time.Hour

// CAHash is the fingerprint a join token carries, so a joining machine can
// tell the hub's self-signed CA from one an attacker in the path presents.
func CAHash(caCert *x509.Certificate) string {
	sum := sha256.Sum256(caCert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// certRawHash is the form DERP pins a relay's certificate by.
func certRawHash(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// ensureGatewayCert returns the hub's serving certificate, reusing the one on
// disk while it still names host. The DERP map pins this exact certificate, so
// replacing it needlessly would cut every node off from the relay until it
// fetches the new map.
func ensureGatewayCert(dir, host string, caCert *x509.Certificate, caKey *rsa.PrivateKey) (tls.Certificate, *x509.Certificate, error) {
	certFile := filepath.Join(dir, "gateway.crt")
	keyFile := filepath.Join(dir, "gateway.key")
	if pair, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err == nil && leaf.CheckSignatureFrom(caCert) == nil && leaf.VerifyHostname(host) == nil &&
			time.Until(leaf.NotAfter) > 30*24*time.Hour {
			pair.Leaf = leaf
			return pair, leaf, nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(gatewayCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return tls.Certificate{}, nil, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	// Only the leaf is served: a DERP client that pins a certificate refuses
	// a handshake that presents more than one.
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, leaf, nil
}
