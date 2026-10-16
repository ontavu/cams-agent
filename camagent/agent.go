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

// Package camagent is the agent in charge of one camera: it discovers the
// media stream over ONVIF, negotiates it over RTSP, and relays the raw RTP and
// RTCP packets to an upstream.
//
// It is driven only through the message bus, by a camctrl.Client. Nothing but
// the process entry point should import this package: everything else holds a
// controller instead.
package camagent

import (
	"context"
	"sync"

	"github.com/juju/errors"
	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-go-utils/utils"
	"github.com/rs/zerolog"
)

// Agent serves one camera. We assume only one stream per camera.
type Agent struct {
	// mediaURL is the media bus endpoint of the upstream this camera pushes to.
	// The agent knows nothing else about it: what happens to a frame after it
	// is pushed is the upstream's business.
	mediaURL string

	id          string
	onvifClient Appliance

	// user and password go into the stream URL, which is where gortsplib reads
	// them from. They cannot be had from the appliance: the SDK does not
	// expose the credentials it was built with, and FetchStreamURI -- which
	// used to inject them itself -- is what chooseProfile replaces.
	user     string
	password string

	bus *agentbus.Server

	singletonLock sync.Mutex

	// state is owned by the Run goroutine, which is the only one to touch it.
	// Reaching it from the outside is what the STATE request is for.
	state camctrl.State

	// media is what the camera was last seen producing. Written by the stream
	// goroutine when it picks a profile and read by the bus handler answering
	// MEDIA, which is why it is guarded rather than owned like state.
	//
	// Never cleared once observed: a camera's encoding does not change while it
	// runs, so a pause should not throw away what is known about it. The zero
	// value means it has never streamed.
	mediaLock sync.Mutex
	media     camctrl.Media

	// group supervises the media subtree. It is nil whenever no media is
	// running, which includes every camera the hub never asked to play.
	group utils.Swarm

	flagRetry bool
}

// Option tunes an Agent at construction.
type Option func(*Agent)

// NoRetry stops the agent from restarting its media stream after a failure, so
// that one attempt is made and reported. Meant for one-shot callers.
func NoRetry() Option {
	return func(cam *Agent) { cam.flagRetry = false }
}

// WithCredentials supplies the ONVIF credentials, which are also the RTSP ones.
//
// Without them the stream URL goes out as the camera published it, which works
// only on a camera that wants no authentication.
func WithCredentials(user, password string) Option {
	return func(cam *Agent) {
		cam.user = user
		cam.password = password
	}
}

// New builds the agent and binds its bus endpoint at bindURL.
//
// Binding happens here rather than in Run because a controller dials
// synchronously: were the socket bound only once Run got scheduled, a
// controller built right after this call would be refused. It also means a
// duplicate camera identifier is reported now, as mangos.ErrAddrInUse, instead
// of silently sharing an endpoint.
func New(appliance Appliance, bindURL, mediaURL string, opts ...Option) (*Agent, error) {
	cam := &Agent{
		mediaURL:    mediaURL,
		id:          appliance.GetUUID(),
		onvifClient: appliance,
		state:       camctrl.StateOff,
		flagRetry:   true,
	}
	for _, opt := range opts {
		opt(cam)
	}

	bus, err := agentbus.Listen("cam "+cam.id, bindURL)
	if err != nil {
		return nil, errors.Trace(err)
	}
	cam.bus = bus

	return cam, nil
}

// ID returns the identifier of the camera served by this agent.
func (cam *Agent) ID() string { return cam.id }

// Close releases the bus endpoint of an agent that will never Run. Run closes
// it on its way out, so this is only for the construction paths that give the
// agent up before starting it; calling it twice is harmless.
func (cam *Agent) Close() error {
	return errors.Trace(cam.bus.Close())
}

// Run serves the bus until the context is cancelled or an EXIT is received. It
// is a singleton: one agent, one Run.
func (cam *Agent) Run(ctx context.Context) {
	if !cam.singletonLock.TryLock() {
		panic("BUG singleton only")
	}
	defer cam.singletonLock.Unlock()

	if cam.state != camctrl.StateOff {
		panic("BUG: unexpected camera agent state")
	}
	cam.state = camctrl.StateIdle

	defer func() {
		cam.stopGroup()
		cam.state = camctrl.StateOff
	}()

	// Serve calls the handler one request at a time, from this goroutine, so
	// the state machine below stays the sole owner of the state and needs no
	// lock of its own.
	cam.bus.Serve(ctx, cam.dispatch)
}

// dispatch turns one request into its reply, and says whether the agent has
// been asked to stop. The reply is always produced: an unknown verb is refused
// rather than ignored, so a controller of another revision learns about it.
//
// A camera takes no arguments, so one that carries any is refused rather than
// silently accepted.
func (cam *Agent) dispatch(ctx context.Context, verb, arg string) (reply []byte, exit bool) {
	cmd, err := camctrl.ParseCommand(verb)
	if err != nil {
		cam.warn(err).Str("verb", verb).Msg("cam bus command")
		return agentbus.EncodeErr(err), false
	}
	if arg != "" {
		err := errors.NotValidf("argument %q to %s", arg, verb)
		cam.warn(err).Msg("cam bus command")
		return agentbus.EncodeErr(err), false
	}

	switch cmd {
	case camctrl.CommandPlay:
		cam.onCmdPlay(ctx)
	case camctrl.CommandPause:
		cam.onCmdStop()
	case camctrl.CommandPing:
		cam.onCmdPing(ctx)
	case camctrl.CommandState:
		return agentbus.EncodeOK(string(cam.state)), false
	case camctrl.CommandMedia:
		return agentbus.EncodeOK(camctrl.EncodeMedia(cam.observedMedia())), false
	case camctrl.CommandExit:
		// Reply first, stop after: the caller deserves an answer.
		return agentbus.EncodeOK(""), true
	default:
		// ParseCommand accepted it, so this package is missing a case.
		err := errors.NotSupportedf("command %q", string(cmd))
		cam.warn(err).Msg("cam bus command")
		return agentbus.EncodeErr(err), false
	}

	return agentbus.EncodeOK(""), false
}

// observedMedia reports what the camera was last seen producing.
func (cam *Agent) observedMedia() camctrl.Media {
	cam.mediaLock.Lock()
	defer cam.mediaLock.Unlock()
	return cam.media
}

// noteMedia records what a stream attempt settled on.
func (cam *Agent) noteMedia(m camctrl.Media) {
	cam.mediaLock.Lock()
	defer cam.mediaLock.Unlock()
	cam.media = m
}

// groupBusy reports whether media goroutines are still running. A nil group
// means none was ever started, which counts as idle.
func (cam *Agent) groupBusy() bool {
	return cam.group != nil && cam.group.Count() > 0
}

// cancelGroup asks the media subtree to stop, without waiting for it.
func (cam *Agent) cancelGroup() {
	if cam.group != nil {
		cam.group.Cancel()
	}
}

// stopGroup tears the media subtree down and waits for it. Whoever cancels also
// waits, and a nil group is the ordinary case for a camera that never played.
func (cam *Agent) stopGroup() {
	if cam.group == nil {
		return
	}
	cam.group.Cancel()
	cam.group.Wait()
	cam.group = nil
}

func (cam *Agent) warn(err error) *zerolog.Event {
	return utils.Logger.Warn().Str("url", cam.id).Err(err)
}

func (cam *Agent) debug() *zerolog.Event {
	return utils.Logger.Trace().Str("url", cam.id)
}

func (cam *Agent) onCmdPlay(ctx context.Context) {
	switch cam.state {
	case camctrl.StateOff:
		panic("BUG unexpected state")
	case camctrl.StatePausing, camctrl.StateResuming:
		cam.state = camctrl.StateResuming
		if cam.groupBusy() {
			return
		}
		cam.state = camctrl.StateIdle
		fallthrough
	case camctrl.StateIdle:
		cam.state = camctrl.StatePlaying
		cam.group = utils.NewEnsemble(ctx)
		cam.group.Run(func(c context.Context) { cam.runStream(c) })
		cam.debug().Msg("camera restarted")
		fallthrough
	case camctrl.StatePlaying:
		// No-Op
	default:
		panic("BUG invalid state")
	}
}

func (cam *Agent) onCmdStop() {
	switch cam.state {
	case camctrl.StateOff:
		panic("BUG unexpected state")
	case camctrl.StatePlaying, camctrl.StateResuming:
		// Trigger a stop of the coroutines
		cam.cancelGroup()
		cam.state = camctrl.StatePausing
		fallthrough
	case camctrl.StatePausing:
		// Wait for the stop to finish
		if cam.groupBusy() {
			return
		}
		cam.group = nil
		cam.state = camctrl.StateIdle
		fallthrough
	case camctrl.StateIdle:
		// No-Op
	default:
		panic("BUG invalid state")
	}
}

func (cam *Agent) onCmdPing(ctx context.Context) {
	switch cam.state {
	case camctrl.StateOff:
		panic("BUG unexpected state")
	case camctrl.StateIdle, camctrl.StatePlaying:
		// No-Op
	case camctrl.StatePausing:
		if cam.groupBusy() {
			return
		}
		cam.group = nil
		cam.state = camctrl.StateIdle
	case camctrl.StateResuming:
		if cam.groupBusy() {
			return
		}
		cam.state = camctrl.StateIdle
		cam.onCmdPlay(ctx)
	default:
		panic("BUG invalid state")
	}
}
