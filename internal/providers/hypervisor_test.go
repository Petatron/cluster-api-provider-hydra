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
	"strings"
	"testing"
)

// Credentials never print their key, whatever the verb -- %d and %x included,
// which fmt does not route through String -- and however they are held.
func TestTLSCredentialsNeverPrintTheirKey(t *testing.T) {
	const secret = "-----BEGIN PRIVATE KEY-----sekrit"
	creds := TLSCredentials{CACert: []byte("ca"), ClientCert: []byte("cert"), ClientKey: []byte(secret)}

	// The key in every form a verb could render it: as text, and as the bytes
	// %d, %x, %X, %o and %b print a []byte as.
	byteVerbs := []string{"%d", "%x", "%X", "%o", "%b"}
	leaks := make([]string, 0, 1+len(byteVerbs))
	leaks = append(leaks, "sekrit")
	for _, verb := range byteVerbs {
		leaks = append(leaks, strings.Trim(fmt.Sprintf(verb, []byte("sekrit")), "[]"))
	}

	hypervisor := Hypervisor{Address: "hv2.lab.test:16514", Credentials: &creds, CredentialsVersion: "uid/1"}
	byValue := struct{ Credentials TLSCredentials }{creds}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X", "%o", "%b"} {
		for _, v := range []any{creds, &creds, hypervisor, &hypervisor, byValue, []TLSCredentials{creds}} {
			out := fmt.Sprintf(verb, v)
			for _, leak := range leaks {
				if strings.Contains(out, leak) {
					t.Errorf("%s of %T printed the key: %s", verb, v, out)
				}
			}
		}
	}
	for name, out := range map[string]string{
		"String":     creds.String(),
		"GoString":   creds.GoString(),
		"MarshalLog": fmt.Sprint(creds.MarshalLog()),
	} {
		if strings.Contains(out, "sekrit") {
			t.Errorf("%s printed the key: %s", name, out)
		}
	}
}
