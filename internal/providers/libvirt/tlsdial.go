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
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"time"
)

// defaultTLSPort is libvirtd's TLS port, used when the remote address names none.
const defaultTLSPort = "16514"

// tlsVerificationOK is the byte libvirtd writes after the TLS handshake when it
// has accepted the client's certificate. Anything else means it rejected us.
const tlsVerificationOK = 1

// tlsDialer connects to a remote libvirtd over mutual TLS with every step bounded.
//
// It replaces go-libvirt's dialers.TLS (PET-36). That dialer bounds the TCP
// connect and the handshake, but then performs an unbounded read of the one
// byte libvirtd sends to say whether it accepted our client certificate. A peer
// that finished the handshake and never sent that byte hung Dial forever -- and
// because Dial had not returned a net.Conn yet, trackingDialer had nothing to
// close, so every retry against such a peer leaked a socket and a goroutine.
//
// Here a single deadline covers the connect, the handshake and the
// verification read, and the connection is closed on any failure, so Dial
// always returns within its timeout and never leaves a socket behind. The
// deadline is cleared once the connection is established: from then on it
// carries RPCs, which the provider bounds per call.
//
// The certificate search paths are go-libvirt's, unchanged, so swapping dialers
// does not move where an operator's PKI is read from.
type tlsDialer struct {
	host, port string
	timeout    time.Duration
	certDirs   []tlsCertDir
	caDirs     []string
}

// tlsCertDir is one place to look for the client certificate and its key.
type tlsCertDir struct {
	certPath, keyPath string
}

// newTLSDialer returns a dialer for host:port. An empty port means libvirtd's
// default; an empty pkiPath means libvirt's standard locations.
func newTLSDialer(host, port, pkiPath string, timeout time.Duration) *tlsDialer {
	if port == "" {
		port = defaultTLSPort
	}
	d := &tlsDialer{host: host, port: port, timeout: timeout}

	// With a PKI path, everything lives in that one directory -- the layout the
	// libvirt-pki Secret mounts, and what go-libvirt's UsePKIPath means.
	if pkiPath != "" {
		d.certDirs = []tlsCertDir{{certPath: pkiPath, keyPath: pkiPath}}
		d.caDirs = []string{pkiPath}
		return d
	}

	// Otherwise libvirt's system locations, with the key under private/.
	d.certDirs = []tlsCertDir{{certPath: "/etc/pki/libvirt/", keyPath: "/etc/pki/libvirt/private/"}}
	d.caDirs = []string{"/etc/pki/CA/"}

	// A non-root user looks in ~/.pki/libvirt first, as libvirt's own client
	// does. go-libvirt dereferences a nil user when the lookup fails; skipping
	// the home directory is the only difference from it.
	if u, err := user.Current(); err == nil && u.Uid != "0" && u.HomeDir != "" {
		home := filepath.Join(u.HomeDir, ".pki", "libvirt")
		d.certDirs = append([]tlsCertDir{{certPath: home, keyPath: home}}, d.certDirs...)
		d.caDirs = append([]string{home}, d.caDirs...)
	}
	return d
}

// Dial implements socket.Dialer.
func (d *tlsDialer) Dial() (net.Conn, error) {
	conf, err := d.config()
	if err != nil {
		return nil, err
	}

	timeout := d.timeout
	if timeout <= 0 {
		timeout = defaultDialTimeout
	}
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(d.host, d.port)

	raw, err := (&net.Dialer{Deadline: deadline}).Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("libvirt: connecting to %s: %w", addr, err)
	}

	conn := tls.Client(raw, conf)
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("libvirt: setting deadline for %s: %w", addr, err)
	}
	if err := conn.Handshake(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("libvirt: TLS handshake with %s: %w", addr, err)
	}

	// libvirtd writes one byte after the handshake: whether it accepted our
	// client certificate. This is the read that used to have no deadline.
	verdict := make([]byte, 1)
	if _, err := io.ReadFull(conn, verdict); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("libvirt: waiting for %s to verify our client certificate: %w", addr, err)
	}
	if verdict[0] != tlsVerificationOK {
		_ = conn.Close()
		return nil, fmt.Errorf("libvirt: %s rejected our client certificate or address", addr)
	}

	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("libvirt: clearing deadline for %s: %w", addr, err)
	}
	return conn, nil
}

// config loads the client certificate and the CA that must have signed the
// server's certificate.
func (d *tlsDialer) config() (*tls.Config, error) {
	cert, err := d.clientCert()
	if err != nil {
		return nil, err
	}
	roots, err := d.caPool()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		// The server certificate is checked against the address actually dialled,
		// so dialling an IP needs that IP as a SAN on the server's certificate.
		ServerName: d.host,
		MinVersion: tls.VersionTLS12,
	}, nil
}

// clientCert returns the first readable clientcert.pem/clientkey.pem pair.
func (d *tlsDialer) clientCert() (tls.Certificate, error) {
	var errs []error
	for _, dir := range d.certDirs {
		certPEM, err := os.ReadFile(filepath.Join(dir.certPath, "clientcert.pem"))
		if err != nil {
			errs = append(errs, fmt.Errorf("reading TLS client certificate: %w", err))
			continue
		}
		keyPEM, err := os.ReadFile(filepath.Join(dir.keyPath, "clientkey.pem"))
		if err != nil {
			errs = append(errs, fmt.Errorf("reading TLS client key: %w", err))
			continue
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("libvirt: invalid TLS client certificate in %s: %w", dir.certPath, err)
		}
		return cert, nil
	}
	return tls.Certificate{}, fmt.Errorf("libvirt: no TLS client certificate found: %w", errors.Join(errs...))
}

// caPool returns the first readable cacert.pem as a certificate pool.
//
// Stricter than go-libvirt, which returns an empty pool when the file holds no
// certificate: that can only fail the handshake later with a less useful error,
// so it is reported here instead.
func (d *tlsDialer) caPool() (*x509.CertPool, error) {
	var errs []error
	for _, dir := range d.caDirs {
		path := filepath.Join(dir, "cacert.pem")
		caPEM, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading TLS CA certificate: %w", err))
			continue
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("libvirt: %s contains no PEM certificate", path)
		}
		return pool, nil
	}
	return nil, fmt.Errorf("libvirt: no TLS CA certificate found: %w", errors.Join(errs...))
}
