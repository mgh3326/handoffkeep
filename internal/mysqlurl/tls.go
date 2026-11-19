package mysqlurl

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// The pinned CA is for a MySQL endpoint whose certificate is signed by a
// private CA and carries no SAN, reached by IP. The system roots cannot verify
// it, and tls=skip-verify verifies nothing, so the CA and the expected server
// name come from the environment.
const (
	caPEMEnv      = "HANDOFFKEEP_MYSQL_CA_PEM"
	serverNameEnv = "HANDOFFKEEP_MYSQL_SERVER_NAME"
	// pinnedTLSName is the driver registry key of the pinned config. While the
	// CA is set, a URL tls= value may only be this name or empty.
	pinnedTLSName = "hk-pinned"
)

// pinnedTLS returns the pinned TLS config when HANDOFFKEEP_MYSQL_CA_PEM is set
// and registers it with the driver under pinnedTLSName. It returns nil, nil
// when the CA is unset. Each call registers again; the environment is fixed for
// the life of a process, so every call registers the same config.
func pinnedTLS() (*tls.Config, error) {
	pemText := os.Getenv(caPEMEnv)
	if strings.TrimSpace(pemText) == "" {
		return nil, nil
	}
	serverName := strings.TrimSpace(os.Getenv(serverNameEnv))
	if serverName == "" {
		return nil, fmt.Errorf("%s is required when %s is set", serverNameEnv, caPEMEnv)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(pemText)) {
		return nil, fmt.Errorf("%s holds no PEM certificate", caPEMEnv)
	}
	cfg := &tls.Config{
		RootCAs:    roots,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
		// Go's own check matches names only against SANs and ignores the CN when
		// there are none, so it is off here and verifyPinned does the chain and
		// name checks instead.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyPinned(roots, serverName),
	}
	if err := mysql.RegisterTLSConfig(pinnedTLSName, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// verifyPinned accepts the server only when its leaf chains to roots and the
// leaf names serverName, by CN or by a DNS SAN, compared case-insensitively.
// VerifyOptions carries no DNSName, so the chain check skips the host name and
// the name rule is the one here.
func verifyPinned(roots *x509.CertPool, serverName string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("mysql server sent no certificate")
		}
		leaf, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("parse mysql server certificate: %w", err)
		}
		intermediates := x509.NewCertPool()
		for _, raw := range rawCerts[1:] {
			c, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("parse mysql server intermediate certificate: %w", err)
			}
			intermediates.AddCert(c)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			return fmt.Errorf("mysql server certificate is not signed by %s: %w", caPEMEnv, err)
		}
		if !leafNameIs(leaf, serverName) {
			return fmt.Errorf("mysql server certificate is not for %s", serverNameEnv)
		}
		return nil
	}
}

// leafNameIs reports whether the leaf's CN or one of its DNS SANs is name.
func leafNameIs(leaf *x509.Certificate, name string) bool {
	if strings.EqualFold(leaf.Subject.CommonName, name) {
		return true
	}
	for _, dns := range leaf.DNSNames {
		if strings.EqualFold(dns, name) {
			return true
		}
	}
	return false
}
