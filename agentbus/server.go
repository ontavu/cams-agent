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
	"context"

	"github.com/juju/errors"
	"github.com/ontavu/cams-go-utils/utils"
	"github.com/ontavu/go-swarm"
	"github.com/rs/zerolog"
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/rep"

	// Registers the "inproc" scheme. Without it every Listen fails with
	// mangos.ErrBadTran.
	_ "go.nanomsg.org/mangos/v3/transport/inproc"
)

// Handler answers one request. It is called from the serving goroutine, one
// request at a time, so an agent whose state is only reached from here needs no
// lock of its own.
//
// The reply is built with EncodeOK or EncodeErr and is never empty: an
// unrecognised verb has to be refused rather than ignored, so that a controller
// of another revision learns about it. exit reports that the agent has been
// asked to stop, in which case the reply still goes out first.
type Handler func(ctx context.Context, verb, arg string) (reply []byte, exit bool)

// Server is the reply side of the bus.
type Server struct {
	name string
	sock mangos.Socket
}

// Listen binds the endpoint at bindURL.
//
// Binding happens here rather than in Serve because a controller dials
// synchronously: were the socket bound only once Serve got scheduled, a
// controller built right after this call would be refused. It also means a
// duplicate address is reported now, as mangos.ErrAddrInUse, instead of
// silently sharing an endpoint.
//
// name only labels the log lines.
func Listen(name, bindURL string) (*Server, error) {
	sock, err := rep.NewSocket()
	if err != nil {
		return nil, errors.Annotate(err, "rep socket")
	}

	// Without a send deadline, a reply to a peer whose queue is full blocks
	// until that peer's pipe goes away, and the serving goroutine is the one
	// waiting -- so one wedged caller would stop the agent answering anybody.
	if err := sock.SetOption(mangos.OptionSendDeadline, DefaultTimeout); err != nil {
		_ = sock.Close()
		return nil, errors.Annotate(err, "set send deadline")
	}

	if err := sock.Listen(bindURL); err != nil {
		_ = sock.Close()
		return nil, errors.Annotatef(err, "listen %s", bindURL)
	}
	return &Server{name: name, sock: sock}, nil
}

// Close releases the endpoint of a server that will never Serve. Serve closes
// the socket on its way out, so this is only for the construction paths that
// give up before starting; calling it twice is harmless.
func (s *Server) Close() error {
	if err := s.sock.Close(); err != nil && !errors.Is(err, mangos.ErrClosed) {
		return errors.Trace(err)
	}
	return nil
}

// Serve answers requests until the context is cancelled or a handler reports
// that the agent should stop. It closes the socket on the way out.
func (s *Server) Serve(ctx context.Context, h Handler) {
	defer func() {
		if err := s.sock.Close(); err != nil && !errors.Is(err, mangos.ErrClosed) {
			s.warn(err).Msg("bus close")
		}
	}()

	// The socket knows nothing about contexts, so closing it is what releases a
	// blocked Recv. Cancel runs before Wait, so this goroutine is always joined.
	watch := swarm.New(ctx)
	defer watch.Wait()
	defer watch.Cancel()
	watch.Run(func(c context.Context) {
		<-c.Done()
		_ = s.sock.Close()
	})

	s.debug().Msg("bus serving")

	for {
		request, err := s.sock.Recv()
		if err != nil {
			// A closed socket is how cancellation reaches us, not a failure.
			if errors.Is(err, mangos.ErrClosed) || ctx.Err() != nil {
				s.debug().Msg("bus stopping")
			} else {
				s.warn(err).Msg("bus recv")
			}
			return
		}

		reply, exit := s.answer(ctx, h, request)

		// The reply is attempted whatever happens, but whether to stop is
		// decided from the handler's answer and not from the fate of the
		// reply. Deciding it after the send let a lost reply discard an EXIT:
		// the agent kept serving an endpoint its owner had already given up
		// on, and the address stayed bound for the life of the process, so the
		// camera could never be rediscovered.
		sendErr := s.sock.Send(reply)

		switch {
		case sendErr == nil:
		case errors.Is(sendErr, mangos.ErrClosed):
			// Cancellation, not a failure.
			s.debug().Msg("bus stopping")
			return
		default:
			// A lost reply is one caller's problem, not the agent's, and
			// carrying on is safe: rep clears the request's backtrace before
			// it attempts the send, so the socket is ready to receive again
			// whether the send succeeded or not.
			s.warn(sendErr).Msg("bus reply")
		}

		if exit {
			s.debug().Msg("bus stopping on request")
			return
		}
	}
}

// answer parses one request and hands it to the handler. A request that does
// not even parse is refused here, so a handler never sees an empty verb.
func (s *Server) answer(ctx context.Context, h Handler, request []byte) (reply []byte, exit bool) {
	verb, arg, err := DecodeRequest(request)
	if err != nil {
		s.warn(err).Str("request", string(request)).Msg("bus request")
		return EncodeErr(err), false
	}
	return h(ctx, verb, arg)
}

func (s *Server) warn(err error) *zerolog.Event {
	return utils.Logger.Warn().Err(err).Str("agent", s.name)
}

func (s *Server) debug() *zerolog.Event {
	return utils.Logger.Trace().Str("agent", s.name)
}
