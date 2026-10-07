/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package libvirt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testDialTimeout is short so the bound is visible. testBoundWithin is how late
// a stalled dial may return: tight enough that a deadline drifting off the
// configured timeout fails (go-libvirt's was a fixed 20s), loose enough for a
// loaded CI runner. testCloseWithin is the slack for the other end to notice a
// closed socket.
const (
	testDialTimeout = 200 * time.Millisecond
	testBoundWithin = 5 * testDialTimeout
	testCloseWithin = 2 * time.Second
)

// requireBoundedByTimeout fails unless a stalled dial returned at its deadline:
// not before it, which would mean something other than the deadline ended it,
// and not long after it.
func requireBoundedByTimeout(t *testing.T, elapsed time.Duration) {
	t.Helper()
	if elapsed < testDialTimeout {
		t.Errorf("Dial returned after %s, before its %s deadline -- the deadline did not end it", elapsed, testDialTimeout)
	}
	if elapsed > testBoundWithin {
		t.Errorf("Dial took %s, want it to return at its %s deadline (allowed up to %s)", elapsed, testDialTimeout, testBoundWithin)
	}
}

// testPKI is a throwaway CA with a server certificate for 127.0.0.1 and a client
// certificate, laid out the way the libvirt-pki Secret is mounted.
type testPKI struct {
	clientDir  string
	serverConf *tls.Config
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	caKey := newTestKey(t)
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hydra test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA: %v", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing CA: %v", err)
	}

	issue := func(serial int64, cn string, usage x509.ExtKeyUsage, ips []net.IP) ([]byte, *ecdsa.PrivateKey) {
		key := newTestKey(t)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{usage},
			IPAddresses:  ips,
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("issuing %s: %v", cn, err)
		}
		return der, key
	}
	serverDER, serverKey := issue(2, "libvirtd", x509.ExtKeyUsageServerAuth, []net.IP{net.IPv4(127, 0, 0, 1)})
	clientDER, clientKey := issue(3, "hydra-provider", x509.ExtKeyUsageClientAuth, nil)

	dir := t.TempDir()
	writePEM(t, filepath.Join(dir, "cacert.pem"), "CERTIFICATE", caDER)
	writePEM(t, filepath.Join(dir, "clientcert.pem"), "CERTIFICATE", clientDER)
	writePEM(t, filepath.Join(dir, "clientkey.pem"), "PRIVATE KEY", marshalKey(t, clientKey))

	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(ca)
	return testPKI{
		clientDir: dir,
		serverConf: &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{serverDER}, PrivateKey: serverKey}},
			ClientCAs:    clientCAs,
			ClientAuth:   tls.RequireAndVerifyClientCert,
			MinVersion:   tls.VersionTLS12,
		},
	}
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return key
}

func marshalKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}
	return der
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// serve accepts connections on ln and hands each to handle. It returns the
// host and port to dial.
func serve(t *testing.T, ln net.Listener, handle func(net.Conn)) (string, string) {
	t.Helper()
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handle(c)
		}
	}()
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting listener address: %v", err)
	}
	return host, port
}

// serveTLS is serve for a libvirtd stand-in that completes the TLS handshake,
// including verifying the client certificate, before handing the connection on.
func serveTLS(t *testing.T, pki testPKI, handle func(net.Conn)) (string, string) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", pki.serverConf)
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	return serve(t, ln, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		if err := c.(*tls.Conn).Handshake(); err != nil {
			return
		}
		handle(c)
	})
}

// waitForPeerClose reads until the other end closes and reports it on the
// returned channel. Reading is also what lets a TLS 1.3 client finish sending
// its handshake.
func waitForPeerClose(c net.Conn, closed chan<- struct{}) {
	_, _ = io.Copy(io.Discard, c)
	close(closed)
}

func requireClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(testCloseWithin):
		t.Fatal("the dialer never closed the connection: a stalled peer would leak a socket per retry")
	}
}

// The PET-36 case: a peer that completes TLS and never sends libvirtd's
// verification byte must neither hang Dial nor keep the socket.
func TestTLSDialerBoundsStalledVerificationRead(t *testing.T) {
	pki := newTestPKI(t)
	closed := make(chan struct{})
	host, port := serveTLS(t, pki, func(c net.Conn) { waitForPeerClose(c, closed) })

	start := time.Now()
	_, err := newTLSDialer(host, port, pki.clientDir, testDialTimeout).Dial()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Dial succeeded against a peer that never sent the verification byte")
	}
	if !strings.Contains(err.Error(), "verify our client certificate") {
		t.Errorf("Dial error = %q, want it to name the verification step", err)
	}
	requireBoundedByTimeout(t, elapsed)
	requireClosed(t, closed)
}

// The handshake is under the same deadline: a peer that accepts TCP and never
// answers the ClientHello is cut off too.
func TestTLSDialerBoundsStalledHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	closed := make(chan struct{})
	host, port := serve(t, ln, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		waitForPeerClose(c, closed)
	})

	start := time.Now()
	_, err = newTLSDialer(host, port, newTestPKI(t).clientDir, testDialTimeout).Dial()
	if err == nil || !strings.Contains(err.Error(), "TLS handshake") {
		t.Fatalf("Dial error = %v, want a TLS handshake failure", err)
	}
	requireBoundedByTimeout(t, time.Since(start))
	requireClosed(t, closed)
}

// Nothing listening is the commonest real failure. It must come back at once,
// wrapped, with the cause still matchable -- not after waiting out the timeout.
func TestTLSDialerReportsARefusedConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting listener address: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	start := time.Now()
	_, err = newTLSDialer(host, port, newTestPKI(t).clientDir, testDialTimeout).Dial()
	elapsed := time.Since(start)

	if err == nil || !strings.Contains(err.Error(), "connecting to") {
		t.Fatalf("Dial error = %v, want a connect failure", err)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("Dial error = %v, want it to wrap ECONNREFUSED", err)
	}
	if elapsed >= testDialTimeout {
		t.Errorf("Dial took %s against a closed port, want it to fail at once rather than wait out the %s timeout", elapsed, testDialTimeout)
	}
}

func TestTLSDialerConnectsAndClearsTheDeadline(t *testing.T) {
	pki := newTestPKI(t)
	host, port := serveTLS(t, pki, func(c net.Conn) {
		if _, err := c.Write([]byte{tlsVerificationOK}); err != nil {
			return
		}
		// Reply only after the dial deadline has passed. A deadline left on the
		// connection would fail the client's read below.
		time.Sleep(3 * testDialTimeout)
		_, _ = c.Write([]byte("ok"))
		_, _ = io.Copy(io.Discard, c)
	})

	conn, err := newTLSDialer(host, port, pki.clientDir, testDialTimeout).Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	got := make([]byte, 2)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("reading after the dial deadline: %v -- the deadline was not cleared", err)
	}
	if string(got) != "ok" {
		t.Errorf("read %q, want %q", got, "ok")
	}
}

func TestTLSDialerRejectsAFailedVerification(t *testing.T) {
	pki := newTestPKI(t)
	closed := make(chan struct{})
	host, port := serveTLS(t, pki, func(c net.Conn) {
		_, _ = c.Write([]byte{0})
		waitForPeerClose(c, closed)
	})

	_, err := newTLSDialer(host, port, pki.clientDir, testDialTimeout).Dial()
	if err == nil || !strings.Contains(err.Error(), "rejected our client certificate") {
		t.Fatalf("Dial error = %v, want a rejection", err)
	}
	requireClosed(t, closed)
}

func TestTLSDialerRejectsAServerFromAnotherCA(t *testing.T) {
	server := newTestPKI(t)
	host, port := serveTLS(t, server, func(c net.Conn) { _, _ = io.Copy(io.Discard, c) })

	// A complete client PKI, but issued by a different CA from the server's.
	_, err := newTLSDialer(host, port, newTestPKI(t).clientDir, testDialTimeout).Dial()
	if err == nil || !strings.Contains(err.Error(), "TLS handshake") {
		t.Fatalf("Dial error = %v, want the handshake to reject the server certificate", err)
	}
}

func TestTLSDialerReportsMissingOrBrokenPKI(t *testing.T) {
	full := newTestPKI(t).clientDir
	copyFile := func(t *testing.T, dir, name string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(full, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, dir string)
		wantErr string
	}{
		{
			name:    "no client certificate",
			setup:   func(*testing.T, string) {},
			wantErr: "clientcert.pem",
		},
		{
			// What a libvirt-pki Secret without a clientkey.pem key produces.
			name: "client certificate but no key",
			setup: func(t *testing.T, dir string) {
				copyFile(t, dir, "clientcert.pem")
				copyFile(t, dir, "cacert.pem")
			},
			wantErr: "clientkey.pem",
		},
		{
			name: "no CA certificate",
			setup: func(t *testing.T, dir string) {
				copyFile(t, dir, "clientcert.pem")
				copyFile(t, dir, "clientkey.pem")
			},
			wantErr: "cacert.pem",
		},
		{
			name: "CA file holds no certificate",
			setup: func(t *testing.T, dir string) {
				copyFile(t, dir, "clientcert.pem")
				copyFile(t, dir, "clientkey.pem")
				if err := os.WriteFile(filepath.Join(dir, "cacert.pem"), []byte("not a certificate\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "contains no PEM certificate",
		},
		{
			name: "key does not match the certificate",
			setup: func(t *testing.T, dir string) {
				copyFile(t, dir, "clientcert.pem")
				copyFile(t, dir, "cacert.pem")
				writePEM(t, filepath.Join(dir, "clientkey.pem"), "PRIVATE KEY", marshalKey(t, newTestKey(t)))
			},
			wantErr: "invalid TLS client certificate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			// Nothing listens here: PKI problems must be reported before any dial.
			_, err := newTLSDialer("127.0.0.1", "1", dir, testDialTimeout).Dial()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Dial error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewTLSDialerSearchPaths(t *testing.T) {
	d := newTLSDialer(testHost, "", testPKIPath, time.Second)
	if d.port != defaultTLSPort {
		t.Errorf("port = %q, want libvirtd's default %q", d.port, defaultTLSPort)
	}
	if len(d.certDirs) != 1 || d.certDirs[0] != (tlsCertDir{certPath: testPKIPath, keyPath: testPKIPath}) {
		t.Errorf("certDirs = %v, want only %s for both certificate and key", d.certDirs, testPKIPath)
	}
	if len(d.caDirs) != 1 || d.caDirs[0] != testPKIPath {
		t.Errorf("caDirs = %v, want only %s", d.caDirs, testPKIPath)
	}
}

// Without a PKI path the user lookup decides the search order. Stubbed, so the
// result does not depend on who runs the tests -- a root CI container would
// otherwise never take the home-directory branch.
func TestNewTLSDialerHomeDirectoryLookup(t *testing.T) {
	system := []tlsCertDir{{certPath: "/etc/pki/libvirt/", keyPath: "/etc/pki/libvirt/private/"}}
	systemCA := []string{"/etc/pki/CA/"}
	home := filepath.Join("/home/hydra", ".pki", "libvirt")

	for _, tc := range []struct {
		name       string
		lookup     func() (*user.User, error)
		wantCerts  []tlsCertDir
		wantCADirs []string
	}{
		{
			name:       "non-root user searches its home first",
			lookup:     func() (*user.User, error) { return &user.User{Uid: "1001", HomeDir: "/home/hydra"}, nil },
			wantCerts:  append([]tlsCertDir{{certPath: home, keyPath: home}}, system...),
			wantCADirs: append([]string{home}, systemCA...),
		},
		{
			name:       "root uses only the system locations",
			lookup:     func() (*user.User, error) { return &user.User{Uid: "0", HomeDir: "/root"}, nil },
			wantCerts:  system,
			wantCADirs: systemCA,
		},
		{
			// go-libvirt dereferences a nil user here; skipping the home
			// directory is this dialer's one deliberate difference.
			name:       "a failed lookup falls back to the system locations",
			lookup:     func() (*user.User, error) { return nil, errors.New("no passwd entry") },
			wantCerts:  system,
			wantCADirs: systemCA,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := currentUser
			currentUser = tc.lookup
			t.Cleanup(func() { currentUser = saved })

			d := newTLSDialer(testHost, testTLSPort, "", time.Second)
			if !slices.Equal(d.certDirs, tc.wantCerts) {
				t.Errorf("certDirs = %v, want %v", d.certDirs, tc.wantCerts)
			}
			if !slices.Equal(d.caDirs, tc.wantCADirs) {
				t.Errorf("caDirs = %v, want %v", d.caDirs, tc.wantCADirs)
			}
		})
	}
}

// With several search directories, an incomplete one is skipped for the next.
func TestTLSDialerFallsThroughToTheNextSearchDirectory(t *testing.T) {
	empty, full := t.TempDir(), newTestPKI(t).clientDir
	d := &tlsDialer{
		host: testHost, port: testTLSPort, timeout: time.Second,
		certDirs: []tlsCertDir{{certPath: empty, keyPath: empty}, {certPath: full, keyPath: full}},
		caDirs:   []string{empty, full},
	}
	if _, err := d.config(); err != nil {
		t.Fatalf("config() = %v, want the second directory's PKI to be used", err)
	}
}

func TestNewTLSDialerDefaultsTheTimeout(t *testing.T) {
	if got := newTLSDialer(testHost, testTLSPort, testPKIPath, 0).timeout; got != defaultConnectTimeout {
		t.Errorf("timeout = %s, want the %s connect default that New also applies", got, defaultConnectTimeout)
	}
}
