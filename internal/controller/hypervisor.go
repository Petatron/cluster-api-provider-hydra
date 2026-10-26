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

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/Petatron/cluster-api-provider-hydra/api/v1alpha1"
	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// defaultHypervisorPort is libvirt's TLS port, which spec.hypervisor.address
// defaults to. The API documents the same number.
const defaultHypervisorPort = "16514"

// The keys of a hypervisor credentials Secret: the file names libvirt reads
// from its PKI directory, so the same three files can be loaded into either.
const (
	hypervisorCACertKey     = "cacert.pem"
	hypervisorClientCertKey = "clientcert.pem"
	hypervisorClientKeyKey  = "clientkey.pem"
)

// errWaitingForHypervisor reports that a machine's cluster names a hypervisor
// the HydraCluster controller has not confirmed yet. New machines are created
// there only once it has, so a machine made meanwhile cannot slip onto a
// hypervisor that the cluster's existing machines turn out not to be on.
var errWaitingForHypervisor = errors.New("waiting for the HydraCluster to confirm its hypervisor")

// errWaitingForHypervisorCredentials reports that a machine records a
// hypervisor its HydraCluster does not currently name -- gone, unreadable, or
// naming another -- so there are no credentials to reach it with. A wait, never
// a fallback: the manager's flag connection may be another hypervisor
// entirely, where a lookup by name could find, and teardown destroy, a domain
// that is not this machine's.
var errWaitingForHypervisorCredentials = errors.New("waiting for credentials for the machine's hypervisor")

// canonicalHypervisor returns address as host:port, filling in libvirt's TLS
// port, so one hypervisor written two ways is recorded and compared as one.
// An address no CRD pattern catches -- a port out of range -- is terminal:
// the spec has to change.
func canonicalHypervisor(address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		// No port, which the API allows. A bracketed IPv6 address loses its
		// brackets here and JoinHostPort puts them back.
		host, port = strings.TrimSuffix(strings.TrimPrefix(address, "["), "]"), defaultHypervisorPort
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || host == "" {
		return "", fmt.Errorf("%w: hypervisor address %q is not host or host:port with a port from 1 to 65535",
			providers.ErrTerminal, address)
	}
	return net.JoinHostPort(host, port), nil
}

// hypervisorOf resolves the hypervisor a HydraCluster names into what the
// connection pool needs, reading its credentials Secret. Nil when it names
// none: the manager's flag connection.
//
// The Secret is read through reader, which must be uncached: see
// HydraMachineReconciler.APIReader. A Secret that is missing, lacks a key, or
// holds something that is not a usable certificate is terminal -- the operator
// has to fix the Secret -- and says which.
func hypervisorOf(ctx context.Context, reader client.Reader, hydraCluster *infrav1.HydraCluster) (*providers.Hypervisor, error) {
	if hydraCluster == nil || hydraCluster.Spec.Hypervisor == nil {
		return nil, nil
	}
	address, err := canonicalHypervisor(hydraCluster.Spec.Hypervisor.Address)
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, fmt.Errorf("%w: no reader is configured for hypervisor credentials", providers.ErrTerminal)
	}
	name := hydraCluster.Spec.Hypervisor.CredentialsSecretRef.Name
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: hydraCluster.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: hypervisor credentials Secret %q does not exist", providers.ErrTerminal, name)
		}
		return nil, fmt.Errorf("reading hypervisor credentials Secret %q: %w", name, err)
	}
	creds := &providers.TLSCredentials{
		CACert:     secret.Data[hypervisorCACertKey],
		ClientCert: secret.Data[hypervisorClientCertKey],
		ClientKey:  secret.Data[hypervisorClientKeyKey],
	}
	if err := checkCredentials(creds); err != nil {
		return nil, fmt.Errorf("%w: hypervisor credentials Secret %q: %v", providers.ErrTerminal, name, err)
	}
	return &providers.Hypervisor{
		Address:            address,
		Credentials:        creds,
		CredentialsVersion: string(secret.UID) + "/" + secret.ResourceVersion,
	}, nil
}

// checkCredentials refuses credentials that could never complete a handshake,
// so a broken Secret is reported against the object that names it rather than
// as a hypervisor that seems not to answer.
func checkCredentials(creds *providers.TLSCredentials) error {
	for _, kv := range []struct {
		key   string
		value []byte
	}{
		{hypervisorCACertKey, creds.CACert},
		{hypervisorClientCertKey, creds.ClientCert},
		{hypervisorClientKeyKey, creds.ClientKey},
	} {
		if len(kv.value) == 0 {
			return fmt.Errorf("key %s is missing or empty", kv.key)
		}
	}
	if _, err := tls.X509KeyPair(creds.ClientCert, creds.ClientKey); err != nil {
		return fmt.Errorf("%s and %s are not a matching certificate and key: %v", hypervisorClientCertKey, hypervisorClientKeyKey, err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(creds.CACert) {
		return fmt.Errorf("%s holds no PEM certificate", hypervisorCACertKey)
	}
	return nil
}

// confirmedHypervisor reports whether a HydraCluster has confirmed the
// hypervisor it names: every machine it had was found there.
func confirmedHypervisor(hydraCluster *infrav1.HydraCluster) bool {
	return hydraCluster != nil && hydraCluster.Spec.Hypervisor != nil && hydraCluster.Status.Hypervisor != nil &&
		hydraCluster.Status.Hypervisor.Address == hydraCluster.Spec.Hypervisor.Address
}
