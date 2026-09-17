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

package main

import (
	"fmt"
	"os"
	"strconv"

	libvirtprovider "github.com/Petatron/cluster-api-provider-hydra/internal/providers/libvirt"
)

// defaultLocalSocket is where go-libvirt's local dialer looks when no remote
// address is configured. Kept in sync with dialers.NewLocal, which does not
// export it.
const defaultLocalSocket = "/var/run/libvirt/libvirt-sock"

// unsetPlaceholder marks an optional setting that carries no value, so the
// startup log distinguishes "not configured" from an empty string that happened
// to be logged.
const unsetPlaceholder = "<unset>"

// envOr returns the environment variable name, or def when it is unset or empty.
//
// The libvirt settings are site-specific -- somebody's hypervisor address, their
// storage pool, their base image -- so they are supplied by a ConfigMap the
// operator owns and which `make deploy` deliberately does not manage. See
// config/manager/libvirt-config.example.yaml.
//
// Reading them from the environment is what lets that ConfigMap be absent. The
// Deployment used to pass them as `--libvirt-remote-addr=$(LIBVIRT_REMOTE_ADDR)`,
// and Kubernetes leaves an unresolvable $(VAR) *literally in place* rather than
// expanding it to nothing -- so an optional ConfigMap that did not exist would
// hand the flag the literal string "$(LIBVIRT_REMOTE_ADDR)", and the failure
// would surface as an unparseable address rather than as "not configured".
// Defaulting the flag from the environment removes the substitution step
// entirely.
//
// An explicitly passed flag still wins: this only supplies the default.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envBoolOr is envOr for a boolean setting.
//
// An unparseable value is reported rather than silently treated as false,
// because the only boolean here is Insecure, where guessing is the difference
// between TLS and plaintext on a link carrying privileged domain RPCs. The
// caller logs the error; the returned default is the safe direction.
func envBoolOr(name string, def bool) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s=%q is not a boolean: %w", name, v, err)
	}
	return b, nil
}

// describeBackend renders the resolved connection for the startup log.
//
// The backend is not built at startup (see the comment where Config is
// constructed), so without this the first evidence of how the manager will
// reach libvirt arrives only when a HydraMachine reconcile fails. Printing it
// once, up front, is what makes a clobbered or missing ConfigMap visible
// immediately.
//
// No secrets are involved -- an address, a URI and two paths.
func describeBackend(cfg libvirtprovider.Config) []any {
	transport := "local socket " + defaultLocalSocket
	if cfg.RemoteAddr != "" {
		transport = "remote " + cfg.RemoteAddr
		if cfg.Insecure {
			transport += " (plaintext TCP)"
		} else {
			transport += " (TLS)"
		}
	}
	return []any{
		"transport", transport,
		"uri", cfg.URI,
		"storagePool", orUnset(cfg.StoragePool),
		"baseImage", orUnset(cfg.BaseImage),
	}
}

func orUnset(s string) string {
	if s == "" {
		return unsetPlaceholder
	}
	return s
}

// backendConfigMapName is the ConfigMap the Deployment reads its LIBVIRT_*
// variables from. The operator creates it; the kustomization does not ship it.
const backendConfigMapName = "libvirt-config"

// legacyBackendConfigMapName is what the same ConfigMap was called while it was
// still a kustomization resource, since kustomize applies its namePrefix to
// resources it manages. A cluster deployed before that changed still has the
// object under this name, and the manager will not find it.
const legacyBackendConfigMapName = "cluster-api-provider-hydra-libvirt-config"

// backendWarning reports why the configured backend cannot work, or nil when
// nothing is obviously wrong.
//
// This deliberately does not dial anything and never blocks startup. It catches
// the one failure that is both common and silent: the manager running in a pod
// with no remote address, which selects a local socket that is not mounted --
// until now visible only as a per-object condition after the fact (PET-45).
//
// The in-cluster message names both ConfigMap names on purpose. The most likely
// way to reach this state is the rename: `kubectl get cm` shows an object that
// looks entirely correct, because it is still the legacy prefixed one, and an
// operator told only to "check the ConfigMap exists" is sent the wrong way.
//
// socketExists is injected so the check is testable off a real host.
func backendWarning(cfg libvirtprovider.Config, inCluster bool, socketExists func(string) bool) error {
	if cfg.RemoteAddr != "" {
		return nil
	}
	if socketExists(defaultLocalSocket) {
		return nil
	}
	if inCluster {
		return fmt.Errorf(
			"no libvirt remote address is configured and %s does not exist in this container, "+
				"so every machine reconcile will fail to connect: set remoteAddr in the %q "+
				"ConfigMap in this namespace, or mount the hypervisor's socket and pin this pod "+
				"to that node. If this deployment predates the rename, the values are still in "+
				"%q and must be copied to %q -- the old object still exists, so the ConfigMap "+
				"looks present while the manager reads nothing",
			defaultLocalSocket, backendConfigMapName,
			legacyBackendConfigMapName, backendConfigMapName,
		)
	}
	return fmt.Errorf(
		"no libvirt remote address is configured and %s does not exist, so every machine "+
			"reconcile will fail to connect", defaultLocalSocket,
	)
}

// runningInCluster reports whether this process looks like it is in a pod.
// The API server's service env vars are injected into every container and are
// absent under `make run`.
func runningInCluster() bool {
	return os.Getenv("KUBERNETES_SERVICE_HOST") != ""
}

func socketExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
