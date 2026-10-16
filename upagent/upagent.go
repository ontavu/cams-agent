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

// Package upagent is the agent in charge of the link to the hub: it registers
// the local streams with the hub's control plane and turns the orders that come
// back into commands for the LAN.
//
// It depends on no other agent. What it needs from the LAN is declared here, as
// an interface, so that nothing of the LAN agent is linked in and the whole
// thing can be tested without a socket. Nothing but the process entry point
// should import this package: everything else holds a upctrl.Client.
package upagent

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juju/errors"
	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/lanctrl"
	"github.com/ontavu/cams-agent/upctrl"
	"github.com/ontavu/cams-api/pb"
	"github.com/ontavu/cams-api/wire"
	"github.com/ontavu/cams-go-utils/utils"
	"github.com/ontavu/go-swarm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// controlStream is the receiving half of the hub's command stream.
// pb.Controller_ControlClient satisfies it; narrowing it here is what makes the
// dispatch loop testable without a gRPC server.
type controlStream interface {
	Recv() (*pb.DownstreamControlRequest, error)
}

// registrar is the hub's registration RPC. pb.RegistrarClient satisfies it as
// it stands, being a single-method interface already.
type registrar interface {
	Register(ctx context.Context, in *pb.RegisterRequest, opts ...grpc.CallOption) (*pb.None, error)
}

// Agent keeps the connection to the hub's control plane.
type Agent struct {
	cfg Config
	lan lanctrl.LanController

	bus *agentbus.Server

	singletonLock sync.Mutex

	// up is the link state. Unlike the camera agent, whose state belongs to its
	// single serving goroutine, this is written by the reconnect loop and read
	// by the bus handler, so it is atomic rather than plain.
	up atomic.Bool
}

// New builds the agent and binds its bus endpoint at bindURL.
//
// Binding here rather than in Run lets a controller be created straight away: a
// mangos dial is synchronous and an unbound endpoint refuses it.
func New(cfg Config, lan lanctrl.LanController, bindURL string) (*Agent, error) {
	us := &Agent{cfg: cfg, lan: lan}

	bus, err := agentbus.Listen("upstream", bindURL)
	if err != nil {
		return nil, errors.Trace(err)
	}
	us.bus = bus

	return us, nil
}

// Close releases the bus endpoint of an agent that will never Run. Run closes
// it on its way out, so this is only for the construction paths that give up
// before starting; calling it twice is harmless.
func (us *Agent) Close() error {
	return errors.Trace(us.bus.Close())
}

// State reports whether the link to the hub is up.
func (us *Agent) State() upctrl.State {
	if us.up.Load() {
		return upctrl.StateUp
	}
	return upctrl.StateDown
}

// Run keeps the link to the hub until the context is cancelled. It is a
// singleton: one agent, one Run.
func (us *Agent) Run(ctx context.Context) {
	if !us.singletonLock.TryLock() {
		panic("BUG the upstream agent is already running")
	}
	defer us.singletonLock.Unlock()

	utils.Logger.Debug().Str("action", "start").Msg("up")

	// The bus is served beside the connection attempts, so that STATE is
	// answerable while the link is down -- which is when it is worth asking.
	swarm.EnsembleRun(ctx,
		func(c context.Context) { us.bus.Serve(c, us.dispatch) },
		us.runConnections,
	)
}

// runConnections retries the hub for as long as it is asked to.
func (us *Agent) runConnections(ctx context.Context) {
	first := true
	for ctx.Err() == nil {
		if !first {
			// Pause between attempts, not before the first one: there is no
			// reason to start a second late.
			select {
			case <-ctx.Done():
				return
			case <-time.After(us.cfg.retryPeriod()):
			}
		}
		first = false

		us.connectAndServe(ctx)
	}
}

// dispatch answers one request from the bus.
func (us *Agent) dispatch(_ context.Context, verb, arg string) (reply []byte, exit bool) {
	cmd, err := upctrl.ParseCommand(verb)
	if err != nil {
		utils.Logger.Warn().Err(err).Str("verb", verb).Msg("up bus command")
		return agentbus.EncodeErr(err), false
	}
	if arg != "" {
		return agentbus.EncodeErr(errors.NotValidf("argument %q to %s", arg, verb)), false
	}

	switch cmd {
	case upctrl.CommandPing:
		return agentbus.EncodeOK(""), false
	case upctrl.CommandState:
		return agentbus.EncodeOK(string(us.State())), false
	default:
		// ParseCommand accepted it, so this package is missing a case.
		err := errors.NotSupportedf("command %q", string(cmd))
		utils.Logger.Warn().Err(err).Msg("up bus command")
		return agentbus.EncodeErr(err), false
	}
}

// connectAndServe holds one connection to the hub for as long as it lasts.
func (us *Agent) connectAndServe(ctx context.Context) {
	utils.Logger.Trace().
		Str("action", "restart").
		Str("endpoint", us.cfg.ControlAddress).
		Msg("up")

	cnx, err := utils.DialInsecure(ctx, us.cfg.ControlAddress, wire.DialOptions()...)
	if err != nil {
		utils.Logger.Error().Err(err).Str("action", "dial").Msg("up")
		return
	}
	defer func() {
		if cerr := cnx.Close(); cerr != nil {
			utils.Logger.Warn().Err(cerr).Str("action", "close").Msg("up")
		}
	}()

	// The stream is built from a context this function owns and every member
	// below cancels, because cancelling a gRPC client stream is the only thing
	// that releases a reader parked in Recv.
	//
	// It used to be built from the outer context while the members ran under
	// the ensemble's child. A registrar failure then cancelled the child, which
	// could not reach the stream, so the reader stayed parked on a healthy
	// connection: this function never returned, the connection was never
	// closed, no reconnect was ever attempted, and STATE went on answering UP
	// for the lifetime of the process.
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()

	stream, err := us.openControl(streamCtx, pb.NewControllerClient(cnx))
	if err != nil {
		utils.Logger.Warn().Err(err).Msg("upstream control open")
		return
	}

	// The hub has accepted the stream. Note that gRPC dials and opens streams
	// lazily, so nothing has round-tripped yet: until the first Recv or the
	// first registration, this is optimism rather than evidence.
	us.up.Store(true)
	defer us.up.Store(false)

	// Two independent goroutines, and no channel between them. The command
	// reader used to hand its work to the registrar's goroutine through an
	// unbuffered channel, which only meant that a slow command stalled the
	// registrations as well.
	swarm.EnsembleRun(ctx,
		func(c context.Context) {
			defer stopStream()
			if err := us.readControl(c, stream); err != nil {
				utils.Logger.Warn().Err(err).Msg("upstream control error")
			}
		},
		func(c context.Context) {
			defer stopStream()
			if err := us.runRegistrar(c, pb.NewRegistrarClient(cnx)); err != nil {
				utils.Logger.Warn().Err(err).Msg("upstream error")
			}
		})
}

// openControl asks the hub for the stream of commands.
func (us *Agent) openControl(ctx context.Context, client pb.ControllerClient) (controlStream, error) {
	ctx = wire.WithSession(
		metadata.AppendToOutgoingContext(ctx, wire.KeyUser, us.cfg.User))

	stream, err := client.Control(ctx)
	if err != nil {
		return nil, errors.Annotate(err, "control open")
	}
	return stream, nil
}

// readControl turns the hub's orders into commands for the LAN.
//
// Only a failure of the stream itself ends the connection. A camera that
// refuses is logged and skipped: one unhealthy camera has no business costing
// every other camera its control channel.
//
// The context is deliberately unused. Recv is the only blocking call here and
// no context check can interrupt it; cancellation arrives instead through the
// context the stream was opened with, which surfaces as an error from Recv.
func (us *Agent) readControl(_ context.Context, stream controlStream) error {
	utils.Logger.Trace().Str("action", "start").Msg("up ctrl")

	for {
		request, err := stream.Recv()
		if err != nil {
			return errors.Annotate(err, "control recv")
		}

		if err := us.onCommand(request); err != nil {
			utils.Logger.Warn().
				Err(err).
				Str("cam", request.StreamID).
				Msg("upstream command refused")
		}
	}
}

// onCommand carries out one order from the hub.
func (us *Agent) onCommand(request *pb.DownstreamControlRequest) error {
	camID := request.StreamID

	switch request.Command {
	case pb.DownstreamCommandType_DOWNSTREAM_COMMAND_TYPE_PLAY:
		return errors.Annotate(us.lan.Play(camID), "play")
	case pb.DownstreamCommandType_DOWNSTREAM_COMMAND_TYPE_STOP:
		return errors.Annotate(us.lan.Pause(camID), "pause")
	default:
		return errors.NotSupportedf("hub command %v", request.Command)
	}
}

// runRegistrar tells the hub about the local streams, over and over.
func (us *Agent) runRegistrar(ctx context.Context, client registrar) error {
	utils.Logger.Trace().Str("action", "start").Msg("up")

	next := time.After(0)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-next:
			next = time.After(us.cfg.registerPeriod())
			if err := us.registerAll(ctx, client); err != nil {
				return errors.Trace(err)
			}
		}
	}
}

// encodingToPB maps what a camera reports onto the wire enum.
//
// An encoding this revision does not know becomes UNSPECIFIED rather than
// failing the registration: the hub reads that as "not known yet", which is
// what it is.
func encodingToPB(enc camctrl.Encoding) pb.VideoEncoding {
	switch enc {
	case camctrl.EncodingH264:
		return pb.VideoEncoding_VIDEO_ENCODING_H264
	case camctrl.EncodingH265:
		return pb.VideoEncoding_VIDEO_ENCODING_H265
	case camctrl.EncodingJPEG:
		return pb.VideoEncoding_VIDEO_ENCODING_JPEG
	default:
		return pb.VideoEncoding_VIDEO_ENCODING_UNSPECIFIED
	}
}

// registerAll registers every camera the LAN agent knows about.
//
// A LAN that cannot be asked costs this round and nothing more: the answer is
// expected to be there again by the next one, and dropping the hub connection
// over it would help nobody. A hub that refuses a registration is another
// matter, and ends the connection so that it is dialled afresh.
func (us *Agent) registerAll(ctx context.Context, client registrar) error {
	camIDs, err := us.lan.List()
	if err != nil {
		utils.Logger.Warn().Err(err).Msg("upstream registration skipped")
		return nil
	}

	// WithSession wraps the block rather than preceding it: NewOutgoingContext
	// replaces whatever metadata the context already carried.
	ctx = wire.WithSession(metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{
		wire.KeyUser: us.cfg.User,
	})))

	for _, camID := range camIDs {
		inReq := pb.RegisterRequest{
			Id: &pb.StreamId{
				User:   us.cfg.User,
				Stream: camID,
			},
		}

		// What the camera produces, when that is known. A camera nobody has
		// asked to play has never described a stream, and a LAN agent of an
		// older revision does not understand the question; both leave the
		// fields unset, which the hub reads as "not known yet" and never as
		// "no video". Neither is a reason to skip the registration, which is
		// the part the hub cannot do without.
		if media, err := us.lan.Media(camID); err != nil {
			utils.Logger.Debug().Err(err).Str("cam", camID).Msg("camera media unknown")
		} else {
			inReq.Encoding = encodingToPB(media.Encoding)
			inReq.Width = uint32(media.Width)
			inReq.Height = uint32(media.Height)
			inReq.GopLength = uint32(media.GopLength)
		}

		// One deadline per call. Without it a hub that accepts the connection
		// and then answers nothing parks this loop for good, and since the
		// reader is parked on its own stream at the same time, the whole
		// connection never returns and the agent reports its link as up
		// forever. The configured timeout used to be dropped on the way in.
		if err := func() error {
			callCtx, cancel := context.WithTimeout(ctx, us.cfg.requestTimeout())
			defer cancel()
			_, err := client.Register(callCtx, &inReq)
			return err
		}(); err != nil {
			return errors.Annotate(err, "register")
		}
	}
	return nil
}
