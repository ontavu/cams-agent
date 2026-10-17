// Copyright (c) 2022-2024 The authors (see the AUTHORS file)
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

// Package upctrl is the controller of the upstream agent: the client structure
// through which the rest of the process asks about the connection to the hub.
//
// Its vocabulary is deliberately thin. Nothing inside the process commands the
// upstream agent -- its orders arrive from the hub over gRPC -- so what is left
// is introspection: whether the link is up, and whether the agent is answering
// at all.
package upctrl

import (
	"errors"
	"fmt"
	"time"

	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/errkind"
)

// Command is a request sent to the upstream agent.
type Command string

// The commands understood by the upstream agent.
//
// There is no PLAY or PAUSE: those reach the agent from the hub, not from here.
// There is no EXIT either, for the same reason as the LAN agent -- the upstream
// agent lives as long as the process.
const (
	CommandPing  Command = "PING"
	CommandState Command = "STATE"
)

// State is what the upstream agent reports about its link to the hub.
type State string

const (
	// StateDown means no usable connection to the hub's control plane. The
	// agent is still there, and still retrying.
	StateDown State = "DOWN"

	// StateUp means the control stream is established and registrations are
	// going out.
	StateUp State = "UP"
)

// DefaultTimeout bounds one exchange with the upstream agent. Both its verbs
// answer from memory, so this only fires on an agent that is wedged.
const DefaultTimeout = 5 * time.Second

// ParseCommand recognises the plain text form of a command. An unknown verb is
// an error rather than a default, so that a controller from a newer revision
// gets a refusal it can log instead of silence.
func ParseCommand(verb string) (Command, error) {
	switch cmd := Command(verb); cmd {
	case CommandPing, CommandState:
		return cmd, nil
	case "":
		return "", errors.New("empty command")
	default:
		return "", fmt.Errorf("command %q %w", verb, errkind.ErrNotSupported)
	}
}

// ParseState recognises the plain text form of a state.
func ParseState(token string) (State, error) {
	switch st := State(token); st {
	case StateDown, StateUp:
		return st, nil
	default:
		return "", fmt.Errorf("state %q %w", token, errkind.ErrNotValid)
	}
}

// UpstreamController is what a consumer needs in order to inspect the link to
// the hub. It is the canonical name for that contract, declared here beside the
// vocabulary so that every consumer depends on one declaration.
//
// Nothing in the process holds one yet; the upstream agent takes its orders
// from the hub, not from the bus. It exists so that a supervisor or a CLI can
// ask whether the link is up.
type UpstreamController interface {
	Ping() error
	State() (State, error)
	Close() error
}

// Compile-time proof that the client satisfies the contract.
var _ UpstreamController = (*Client)(nil)

// Client is the controller of the upstream agent. Its methods are safe for
// concurrent use.
type Client struct {
	bus *agentbus.Client
}

// Option tunes a Client at construction.
type Option func(*settings)

type settings struct {
	timeout time.Duration
}

// WithTimeout overrides DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(s *settings) { s.timeout = d }
}

// New dials the upstream agent listening at connectURL. The agent must already
// be bound, which upagent.New guarantees.
func New(connectURL string, opts ...Option) (*Client, error) {
	cfg := settings{timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}

	bus, err := agentbus.Dial("upstream", connectURL, agentbus.WithTimeout(cfg.timeout))
	if err != nil {
		return nil, err
	}
	return &Client{bus: bus}, nil
}

// Close releases the socket.
func (c *Client) Close() error { return c.bus.Close() }

// Ping checks that the agent is answering.
func (c *Client) Ping() error {
	_, err := c.bus.Do(string(CommandPing), "")
	return err
}

// State reports whether the link to the hub is up.
func (c *Client) State() (State, error) {
	arg, err := c.bus.Do(string(CommandState), "")
	if err != nil {
		return "", err
	}
	st, err := ParseState(arg)
	return st, err
}
