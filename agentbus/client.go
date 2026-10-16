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

package agentbus

import (
	"time"

	"github.com/juju/errors"
	"github.com/ontavu/cams-go-utils/utils"
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/req"

	// Registers the "inproc" scheme. Without it every Dial fails with
	// mangos.ErrBadTran.
	_ "go.nanomsg.org/mangos/v3/transport/inproc"
)

// DefaultTimeout bounds one request/reply exchange. An agent wedged in its own
// logic has to surface as an error at the caller rather than as a caller stuck
// forever.
//
// A controller that fronts an agent which itself calls another one should be
// given a longer timeout than the inner hop, so that the inner one is the one
// to expire and the failure is reported where it happened.
const DefaultTimeout = 5 * time.Second

// Client is the request side of the bus. Its methods are safe for concurrent
// use.
type Client struct {
	name string
	sock mangos.Socket
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

// Dial connects to the agent listening at connectURL. The agent must already be
// bound: mangos dials synchronously, so a missing listener is reported here as
// mangos.ErrConnRefused instead of failing later on the first command.
//
// name only labels the log lines.
func Dial(name, connectURL string, opts ...Option) (*Client, error) {
	cfg := settings{timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(&cfg)
	}

	sock, err := req.NewSocket()
	if err != nil {
		return nil, errors.Annotate(err, "req socket")
	}

	// Every option has to be set before the first OpenContext: a context copies
	// the socket's options when it is created and never sees a later change.
	//
	// OptionRetryTime deserves a word. A req socket resends an unanswered
	// request every minute by default, which would replay a command that is not
	// idempotent. Zero turns that off, and mangos then discards instead, which
	// its own source calls the right choice for exactly that reason.
	//
	// OptionFailNoPeers is deliberately left alone. It would report a missing
	// agent without waiting out the deadline, but it makes mangos cancel a
	// request as soon as the last pipe goes away, even when the reply is already
	// on its way back -- so a command answered by an agent that then stops would
	// be reported as a failure although it fully succeeded.
	options := []struct {
		name  string
		value interface{}
	}{
		{mangos.OptionRetryTime, time.Duration(0)},
		{mangos.OptionRecvDeadline, cfg.timeout},
		{mangos.OptionSendDeadline, cfg.timeout},
	}
	for _, opt := range options {
		if err := sock.SetOption(opt.name, opt.value); err != nil {
			_ = sock.Close()
			return nil, errors.Annotatef(err, "set %s", opt.name)
		}
	}

	if err := sock.Dial(connectURL); err != nil {
		_ = sock.Close()
		return nil, errors.Annotatef(err, "dial %s", connectURL)
	}

	return &Client{name: name, sock: sock}, nil
}

// Name returns the label of the peer this client talks to.
func (c *Client) Name() string { return c.name }

// Close releases the socket.
//
// It does not linger: mangos declares OptionLinger and documents it, but
// nothing in the library ever reads it, so anything still queued is abandoned.
// That is fine for a command socket, where a reply is awaited before the next
// request, and it means teardown is prompt.
func (c *Client) Close() error {
	return errors.Trace(c.sock.Close())
}

// Do performs one request/reply exchange and returns the argument of the reply.
//
// Each exchange runs in its own mangos context. Sharing the socket's default
// context between goroutines would be worse than a data race: a second Send
// cancels the request already in flight, so the first caller is told its
// command failed even when the agent has already carried it out.
func (c *Client) Do(verb, arg string) (string, error) {
	ctx, err := c.sock.OpenContext()
	if err != nil {
		return "", errors.Annotate(err, "open context")
	}
	defer func() {
		if cerr := ctx.Close(); cerr != nil {
			// Nothing actionable is left: the exchange is over either way, and
			// the socket outlives the context.
			utils.Logger.Debug().Err(cerr).Str("peer", c.name).Msg("bus context close")
		}
	}()

	if err := ctx.Send(EncodeRequest(verb, arg)); err != nil {
		return "", errors.Annotatef(err, "send %s", verb)
	}

	reply, err := ctx.Recv()
	if err != nil {
		return "", errors.Annotatef(err, "recv %s", verb)
	}

	out, err := DecodeReply(reply)
	return out, errors.Annotatef(err, "reply to %s", verb)
}

// DoTolerateGone is Do for a command whose effect is that the agent stops.
//
// Such a command cannot always be confirmed, and that is not a failure: the
// agent answers and then closes its socket, and mangos cancels an outstanding
// request as soon as the pipe carrying it goes away. With retry disabled,
// RemovePipe cancels the context rather than resend a command it rightly
// considers non-idempotent, so the caller sees ErrCanceled on exactly the
// exchanges that worked best. The peer going away is the success condition, so
// it is reported as one.
func (c *Client) DoTolerateGone(verb, arg string) (string, error) {
	out, err := c.Do(verb, arg)
	if errors.Is(err, mangos.ErrCanceled) || errors.Is(err, mangos.ErrNoPeers) {
		return "", nil
	}
	return out, errors.Trace(err)
}
