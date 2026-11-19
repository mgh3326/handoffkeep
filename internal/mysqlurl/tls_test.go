package mysqlurl

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

const testServerName = "hk-mysql.test"

// testCert is a generated certificate with its key, kept as PEM for the CA
// environment variable and as parsed values for signing and serving.
type testCert struct {
	pemText string
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// issue signs tmpl with parent's key, or with its own key when parent is nil.
func issue(t *testing.T, tmpl *x509.Certificate, parent *testCert) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore = time.Now().Add(-time.Hour)
	tmpl.NotAfter = time.Now().Add(time.Hour)
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCert{
		pemText: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		cert:    cert,
		key:     key,
	}
}

func newTestCA(t *testing.T, cn string) testCert {
	t.Helper()
	return issue(t, &x509.Certificate{
		Subject:               pkix.Name{CommonName: cn},
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}, nil)
}

// newLeaf issues a server certificate with no SAN unless dnsNames is given,
// matching the endpoint certificate this package pins.
func newLeaf(t *testing.T, ca testCert, cn string, dnsNames ...string) testCert {
	t.Helper()
	return issue(t, &x509.Certificate{
		Subject:     pkix.Name{CommonName: cn},
		DNSNames:    dnsNames,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		KeyUsage:    x509.KeyUsageDigitalSignature,
	}, &ca)
}

// pinnedEnv sets the CA variables for one test and returns the config Parse
// produces for it after a round trip through the DSN the store opens with.
func pinnedEnv(t *testing.T, caPEM, serverName string) *tls.Config {
	t.Helper()
	t.Setenv(caPEMEnv, caPEM)
	t.Setenv(serverNameEnv, serverName)
	cfg, err := Parse("mysql://u:p@127.0.0.1:3306/db")
	if err != nil {
		t.Fatal(err)
	}
	dsn, err := mysql.ParseDSN(cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	if dsn.TLS == nil {
		t.Fatal("driver resolved no TLS config for the pinned connection")
	}
	return dsn.TLS
}

// handshake serves leaf on a local TLS listener and dials it with cfg; it
// returns the client's handshake result.
func handshake(t *testing.T, leaf testCert, cfg *tls.Config) error {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{leaf.cert.Raw}, PrivateKey: leaf.key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.(*tls.Conn).Handshake()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().(*net.TCPAddr).String(), cfg)
	if err != nil {
		return err
	}
	return conn.Close()
}

func TestPinnedTLSAcceptsChainAndCommonName(t *testing.T) {
	ca := newTestCA(t, "hk-test-ca")
	cfg := pinnedEnv(t, ca.pemText, testServerName)
	if cfg.ServerName != testServerName || cfg.MinVersion != tls.VersionTLS12 || cfg.RootCAs == nil {
		t.Fatalf("pinned config = ServerName %q MinVersion %x RootCAs nil=%v", cfg.ServerName, cfg.MinVersion, cfg.RootCAs == nil)
	}
	if err := handshake(t, newLeaf(t, ca, testServerName), cfg); err != nil {
		t.Fatalf("handshake with a chain to the pinned CA and matching CN: %v", err)
	}
}

func TestPinnedTLSAcceptsDNSSANWhenCNDiffers(t *testing.T) {
	ca := newTestCA(t, "hk-test-ca")
	cfg := pinnedEnv(t, ca.pemText, testServerName)
	if err := handshake(t, newLeaf(t, ca, "other-name", testServerName), cfg); err != nil {
		t.Fatalf("handshake with a matching DNS SAN: %v", err)
	}
}

func TestPinnedTLSRejectsOtherCA(t *testing.T) {
	pinned := newTestCA(t, "hk-test-ca")
	other := newTestCA(t, "hk-other-ca")
	cfg := pinnedEnv(t, pinned.pemText, testServerName)
	if err := handshake(t, newLeaf(t, other, testServerName), cfg); err == nil {
		t.Fatal("handshake accepted a certificate not signed by the pinned CA")
	}
}

func TestPinnedTLSRejectsWrongName(t *testing.T) {
	ca := newTestCA(t, "hk-test-ca")
	cfg := pinnedEnv(t, ca.pemText, testServerName)
	if err := handshake(t, newLeaf(t, ca, "someone-else"), cfg); err == nil {
		t.Fatal("handshake accepted a certificate whose CN and SANs do not match the server name")
	}
}

func TestPinnedTLSNameIsExactNotPrefix(t *testing.T) {
	ca := newTestCA(t, "hk-test-ca")
	cfg := pinnedEnv(t, ca.pemText, testServerName)
	if err := handshake(t, newLeaf(t, ca, "x."+testServerName), cfg); err == nil {
		t.Fatal("handshake accepted a name that only contains the server name")
	}
}

func TestParseRequiresServerNameWithCA(t *testing.T) {
	ca := newTestCA(t, "hk-test-ca")
	t.Setenv(caPEMEnv, ca.pemText)
	t.Setenv(serverNameEnv, "  ")
	if _, err := Parse("mysql://u:p@127.0.0.1:3306/db"); err == nil {
		t.Fatal("Parse accepted a CA without a server name")
	}
}

func TestParseRejectsCAWithoutCertificate(t *testing.T) {
	t.Setenv(serverNameEnv, testServerName)
	for _, value := range []string{"not a certificate", "", "  \n"} {
		t.Setenv(caPEMEnv, value)
		if _, err := Parse("mysql://u:p@127.0.0.1:3306/db"); err == nil {
			t.Fatalf("Parse accepted CA value %q holding no certificate", value)
		}
	}
}

// An empty CA variable set in the environment must not drop the pin: the URL's
// tls=skip-verify is refused rather than used.
func TestParseEmptyCAValueDoesNotFallBackToURLTLS(t *testing.T) {
	t.Setenv(caPEMEnv, "")
	t.Setenv(serverNameEnv, testServerName)
	if _, err := Parse("mysql://u:p@127.0.0.1:3306/db?tls=skip-verify"); err == nil {
		t.Fatal("Parse used URL tls=skip-verify while HANDOFFKEEP_MYSQL_CA_PEM is set but empty")
	}
}

// unsetCA removes the CA variable for one test; t.Setenv restores it afterwards.
func unsetCA(t *testing.T) {
	t.Helper()
	t.Setenv(caPEMEnv, "")
	if err := os.Unsetenv(caPEMEnv); err != nil {
		t.Fatal(err)
	}
}

func TestParseTLSParameterWithCA(t *testing.T) {
	ca := newTestCA(t, "hk-test-ca")
	t.Setenv(caPEMEnv, ca.pemText)
	t.Setenv(serverNameEnv, testServerName)
	for _, tc := range []struct {
		name, raw string
		wantErr   bool
		wantTLS   string
	}{
		{"omitted", "mysql://u:p@127.0.0.1:3306/db", false, pinnedTLSName},
		{"empty", "mysql://u:p@127.0.0.1:3306/db?tls=", false, pinnedTLSName},
		{"same name", "mysql://u:p@127.0.0.1:3306/db?tls=hk-pinned", false, pinnedTLSName},
		{"tls=true", "mysql://u:p@127.0.0.1:3306/db?tls=true", true, ""},
		{"skip-verify", "mysql://u:p@127.0.0.1:3306/db?tls=skip-verify", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) accepted a tls= that disagrees with the pinned CA", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.TLSConfig != tc.wantTLS {
				t.Fatalf("TLSConfig = %q, want %q", cfg.TLSConfig, tc.wantTLS)
			}
		})
	}
}

func TestParseTLSParameterWithoutCAIsUnchanged(t *testing.T) {
	unsetCA(t)
	t.Setenv(serverNameEnv, "")
	cfg, err := Parse("mysql://u:p@127.0.0.1:3306/db?tls=skip-verify")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLSConfig != "skip-verify" {
		t.Fatalf("TLSConfig = %q, want the URL value unchanged", cfg.TLSConfig)
	}
}
