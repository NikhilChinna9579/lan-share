// Package identity creates and loads this device's key pair and self-signed certificate
// (PROTOCOL.md §1). The fingerprint of the public key is the device's ID.
package identity

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
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Identity is this device's TLS certificate plus its fingerprint.
type Identity struct {
	Cert        tls.Certificate
	Fingerprint string
}

// LoadOrCreate reads identity.pem from dir, creating a new identity on first run.
func LoadOrCreate(dir, name string) (*Identity, error) {
	path := filepath.Join(dir, "identity.pem")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		data, err = create(name)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		// 0600: only the current user can read the private key.
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return nil, fmt.Errorf("identity: %s is damaged: %w", path, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	return &Identity{Cert: cert, Fingerprint: Fingerprint(leaf)}, nil
}

// Fingerprint is lowercase hex SHA-256 of the certificate's SubjectPublicKeyInfo.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func create(name string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	out = append(out, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
	return out, nil
}
