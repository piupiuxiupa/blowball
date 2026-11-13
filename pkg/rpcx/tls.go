package rpcx

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// Service identities are the CommonNames of the client certificates issued to
// each blowball process. Deployment tooling is responsible for issuing the
// certificates; the code only maps an identity to the methods it may call.
const (
	IdentityScheduler = "blowball-scheduler"
	IdentityAPI       = "blowball-api"
	IdentityAgent     = "blowball-agent"
	IdentityMain      = "blowball-main" // combined all role (API + Agent)
)

// TLSFiles is the on-disk material shared by both ends. All paths are required
// for an enabled listener/client. ServerName is only used by clients; empty
// means "use the dial target host".
type TLSFiles struct {
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	CAFile     string `yaml:"ca_file"`
	ServerName string `yaml:"server_name"`
}

func (f TLSFiles) validate() error {
	if f.CertFile == "" || f.KeyFile == "" || f.CAFile == "" {
		return fmt.Errorf("rpcx: cert_file, key_file and ca_file are required for mTLS")
	}
	return nil
}

// ServerTLS builds the server-side mutual-TLS configuration: the server
// presents its certificate and requires a verified client certificate from the
// configured CA.
func ServerTLS(files TLSFiles) (*tls.Config, error) {
	if err := files.validate(); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("rpcx: load server key pair: %w", err)
	}
	pool, err := loadCAPool(files.CAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientTLS builds the client-side mutual-TLS configuration. ServerName, when
// set, pins the expected server identity in addition to CA verification.
func ClientTLS(files TLSFiles) (*tls.Config, error) {
	if err := files.validate(); err != nil {
		return nil, err
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("rpcx: load client key pair: %w", err)
	}
	pool, err := loadCAPool(files.CAFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   files.ServerName,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

func loadCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rpcx: read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("rpcx: CA file %q contains no certificates", path)
	}
	return pool, nil
}

// CertificateIdentity returns the service identity carried by a verified
// client certificate. The deployment convention is CommonName = identity.
func CertificateIdentity(cert *x509.Certificate) string {
	if cert == nil {
		return ""
	}
	return cert.Subject.CommonName
}
