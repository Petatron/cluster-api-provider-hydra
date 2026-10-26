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

package providers

import (
	"fmt"
	"io"
)

// Hypervisor is a hypervisor a cluster names, with the credentials to reach it
// (PET-38). It is how a controller asks for a connection other than the
// manager's flag-configured one without knowing anything about the backend.
type Hypervisor struct {
	// Address is the hypervisor's TLS listener as host:port.
	Address string

	// Credentials is the TLS client identity, read from the cluster's Secret.
	Credentials *TLSCredentials

	// CredentialsVersion identifies those credentials -- the Secret's UID and
	// resourceVersion -- so that rotating them opens a connection with the new
	// ones instead of reusing one built with the old.
	CredentialsVersion string
}

// TLSCredentials is a TLS client identity and the CA its server must chain to,
// as PEM: the contents of cacert.pem, clientcert.pem and clientkey.pem.
//
// It holds a private key, so it never prints one. It implements fmt.Formatter,
// which fmt consults before anything else for every verb -- %d and %x included,
// which would otherwise print the key's bytes -- and the same goes for a struct
// holding it. String, GoString and MarshalLog redact too, for callers that use
// them directly and for structured logging. fmt cannot call a method through an
// unexported field, so never hold one by value in one; by pointer it prints as
// an address.
type TLSCredentials struct {
	CACert, ClientCert, ClientKey []byte
}

const redactedCredentials = "TLSCredentials{<redacted>}"

// String implements fmt.Stringer without the key.
func (TLSCredentials) String() string { return redactedCredentials }

// GoString implements fmt.GoStringer without the key.
func (TLSCredentials) GoString() string { return redactedCredentials }

// Format implements fmt.Formatter without the key, whatever the verb.
func (TLSCredentials) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redactedCredentials) }

// MarshalLog implements logr.Marshaler without the key.
func (TLSCredentials) MarshalLog() any { return redactedCredentials }
