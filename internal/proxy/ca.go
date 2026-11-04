package proxy

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"time"
)

const certificateCacheLimit = 256

type certificateAuthority struct {
	cert        *x509.Certificate
	key         crypto.Signer
	fingerprint string
	mu          sync.Mutex
	cache       map[string]*tls.Certificate
	now         func() time.Time
}

func serialNumber() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

// GenerateCA never overwrites an existing file. Operators explicitly distribute
// only certFile; the signing key must remain outside the agent's filesystem.
func GenerateCA(certFile, keyFile string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := serialNumber()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "GitHub Agent Proxy Interception CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	// Reserve both names before writing either file.
	f, err := os.OpenFile(keyFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	c, err := os.OpenFile(certFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		_ = os.Remove(keyFile)
		return err
	}
	defer c.Close()
	if err := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}); err != nil {
		return err
	}
	return pem.Encode(c, &pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func loadCA(certFile, keyFile string) (*certificateAuthority, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, fmt.Errorf("interception CA must be a currently valid signing CA")
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("unsupported CA key")
	}
	fingerprint := sha256.Sum256(cert.Raw)
	return &certificateAuthority{cert: cert, key: key, fingerprint: hex.EncodeToString(fingerprint[:]), cache: map[string]*tls.Certificate{}, now: time.Now}, nil
}

func (ca *certificateAuthority) certificate(host string) (*tls.Certificate, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	now := ca.now()
	if now.Before(ca.cert.NotBefore) || !now.Before(ca.cert.NotAfter) {
		return nil, fmt.Errorf("interception CA expired or not yet valid")
	}
	if cert := ca.cache[host]; cert != nil && now.Before(cert.Leaf.NotAfter.Add(-time.Minute)) {
		return cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	expires := now.Add(24 * time.Hour)
	if expires.After(ca.cert.NotAfter) {
		expires = ca.cert.NotAfter
	}
	tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, NotBefore: now.Add(-5 * time.Minute), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	cert := &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key, Leaf: leaf}
	if len(ca.cache) >= certificateCacheLimit { // Bounded memory, including hostile random hostnames.
		for name := range ca.cache {
			delete(ca.cache, name)
			break
		}
	}
	ca.cache[host] = cert
	return cert, nil
}
