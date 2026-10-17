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

// Package camctrl is the controller of a camera agent: the client structure
// through which the rest of the process drives one camera.
//
// The package also owns the vocabulary the two sides exchange. It has to live
// on this side of the seam rather than in the agent, because everything that
// holds a controller would otherwise pull the agent in transitively, and
// keeping the agent out of every package but the process entry point is the
// whole point of the split. The framing underneath belongs to agentbus.
package camctrl

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/errkind"
)

// Command is a request sent to a camera agent.
type Command string

// The commands understood by a camera agent. They are the plain text form
// travelling on the bus, so keep them stable: an agent and a controller may be
// built from different revisions.
const (
	CommandPlay  Command = "PLAY"
	CommandPause Command = "PAUSE"
	CommandPing  Command = "PING"
	CommandExit  Command = "EXIT"
	CommandState Command = "STATE"
	CommandMedia Command = "MEDIA"
)

// State is the state of a camera agent, as reported by CommandState.
type State string

// The states a camera agent may report. They mirror the internal state machine
// of the agent one for one.
const (
	StateOff      State = "OFF"
	StateIdle     State = "IDLE"
	StatePlaying  State = "PLAYING"
	StatePausing  State = "PAUSING"
	StateResuming State = "RESUMING"
)

// DefaultTimeout bounds one exchange with a camera agent.
//
// A camera's handler performs no I/O -- starting a stream spawns a goroutine
// group and returns -- so this only ever fires on a wedged agent. It is kept
// below the timeout of the LAN agent's controller on purpose, so that a wedged
// camera is reported as a camera failure rather than as a LAN timeout.
const DefaultTimeout = 5 * time.Second

// CameraController is what a consumer needs in order to drive a camera. It is
// the canonical name for that contract, declared here beside the vocabulary so
// that every consumer depends on one declaration rather than its own copy.
//
// It is deliberately narrower than *Client: the LAN agent commands cameras and
// only inspects what it has to pass on. That is why New returns the concrete
// type -- returning this interface instead would put State out of reach of
// everybody, including the tests.
//
// Media is here because the LAN agent does have to relay it: the hub wants to
// know what a camera produces before it commits a pipeline to it, and the only
// thing that knows is the camera agent that described the stream.
//
// **Ping is here because leaving it out broke the Play path**, and it is the
// clearest example of what this file warns about. A camera asked to play while
// it is still stopping parks in StateResuming and waits for a ping to finish
// the transition; the LAN agent owns the cameras and holds only this interface;
// the verb existed on *Client and not here, so nobody could send it, and the
// camera never restarted. Narrower than the client is right. Narrower than what
// the agent has to do is a bug.
type CameraController interface {
	Play() error
	Pause() error
	Ping() error
	Exit() error
	Media() (Media, error)
	Close() error
}

// Compile-time proof that the client satisfies the contract.
var _ CameraController = (*Client)(nil)

// ParseCommand recognises the plain text form of a command. An unknown verb is
// an error rather than a default, so that a controller from a newer revision
// gets a refusal it can log instead of silence.
func ParseCommand(verb string) (Command, error) {
	switch cmd := Command(verb); cmd {
	case CommandPlay, CommandPause, CommandPing, CommandExit, CommandState,
		CommandMedia:
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
	case StateOff, StateIdle, StatePlaying, StatePausing, StateResuming:
		return st, nil
	default:
		return "", fmt.Errorf("state %q %w", token, errkind.ErrNotValid)
	}
}

// Encoding is a video encoding a camera agent may report.
//
// The tokens are the plain text form travelling on the bus and they match the
// names ONVIF uses in VideoEncoderConfiguration, so keep them stable: an agent
// and a controller may be built from different revisions.
type Encoding string

// The encodings a camera agent may report.
const (
	EncodingUnspecified Encoding = "UNSPECIFIED"
	EncodingH264        Encoding = "H264"
	EncodingH265        Encoding = "H265"
	EncodingJPEG        Encoding = "JPEG"
)

// Media is what a camera agent reports about the stream it last set up, as
// reported by CommandMedia.
//
// The zero value is what a camera that has never streamed says: a registered
// camera nobody has asked to play is an ordinary state and not an error. Once
// observed it is kept, because a camera's encoding does not change while it
// runs -- a pause should not throw away what is known about it.
type Media struct {
	Encoding Encoding
	Width    int
	Height   int

	// GopLength is the camera's advertised GOP in frames, zero when it reports
	// none. ONVIF only carries it for H.264.
	GopLength int
}

// Unknown reports whether the agent has nothing to say about its media yet.
func (m Media) Unknown() bool {
	return m.Encoding == "" || m.Encoding == EncodingUnspecified
}

// ParseEncoding recognises the plain text form of an encoding.
//
// An unrecognised token becomes EncodingUnspecified rather than an error, which
// is the opposite of what ParseCommand does with an unknown verb, on purpose: a
// command cannot be half-executed and has to be refused, whereas a value from a
// newer agent is better read as "an encoding this revision does not know" than
// as a failure that also loses the registration it was travelling in.
func ParseEncoding(token string) Encoding {
	switch enc := Encoding(strings.ToUpper(strings.TrimSpace(token))); enc {
	case EncodingH264, EncodingH265, EncodingJPEG:
		return enc
	default:
		return EncodingUnspecified
	}
}

// EncodeMedia builds the argument of a MEDIA reply: the encoding and three
// numbers, separated by spaces.
func EncodeMedia(m Media) string {
	enc := m.Encoding
	if enc == "" {
		enc = EncodingUnspecified
	}
	return fmt.Sprintf("%s %d %d %d", enc, m.Width, m.Height, m.GopLength)
}

// DecodeMedia reads back what EncodeMedia wrote.
//
// Trailing fields are ignored rather than refused, so that an agent from a
// newer revision reporting more about itself is still understood here.
func DecodeMedia(arg string) (Media, error) {
	fields := strings.Fields(arg)
	if len(fields) < 4 {
		return Media{}, fmt.Errorf("media reply %q %w", arg, errkind.ErrNotValid)
	}

	out := Media{Encoding: ParseEncoding(fields[0])}
	for i, into := range []*int{&out.Width, &out.Height, &out.GopLength} {
		n, err := strconv.Atoi(fields[i+1])
		if err != nil {
			return Media{}, fmt.Errorf("media reply %q %w", arg, errkind.ErrNotValid)
		}
		if n < 0 {
			return Media{}, fmt.Errorf("media reply %q %w", arg, errkind.ErrNotValid)
		}
		*into = n
	}
	return out, nil
}

// Client is the controller of one camera agent. Its methods are safe for
// concurrent use.
type Client struct {
	id  string
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

// New dials the camera agent listening at connectURL. The agent must already be
// bound, which camagent.New guarantees.
func New(id, connectURL string, opts ...Option) (*Client, error) {
	cfg := settings{timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}

	bus, err := agentbus.Dial("cam "+id, connectURL, agentbus.WithTimeout(cfg.timeout))
	if err != nil {
		return nil, err
	}
	return &Client{id: id, bus: bus}, nil
}

// Close releases the socket.
func (c *Client) Close() error { return c.bus.Close() }

// Play asks the agent to stream its media upstream.
func (c *Client) Play() error {
	_, err := c.bus.Do(string(CommandPlay), "")
	return err
}

// Pause asks the agent to stop streaming, keeping the agent alive.
func (c *Client) Pause() error {
	_, err := c.bus.Do(string(CommandPause), "")
	return err
}

// Ping lets the agent advance the transient states of its state machine.
func (c *Client) Ping() error {
	_, err := c.bus.Do(string(CommandPing), "")
	return err
}

// Exit asks the agent to stop for good. Run returns shortly afterwards.
//
// The confirmation is best effort: the agent answers and then closes its
// socket, which mangos reports to the caller as a cancelled request. See
// agentbus.Client.DoTolerateGone.
func (c *Client) Exit() error {
	_, err := c.bus.DoTolerateGone(string(CommandExit), "")
	return err
}

// Media reports what the agent last set up, or the zero value if it never has.
func (c *Client) Media() (Media, error) {
	arg, err := c.bus.Do(string(CommandMedia), "")
	if err != nil {
		return Media{}, err
	}
	m, err := DecodeMedia(arg)
	return m, err
}

// State reports what the agent is currently doing.
func (c *Client) State() (State, error) {
	arg, err := c.bus.Do(string(CommandState), "")
	if err != nil {
		return "", err
	}
	st, err := ParseState(arg)
	return st, err
}
