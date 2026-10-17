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

// Package lanctrl is the controller of the LAN agent: the client structure
// through which the rest of the process asks which cameras exist and tells them
// to play or to pause.
//
// It owns the vocabulary the two sides exchange, for the same reason camctrl
// does: putting it in the agent would drag the agent into every package that
// merely wants to talk to it.
package lanctrl

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/errkind"
)

// Command is a request sent to the LAN agent.
type Command string

// The commands understood by the LAN agent. PLAY, PAUSE and MEDIA carry a
// camera identifier; LIST and PING carry nothing.
//
// There is no EXIT. The LAN agent lives as long as the process, so a verb that
// stopped it would only add a way to break the process that nothing needs.
const (
	CommandList  Command = "LIST"
	CommandPlay  Command = "PLAY"
	CommandPause Command = "PAUSE"
	CommandPing  Command = "PING"
	CommandMedia Command = "MEDIA"
)

// DefaultTimeout bounds one exchange with the LAN agent.
//
// It is deliberately longer than camctrl.DefaultTimeout: a PLAY or a MEDIA
// crosses two hops, and the inner one has to be the one that expires, so that a
// wedged camera is reported as a camera failure rather than as an opaque LAN
// timeout.
const DefaultTimeout = 10 * time.Second

// ParseCommand recognises the plain text form of a command. An unknown verb is
// an error rather than a default, so that a controller from a newer revision
// gets a refusal it can log instead of silence.
func ParseCommand(verb string) (Command, error) {
	switch cmd := Command(verb); cmd {
	case CommandList, CommandPlay, CommandPause, CommandPing, CommandMedia:
		return cmd, nil
	case "":
		return "", errors.New("empty command")
	default:
		return "", fmt.Errorf("command %q %w", verb, errkind.ErrNotSupported)
	}
}

// EncodeIDs builds the argument of a LIST reply.
//
// The identifiers are separated by spaces, so one that contains whitespace
// would be read back as several. They come from the ONVIF UUID of a device,
// which is to say from hardware nobody here controls, so the LAN agent rejects
// such an identifier when it registers a camera. This function is the last line
// of defence and skips one rather than emitting a reply it knows is ambiguous.
func EncodeIDs(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || strings.ContainsAny(id, " \t\r\n") {
			continue
		}
		out = append(out, id)
	}
	return strings.Join(out, " ")
}

// DecodeIDs splits the argument of a LIST reply. An empty argument is an empty
// list, not an error: an agent that knows no camera yet is a normal state.
func DecodeIDs(arg string) []string {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return []string{}
	}
	return fields
}

// LanController is what a consumer needs in order to drive the LAN agent. It is
// the canonical name for that contract, declared here beside the vocabulary so
// that every consumer depends on one declaration rather than its own copy.
//
// It omits Ping and Close on purpose: the upstream agent neither probes the LAN
// agent nor owns its socket. Whoever built the client closes it, through the
// concrete type New returns.
type LanController interface {
	List() ([]string, error)
	Play(camID string) error
	Pause(camID string) error
	Media(camID string) (camctrl.Media, error)
}

// Compile-time proof that the client satisfies the contract.
var _ LanController = (*Client)(nil)

// Client is the controller of the LAN agent. Its methods are safe for
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

// New dials the LAN agent listening at connectURL. The agent must already be
// bound, which lanagent.New guarantees.
func New(connectURL string, opts ...Option) (*Client, error) {
	cfg := settings{timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}

	bus, err := agentbus.Dial("lan", connectURL, agentbus.WithTimeout(cfg.timeout))
	if err != nil {
		return nil, err
	}
	return &Client{bus: bus}, nil
}

// Close releases the socket.
func (c *Client) Close() error { return c.bus.Close() }

// List returns the identifiers of the cameras the agent knows about.
func (c *Client) List() ([]string, error) {
	arg, err := c.bus.Do(string(CommandList), "")
	if err != nil {
		return nil, err
	}
	return DecodeIDs(arg), nil
}

// Play asks the agent to have one camera stream upstream.
func (c *Client) Play(camID string) error {
	_, err := c.bus.Do(string(CommandPlay), camID)
	return err
}

// Pause asks the agent to have one camera stop streaming.
func (c *Client) Pause(camID string) error {
	_, err := c.bus.Do(string(CommandPause), camID)
	return err
}

// Media reports what one camera was last seen producing.
//
// The reply is camctrl's own, relayed: the vocabulary is declared once, beside
// the agent that produces it, so that the two hops cannot drift apart.
func (c *Client) Media(camID string) (camctrl.Media, error) {
	arg, err := c.bus.Do(string(CommandMedia), camID)
	if err != nil {
		return camctrl.Media{}, err
	}
	m, err := camctrl.DecodeMedia(arg)
	return m, err
}

// Ping checks that the agent is answering.
func (c *Client) Ping() error {
	_, err := c.bus.Do(string(CommandPing), "")
	return err
}
