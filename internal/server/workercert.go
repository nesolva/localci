package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Files of the worker listener's certificate, in the data directory, private to the user.
const (
	WorkerCertFile = "worker-cert.pem"
	WorkerKeyFile  = "worker-key.pem"
)

// LoadWorkerCert returns the worker listener's self-signed certificate and its SHA-256 fingerprint in hex. Workers
// pin that fingerprint; no name is checked. A pair is generated (key mode 0600, and logged with its fingerprint
// through logf when it is not nil) only when neither file exists. A pair that is present but unreadable, half there
// or mismatched is an error naming the files: replacing it silently would break every pinned worker.
func LoadWorkerCert(dataDir string, logf func(format string, args ...any)) (tls.Certificate, string, error) {
	certPath, keyPath := filepath.Join(dataDir, WorkerCertFile), filepath.Join(dataDir, WorkerKeyFile)
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	if certErr == nil || keyErr == nil || !errors.Is(certErr, fs.ErrNotExist) || !errors.Is(keyErr, fs.ErrNotExist) {
		c, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err == nil && len(c.Certificate) == 0 {
			err = errors.New("no certificate in the file")
		}
		if err != nil {
			return tls.Certificate{}, "", fmt.Errorf("worker certificate %s and key %s cannot be used: %w "+
				"(fix or remove both files to generate a new pair; workers pinned to the old one must be given the new fingerprint)",
				certPath, keyPath, err)
		}
		return c, Fingerprint(c.Certificate[0]), nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return tls.Certificate{}, "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "localci worker listener"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		return tls.Certificate{}, "", err
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	if logf != nil {
		logf("generated a new worker certificate (%s, %s); workers pin its fingerprint %s", certPath, keyPath, Fingerprint(der))
	}
	return c, Fingerprint(der), nil
}

// Fingerprint is the hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string {
	h := sha256.Sum256(der)
	return hex.EncodeToString(h[:])
}
