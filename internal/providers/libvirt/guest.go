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
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/Petatron/cluster-api-provider-hydra/internal/providers"
)

// guestAgentTimeout bounds one guest-agent command, in seconds. The agent
// answers guest-exec and guest-exec-status at once; how long the program
// itself runs is the caller's to bound.
const guestAgentTimeout = 10

// StartInGuest implements providers.MachineProvider through the QEMU guest
// agent's guest-exec, with the program's output captured for GuestStatus.
func (p *Provider) StartInGuest(ctx context.Context, id, path string) (int, error) {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()

	dom, err := p.runningDomain(ctx, id)
	if err != nil {
		return 0, err
	}
	var started struct {
		PID int `json:"pid"`
	}
	if err := p.agentCommand(ctx, dom, "guest-exec", map[string]any{"path": path, "capture-output": true}, &started); err != nil {
		if !errors.Is(err, providers.ErrGuestRefused) && !agentNotConnected(err) {
			// Sent, but no answer: the guest may have started it.
			err = fmt.Errorf("%w: %v", providers.ErrGuestOutcomeUnknown, err)
		}
		return 0, fmt.Errorf("libvirt: starting %s in domain %q: %w", path, dom.Name, err)
	}
	return started.PID, nil
}

// GuestStatus implements providers.MachineProvider through the QEMU guest
// agent's guest-exec-status. The agent forgets a program once its exit has
// been reported, so how it ended is answered only once.
func (p *Provider) GuestStatus(ctx context.Context, id string, pid int) (*providers.GuestRun, error) {
	ctx, cancel, err := p.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	dom, err := p.runningDomain(ctx, id)
	if err != nil {
		return nil, err
	}
	var st guestExecStatus
	if err := p.agentCommand(ctx, dom, "guest-exec-status", map[string]any{"pid": pid}, &st); err != nil {
		return nil, fmt.Errorf("libvirt: reading the status of pid %d in domain %q: %w", pid, dom.Name, err)
	}
	if !st.Exited {
		return nil, nil
	}
	return st.run()
}

// runningDomain looks up the machine's domain and requires it to be running.
func (p *Provider) runningDomain(ctx context.Context, id string) (golibvirt.Domain, error) {
	dom, err := p.lookupByUUID(ctx, id)
	if err != nil {
		return golibvirt.Domain{}, err
	}
	active, err := call(ctx, p, func() (int32, error) { return p.client().DomainIsActive(dom) })
	if err != nil {
		if isNotFound(err) {
			return golibvirt.Domain{}, providers.ErrNotFound
		}
		return golibvirt.Domain{}, fmt.Errorf("libvirt: checking whether domain %q is running: %w", dom.Name, err)
	}
	if active == 0 {
		return golibvirt.Domain{}, providers.ErrNotRunning
	}
	return dom, nil
}

// guestExecStatus is the guest agent's answer to guest-exec-status.
type guestExecStatus struct {
	Exited   bool   `json:"exited"`
	ExitCode *int   `json:"exitcode,omitempty"`
	Signal   *int   `json:"signal,omitempty"`
	OutData  string `json:"out-data,omitempty"`
	ErrData  string `json:"err-data,omitempty"`
}

func (st guestExecStatus) run() (*providers.GuestRun, error) {
	out, err := base64.StdEncoding.DecodeString(st.OutData)
	if err != nil {
		return nil, fmt.Errorf("libvirt: decoding guest output: %w", err)
	}
	errOut, err := base64.StdEncoding.DecodeString(st.ErrData)
	if err != nil {
		return nil, fmt.Errorf("libvirt: decoding guest error output: %w", err)
	}
	run := &providers.GuestRun{Output: string(out) + string(errOut)}
	switch {
	case st.ExitCode != nil:
		run.ExitCode = *st.ExitCode
	case st.Signal != nil:
		// Killed by a signal: report it the way a shell would.
		run.ExitCode = 128 + *st.Signal
	default:
		return nil, errors.New("libvirt: the guest agent reported an exit with neither a code nor a signal")
	}
	return run, nil
}

// agentCommand sends one guest-agent command and decodes its "return" value
// into into.
func (p *Provider) agentCommand(ctx context.Context, dom golibvirt.Domain, execute string, args map[string]any, into any) error {
	cmd, err := json.Marshal(map[string]any{"execute": execute, "arguments": args})
	if err != nil {
		return err
	}
	res, err := call(ctx, p, func() (golibvirt.OptString, error) {
		return p.client().QEMUDomainAgentCommand(dom, string(cmd), guestAgentTimeout, 0)
	})
	if err != nil {
		if isAgentRefusal(err) {
			return fmt.Errorf("%w: %v", providers.ErrGuestRefused, err)
		}
		return err
	}
	if len(res) == 0 {
		return fmt.Errorf("the guest agent returned nothing for %s", execute)
	}
	var reply struct {
		Return json.RawMessage `json:"return"`
	}
	if err := json.Unmarshal([]byte(res[0]), &reply); err != nil {
		return fmt.Errorf("decoding the guest agent's reply to %s: %w", execute, err)
	}
	if len(reply.Return) == 0 {
		return fmt.Errorf("the guest agent's reply to %s has no return value", execute)
	}
	return json.Unmarshal(reply.Return, into)
}

// isAgentRefusal reports whether err is the guest agent answering with an
// error of its own -- a program it cannot run, a pid it does not know -- as
// opposed to the agent not answering at all (ErrAgentUnresponsive), which is
// worth retrying.
func isAgentRefusal(err error) bool {
	var lerr golibvirt.Error
	if !errors.As(err, &lerr) {
		return false
	}
	if lerr.Code == uint32(golibvirt.ErrAgentCommandFailed) {
		return true
	}
	// Older libvirt reports the agent's own error as an internal error.
	return lerr.Code == uint32(golibvirt.ErrInternalError) &&
		strings.Contains(lerr.Message, "unable to execute QEMU agent command")
}

// agentNotConnected reports whether libvirt refused a guest-agent command
// before sending it, because no agent is connected: then nothing ran.
func agentNotConnected(err error) bool {
	var lerr golibvirt.Error
	return errors.As(err, &lerr) && lerr.Code == uint32(golibvirt.ErrAgentUnresponsive) &&
		strings.Contains(lerr.Message, "not connected")
}
