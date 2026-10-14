// Package proxy serves a local HTTP(S) proxy whose egress is the lane pool.
// It exists because the free tier now leases a session to the running opencode
// client only: no third party can mint one, so the client itself has to make
// the calls, and the lanes have to sit under it instead of behind a gateway.
package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CA mints the throwaway certificate authority that opencode is told to trust
// (NODE_EXTRA_CA_CERTS). It never leaves the data dir.
type CA struct {
	path string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	raw  tls.Certificate

	mu    sync.Mutex
	cache map[string]*tls.Certificate
}

// OpenCA loads the CA from dir, creating it on first use. The PEM file it
// returns is what NODE_EXTRA_CA_CERTS must point at.
func OpenCA(dir string) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pemPath := filepath.Join(dir, "ca.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")
	c := &CA{path: pemPath, cache: map[string]*tls.Certificate{}}

	certPEM, cerr := os.ReadFile(pemPath)
	keyPEM, kerr := os.ReadFile(keyPath)
	if cerr == nil && kerr == nil {
		cert, key, err := parseCA(certPEM, keyPEM)
		if err == nil {
			c.cert, c.key = cert, key
			c.raw = tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}
			return c, nil
		}
	}
	cert, key, err := newCA()
	if err != nil {
		return nil, err
	}
	c.cert, c.key = cert, key
	c.raw = tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o644); err != nil {
		return nil, err
	}
	return c, nil
}

// Path is the PEM file to hand to the client as NODE_EXTRA_CA_CERTS.
func (c *CA) Path() string { return c.path }

func newCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "lanvello local CA", Organization: []string{"lanvello"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, fmt.Errorf("ca: bad pem")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func randSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// Cert returns a certificate for host, minted on demand and cached for the
// process lifetime: one CA, one leaf per host the client is talking to.
func (c *CA) Cert(host string) (tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if got, ok := c.cache[host]; ok {
		return *got, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := randSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(2, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	out := tls.Certificate{Certificate: [][]byte{der, c.cert.Raw}, PrivateKey: key}
	c.cache[host] = &out
	return out, nil
}
