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
	"encoding/json"
	"errors"
	"strings"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// agentScript answers guest-exec with pid 42 and guest-exec-status with the
// replies given, in order, recording every command.
func agentScript(t *testing.T, statuses ...string) (func(string) (string, error), *[]map[string]any) {
	t.Helper()
	var sent []map[string]any
	return func(cmd string) (string, error) {
		var c map[string]any
		if err := json.Unmarshal([]byte(cmd), &c); err != nil {
			t.Fatalf("the provider sent a command that is not JSON: %q", cmd)
		}
		sent = append(sent, c)
		switch c["execute"] {
		case "guest-exec":
			return `{"return":{"pid":42}}`, nil
		case "guest-exec-status":
			if len(statuses) == 0 {
				t.Fatalf("unexpected guest-exec-status")
			}
			s := statuses[0]
			statuses = statuses[1:]
			return s, nil
		}
		return "", errors.New("unknown command")
	}, &sent
}

func TestStartInGuestAndStatus(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("cp-1", "<domain/>", true)
	agent, sent := agentScript(t,
		`{"return":{"exited":false}}`,
		// "handed over\n" and "warn\n", base64.
		`{"return":{"exited":true,"exitcode":0,"out-data":"aGFuZGVkIG92ZXIK","err-data":"d2Fybgo="}}`,
	)
	d.agent = agent
	id := formatUUID(d.dom.UUID)

	pid, err := p.StartInGuest(t.Context(), id, "/usr/local/sbin/hook")
	if err != nil || pid != 42 {
		t.Fatalf("StartInGuest = %d, %v", pid, err)
	}
	start := (*sent)[0]
	args, _ := start["arguments"].(map[string]any)
	if start["execute"] != "guest-exec" || args["path"] != "/usr/local/sbin/hook" || args["capture-output"] != true {
		t.Fatalf("guest-exec sent as %v", start)
	}

	run, err := p.GuestStatus(t.Context(), id, pid)
	if err != nil || run != nil {
		t.Fatalf("a running program must read as nil, got %+v, %v", run, err)
	}
	run, err = p.GuestStatus(t.Context(), id, pid)
	if err != nil || run == nil || run.ExitCode != 0 || run.Output != "handed over\nwarn\n" {
		t.Fatalf("GuestStatus = %+v, %v", run, err)
	}
	status := (*sent)[1]
	if a, _ := status["arguments"].(map[string]any); status["execute"] != "guest-exec-status" || a["pid"] != float64(42) {
		t.Fatalf("guest-exec-status sent as %v", status)
	}
}

func TestGuestStatusReportsExitCodesAndSignals(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("cp-1", "<domain/>", true)
	d.agent, _ = agentScript(t,
		`{"return":{"exited":true,"exitcode":3}}`,
		`{"return":{"exited":true,"signal":9}}`,
		`{"return":{"exited":true}}`,
	)
	id := formatUUID(d.dom.UUID)
	if run, err := p.GuestStatus(t.Context(), id, 42); err != nil || run.ExitCode != 3 {
		t.Fatalf("exit 3: %+v, %v", run, err)
	}
	if run, err := p.GuestStatus(t.Context(), id, 42); err != nil || run.ExitCode != 128+9 {
		t.Fatalf("signal 9: %+v, %v", run, err)
	}
	if _, err := p.GuestStatus(t.Context(), id, 42); err == nil {
		t.Fatal("an exit with neither code nor signal must be an error, never success")
	}
}

func TestGuestCallsRefuseMachinesNotRunning(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("cp-1", "<domain/>", false)
	d.agent, _ = agentScript(t)
	id := formatUUID(d.dom.UUID)
	if _, err := p.StartInGuest(t.Context(), id, "/x"); !errors.Is(err, providers.ErrNotRunning) {
		t.Fatalf("stopped domain: %v", err)
	}
	if _, err := p.GuestStatus(t.Context(), id, 1); !errors.Is(err, providers.ErrNotRunning) {
		t.Fatalf("stopped domain: %v", err)
	}
	if _, err := p.StartInGuest(t.Context(), "00000000-0000-0000-0000-0000000000ff", "/x"); !errors.Is(err, providers.ErrNotFound) {
		t.Fatalf("missing domain: %v", err)
	}
}

func TestStartInGuestWithoutAgentIsAnError(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("cp-1", "<domain/>", true)
	_, err := p.StartInGuest(t.Context(), formatUUID(d.dom.UUID), "/x")
	if err == nil || errors.Is(err, providers.ErrNotRunning) || errors.Is(err, providers.ErrNotFound) ||
		!strings.Contains(err.Error(), "starting /x") {
		t.Fatalf("no agent: want a retryable error, got %v", err)
	}
}

// The guest agent refusing a request is final; the agent not answering is not.
func TestGuestRefusalsAreFinalAndSilenceIsNot(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("cp-1", "<domain/>", true)
	id := formatUUID(d.dom.UUID)
	for _, tc := range []struct {
		name    string
		err     golibvirt.Error
		refused bool
	}{
		{"program missing (modern libvirt)", golibvirt.Error{Code: uint32(golibvirt.ErrAgentCommandFailed), Message: "Failed to execute child process (No such file or directory)"}, true},
		{"pid unknown (older libvirt)", golibvirt.Error{Code: uint32(golibvirt.ErrInternalError), Message: "internal error: unable to execute QEMU agent command 'guest-exec-status': Invalid parameter 'pid'"}, true},
		{"agent not answering", golibvirt.Error{Code: uint32(golibvirt.ErrAgentUnresponsive), Message: "Guest agent is not responding"}, false},
		{"another internal error", golibvirt.Error{Code: uint32(golibvirt.ErrInternalError), Message: "internal error: client socket is closed"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lerr := tc.err
			d.agent = func(string) (string, error) { return "", lerr }
			_, err := p.StartInGuest(t.Context(), id, "/x")
			if errors.Is(err, providers.ErrGuestRefused) != tc.refused {
				t.Fatalf("StartInGuest: %v; refused want %v", err, tc.refused)
			}
			_, err = p.GuestStatus(t.Context(), id, 1)
			if errors.Is(err, providers.ErrGuestRefused) != tc.refused {
				t.Fatalf("GuestStatus: %v; refused want %v", err, tc.refused)
			}
		})
	}
}

func TestGuestStatusRejectsBadBase64(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("cp-1", "<domain/>", true)
	d.agent, _ = agentScript(t, `{"return":{"exited":true,"exitcode":0,"out-data":"!!!"}}`)
	if _, err := p.GuestStatus(t.Context(), formatUUID(d.dom.UUID), 42); err == nil {
		t.Fatal("undecodable output must be an error")
	}
}

// A guest-exec that libvirt never sent (no agent connected) is a plain
// failure; one sent but unanswered has an unknown outcome.
func TestStartInGuestTellsUnsentFromUnanswered(t *testing.T) {
	p, f := newFakeProvider(t)
	d := f.addDomain("cp-1", "<domain/>", true)
	id := formatUUID(d.dom.UUID)
	for _, tc := range []struct {
		name    string
		err     golibvirt.Error
		unknown bool
	}{
		// libvirt 12.0.0's messages (src/qemu/qemu_agent.c).
		{"not connected", golibvirt.Error{Code: uint32(golibvirt.ErrAgentUnresponsive), Message: "Guest agent is not responding: QEMU guest agent is not connected"}, false},
		{"guest-sync unanswered", golibvirt.Error{Code: uint32(golibvirt.ErrAgentUnresponsive), Message: "Guest agent is not responding: guest agent didn't respond to synchronization within '10' seconds"}, false},
		{"command unanswered", golibvirt.Error{Code: uint32(golibvirt.ErrAgentCommandTimeout), Message: "Guest agent command timed out: guest agent didn't respond to command within '10' seconds"}, true},
		{"agent lost mid command", golibvirt.Error{Code: uint32(golibvirt.ErrAgentUnresponsive), Message: "Guest agent is not responding: Guest agent disappeared while executing command"}, true},
		{"connection dropped", golibvirt.Error{Code: uint32(golibvirt.ErrInternalError), Message: "internal error: client socket is closed"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lerr := tc.err
			d.agent = func(string) (string, error) { return "", lerr }
			_, err := p.StartInGuest(t.Context(), id, "/x")
			if err == nil || errors.Is(err, providers.ErrGuestOutcomeUnknown) != tc.unknown {
				t.Fatalf("%v; unknown want %v", err, tc.unknown)
			}
		})
	}
}
