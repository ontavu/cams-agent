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

package mediabus

import (
	"context"
	"errors"
	"fmt"

	"github.com/ontavu/cams-go-utils/utils"
	"github.com/ontavu/go-swarm"
	"github.com/rs/zerolog"
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/pull"

	// Registers the "inproc" scheme. Without it every Listen fails with
	// mangos.ErrBadTran.
	_ "go.nanomsg.org/mangos/v3/transport/inproc"
)

// DefaultReceiveDepth is how many frames may wait for the handler. It is the
// upstream's own shock absorber, on top of the pusher's queue.
//
// When it fills, mangos blocks the pipe rather than dropping, so the pressure
// travels back to the pusher and is reported there. Nothing is ever discarded
// silently on this side.
const DefaultReceiveDepth = 128

// Handler consumes one frame. It is called from the serving goroutine, one
// frame at a time and in order, so an upstream whose state is only reached from
// here needs no lock of its own.
//
// The frame's payload aliases the received message and is only valid until the
// handler returns; anything kept beyond that has to be copied. Returning an
// error stops Serve.
type Handler func(ctx context.Context, f Frame) error

// Puller is the passive side of the media bus.
type Puller struct {
	name string
	sock mangos.Socket
}

// ListenPull binds the endpoint at bindURL.
//
// Binding happens here rather than in Serve so that a camera can connect as
// soon as the upstream exists, without waiting for it to be scheduled. A
// duplicate address is reported now, as mangos.ErrAddrInUse.
//
// name only labels the log lines.
func ListenPull(name, bindURL string) (*Puller, error) {
	sock, err := pull.NewSocket()
	if err != nil {
		return nil, fmt.Errorf("pull socket: %w", err)
	}
	if err := sock.SetOption(mangos.OptionReadQLen, DefaultReceiveDepth); err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("set read queue: %w", err)
	}
	if err := sock.Listen(bindURL); err != nil {
		_ = sock.Close()
		return nil, fmt.Errorf("listen %s: %w", bindURL, err)
	}
	return &Puller{name: name, sock: sock}, nil
}

// Close releases the endpoint of a puller that will never Serve. Serve closes
// it on its way out, so this is only for the construction paths that give up
// before starting; calling it twice is harmless.
func (p *Puller) Close() error {
	if err := p.sock.Close(); err != nil && !errors.Is(err, mangos.ErrClosed) {
		return err
	}
	return nil
}

// Serve consumes frames until the context is cancelled, the handler fails, or
// the socket is closed. It closes the socket on the way out, which is what
// makes a camera's next send fail rather than pile up frames nobody reads.
func (p *Puller) Serve(ctx context.Context, h Handler) {
	defer func() {
		if err := p.sock.Close(); err != nil && !errors.Is(err, mangos.ErrClosed) {
			p.warn(err).Msg("media bus close")
		}
	}()

	// The socket knows nothing about contexts, so closing it is what releases a
	// blocked receive. Cancel runs before Wait, so this goroutine is joined.
	watch := swarm.New(ctx)
	defer watch.Wait()
	defer watch.Cancel()
	watch.Run(func(c context.Context) {
		<-c.Done()
		_ = p.sock.Close()
	})

	p.debug().Msg("media bus serving")

	for {
		msg, err := p.sock.RecvMsg()
		if err != nil {
			// A closed socket is how cancellation reaches us, not a failure.
			if errors.Is(err, mangos.ErrClosed) || ctx.Err() != nil {
				p.debug().Msg("media bus stopping")
			} else {
				p.warn(err).Msg("media bus recv")
			}
			return
		}

		frame, derr := DecodeFrame(msg.Body)
		if derr != nil {
			// A frame nobody can read is dropped, loudly. Stopping the upstream
			// over one malformed message would cost the whole stream.
			p.warn(derr).Msg("media bus frame")
			msg.Free()
			continue
		}

		herr := h(ctx, frame)

		// Freeing returns the buffer to the pool that the pusher allocated it
		// from, which is where the saving on this path comes from.
		msg.Free()

		if herr != nil {
			p.warn(herr).Msg("media bus handler")
			return
		}
	}
}

func (p *Puller) warn(err error) *zerolog.Event {
	return utils.Logger.Warn().Err(err).Str("media", p.name)
}

func (p *Puller) debug() *zerolog.Event {
	return utils.Logger.Trace().Str("media", p.name)
}
