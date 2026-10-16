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

package lanagent

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jfsmig/go-bags"
	"github.com/jfsmig/onvif/v2/networking"
	"github.com/jfsmig/onvif/v2/sdk"
	"github.com/juju/errors"
	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/camctrl"
	// Renamed because every method here names its receiver lan, which would
	// hide the package.
	hostlan "github.com/ontavu/cams-agent/lan"
	"github.com/ontavu/cams-agent/lanctrl"
	"github.com/ontavu/cams-go-utils/utils"
)

// ErrNoSuchCamera is returned for a command naming a camera the agent does not
// know. Note that only its message crosses the bus, not its identity.
var ErrNoSuchCamera = errors.New("no such camera")

// CameraAgent is the runnable half of a camera. The LAN agent owns its
// lifetime: it either submits Run to the camera swarm, or closes it when a
// concurrent discovery turns out to have registered the same camera already.
// Closing matters because the agent binds its bus endpoint when it is built,
// not when it is run.
type CameraAgent interface {
	Run(ctx context.Context)
	Close() error
}

// CameraFactory instantiates the agent of a freshly discovered camera and
// returns a controller for it, together with the agent itself.
//
// The LAN agent does not know how a camera agent is built, which is what keeps
// camagent out of every package but the process entry point. It does name
// camctrl, for the contract only: a controller package is a dependency this
// agent is allowed to have, an agent package is not.
type CameraFactory func(appliance sdk.Appliance) (camctrl.CameraController, CameraAgent, error)

// cameraEntry is what the LAN agent keeps for each camera: the controller, plus
// the discovery bookkeeping that belongs to the manager rather than to the
// camera itself.
type cameraEntry struct {
	id  string
	ctl camctrl.CameraController

	// generation is the last discovery round in which the camera showed up.
	generation uint32
}

func (entry *cameraEntry) PK() string { return entry.id }

type Agent struct {
	cfg Config

	newCamera CameraFactory

	bus *agentbus.Server

	httpClient http.Client

	// Last generation number to have been used/
	generation uint32

	// How many generations can be missed before a device is forgotten
	GraceGenerations uint32

	singletonLock sync.Mutex
	dataLock      sync.Mutex

	devices    bags.SortedObj[string, *cameraEntry]
	interfaces bags.SortedObj[string, *Nic]

	// Fields extracted from the configuration
	devicesStatic              []CameraConfig
	interfacesStatic           []string
	interfacesDiscoverPatterns []string

	nicsGroup utils.Swarm
	camsSwarm utils.Swarm
}

// New builds the LAN agent and binds its bus endpoint at bindURL.
//
// Binding here rather than in Run is what lets main create a controller for the
// agent straight away: a mangos dial is synchronous and would be refused by an
// endpoint that is not bound yet.
func New(cfg Config, newCamera CameraFactory, bindURL string) (*Agent, error) {
	lan := &Agent{
		cfg:        cfg,
		newCamera:  newCamera,
		httpClient: http.Client{},

		devices:    make([]*cameraEntry, 0),
		interfaces: make([]*Nic, 0),

		interfacesDiscoverPatterns: cfg.DiscoverPatterns,
		interfacesStatic:           cfg.Interfaces,
		devicesStatic:              cfg.Cameras,

		GraceGenerations: cfg.GraceGenerations,
	}

	for _, itf := range cfg.Interfaces {
		lan.registerInterface(itf)
	}

	bus, err := agentbus.Listen("lan", bindURL)
	if err != nil {
		return nil, errors.Trace(err)
	}
	lan.bus = bus

	return lan, nil
}

// Close releases the bus endpoint of an agent that will never Run. Run closes
// it on its way out, so this is only for the construction paths that give up
// before starting; calling it twice is harmless.
func (lan *Agent) Close() error {
	return errors.Trace(lan.bus.Close())
}

// dispatch answers one request from the bus.
//
// It runs on the serving goroutine, so it must not hold dataLock across a call
// to a camera: the accessors below copy what they need and release the lock
// first, which is the same discipline the purge path follows.
func (lan *Agent) dispatch(_ context.Context, verb, arg string) (reply []byte, exit bool) {
	cmd, err := lanctrl.ParseCommand(verb)
	if err != nil {
		utils.Logger.Warn().Err(err).Str("verb", verb).Msg("lan bus command")
		return agentbus.EncodeErr(err), false
	}

	switch cmd {
	case lanctrl.CommandList:
		if arg != "" {
			return agentbus.EncodeErr(errors.NotValidf("argument %q to %s", arg, verb)), false
		}
		return agentbus.EncodeOK(lanctrl.EncodeIDs(lan.Cameras())), false

	case lanctrl.CommandPing:
		return agentbus.EncodeOK(""), false

	case lanctrl.CommandPlay, lanctrl.CommandPause:
		if arg == "" {
			return agentbus.EncodeErr(errors.NotValidf("%s without a camera", verb)), false
		}
		if err := lan.UpdateStreamExpectation(arg, cmd); err != nil {
			utils.Logger.Warn().Err(err).Str("cam", arg).Msg("lan bus command")
			return agentbus.EncodeErr(err), false
		}
		return agentbus.EncodeOK(""), false

	case lanctrl.CommandMedia:
		if arg == "" {
			return agentbus.EncodeErr(errors.NotValidf("%s without a camera", verb)), false
		}
		media, err := lan.CameraMedia(arg)
		if err != nil {
			utils.Logger.Warn().Err(err).Str("cam", arg).Msg("lan bus command")
			return agentbus.EncodeErr(err), false
		}
		return agentbus.EncodeOK(camctrl.EncodeMedia(media)), false

	default:
		// ParseCommand accepted it, so this package is missing a case.
		err := errors.NotSupportedf("command %q", string(cmd))
		utils.Logger.Warn().Err(err).Msg("lan bus command")
		return agentbus.EncodeErr(err), false
	}
}

// CameraMedia asks one camera what it was last seen producing, and relays the
// answer.
//
// The LAN agent holds no opinion about it: it knows which cameras exist and the
// camera knows what it streams, so this is a lookup and a hop. The lock is
// released before the hop, which is the same discipline UpdateStreamExpectation
// follows and the reason dispatch may call either.
func (lan *Agent) CameraMedia(camID string) (camctrl.Media, error) {
	entry := func() *cameraEntry {
		lan.dataLock.Lock()
		defer lan.dataLock.Unlock()
		entry, _ := lan.devices.Get(camID)
		return entry
	}()

	if entry == nil {
		return camctrl.Media{}, errors.Annotate(ErrNoSuchCamera, camID)
	}
	media, err := entry.ctl.Media()
	return media, errors.Annotate(err, "media")
}

// UpdateStreamExpectation forwards a command from the hub to one camera.
func (lan *Agent) UpdateStreamExpectation(camId string, cmd lanctrl.Command) error {
	// Locate the camera
	entry := func(camId string) *cameraEntry {
		lan.dataLock.Lock()
		defer lan.dataLock.Unlock()
		entry, _ := lan.devices.Get(camId)
		return entry
	}(camId)

	if entry == nil {
		return errors.Annotate(ErrNoSuchCamera, camId)
	}

	switch cmd {
	case lanctrl.CommandPlay:
		return errors.Annotate(entry.ctl.Play(), "play")
	case lanctrl.CommandPause:
		return errors.Annotate(entry.ctl.Pause(), "pause")
	default:
		return errors.NotSupportedf("command %q", string(cmd))
	}
}

func (lan *Agent) Run(ctx context.Context) {
	if !lan.singletonLock.TryLock() {
		panic("BUG: the LAN agent coroutine is a singleton")
	}
	defer lan.singletonLock.Unlock()

	utils.Logger.Info().Str("action", "start").Msg("lan")

	// Cameras may come ang go, so a simple goroutine swarm if enough.
	lan.camsSwarm = utils.NewSwarm(ctx)

	// ... This is not the case for network interfaces that are rather stable.
	lan.nicsGroup = utils.NewEnsemble(ctx)

	// An owner that exits leaves no agent of its own running, on every path out
	// of this function and not only the expected one. Cancel runs before Wait,
	// and the cameras are joined last because a NIC goroutine can still be
	// starting one while the ensemble unwinds.
	defer func() {
		lan.nicsGroup.Cancel()
		lan.nicsGroup.Wait()

		utils.Logger.Info().Str("action", "wait cams").Msg("lan")
		lan.camsSwarm.Cancel()
		lan.camsSwarm.Wait()
	}()

	// Perform a first discovery of the local interfaces.
	// No need to do it periodically, interfaces are unlikely plug & play
	if err := lan.discoverNics(); err != nil {
		utils.Logger.Error().Err(err).Msg("disc")
		return
	}

	// Spawn one goroutine per registered interface, for concurrent discoveries
	fn := func(ctx0 context.Context, gen uint32, devs []networking.ClientInfo) {
		lan.learnAllCamerasSync(ctx0, gen, devs)
	}
	for _, itf := range lan.interfaces {
		func(itf *Nic) {
			lan.nicsGroup.Run(func(c context.Context) { itf.RunRescanLoop(c, fn) })
		}(itf)
	}

	lan.nicsGroup.Run(func(c context.Context) { lan.runTimers(c) })

	// The bus joins the ensemble, so a bus failure tears the agent down the way
	// a NIC failure does, and cancellation reaches the serving loop through the
	// socket-close bridge inside agentbus.
	lan.nicsGroup.Run(func(c context.Context) { lan.bus.Serve(c, lan.dispatch) })

	utils.Logger.Info().Str("action", "wait nics").Msg("lan")

	// Wait for the discovery goroutines to stop, this will happen until a strong
	// error condition occurs. The deferred teardown above joins everything.
	lan.nicsGroup.Wait()
}

// Cameras returns the identifiers of the cameras known right now. Only the
// identifiers: that is all the upstream agent registers, and handing out the
// controllers would spread the ownership of the cameras around.
func (lan *Agent) Cameras() []string {
	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()
	out := make([]string, 0, len(lan.devices))
	for _, entry := range lan.devices {
		out = append(out, entry.id)
	}
	return out
}

// counts reports the size of the registry and of the interface list.
func (lan *Agent) counts() (devices, interfaces int) {
	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()
	return len(lan.devices), len(lan.interfaces)
}

// runTimers runs the main loop of the agent to trigger periodical actions
func (lan *Agent) runTimers(ctx context.Context) {
	nextScan := time.After(0)
	nextCheck := time.After(0)
	for {
		select {
		case <-ctx.Done():
			utils.Logger.Info().Str("action", "stop").Msg("lan")
			return
		case <-nextScan:
			lan.triggerRescanAsync(ctx)
			nextScan = time.After(lan.cfg.scanPeriod())
		case <-nextCheck:
			lan.httpClient.CloseIdleConnections()
			lan.pingCameras()
			// Counted under the lock: the discovery goroutines add to the
			// registry while this loop reads it.
			devices, interfaces := lan.counts()
			utils.Logger.Info().
				Str("action", "check").
				Int("devices", devices).
				Int("interfaces", interfaces).
				Msg("lan")
			nextCheck = time.After(lan.cfg.checkPeriod())
		}
	}
}

// pingCameras nudges every camera's state machine.
//
// It is not a health check. A camera asked to play while it is still stopping
// parks in StateResuming and waits: onCmdPlay cannot restart a stream whose
// group has not finished winding down, so onCmdPing is what finishes the
// transition afterwards. Nothing sent that ping, and the camera never came
// back -- which is what this loop is for, since it already runs on a period an
// operator can shorten with --speed.
//
// The entries are snapshotted under the lock and pinged after releasing it: a
// ping is a synchronous round trip on the bus, and holding dataLock across one
// would stall the discovery goroutines that add to the same registry.
func (lan *Agent) pingCameras() {
	lan.dataLock.Lock()
	cameras := make([]*cameraEntry, 0, len(lan.devices))
	for _, entry := range lan.devices {
		cameras = append(cameras, entry)
	}
	lan.dataLock.Unlock()

	for _, entry := range cameras {
		if err := entry.ctl.Ping(); err != nil {
			// A camera that has gone answers ErrCanceled, and one that is busy
			// answers late. A ping is idempotent and carries no instruction, so
			// there is nothing to do about either but say so: the discovery
			// loop is what removes a camera that has really left.
			utils.Logger.Debug().Err(err).
				Str("key", entry.PK()).Str("action", "ping").Msg("device")
		}
	}
}

// discoverNics does the discovery from the output of a given function.
// It helps to test the logic.
func (lan *Agent) discoverNics() error {
	itfs, err := hostlan.DiscoverSystemNics()
	if err != nil {
		return errors.Trace(err)
	}

	utils.Logger.Trace().Strs("interfaces", itfs).Msg("disc")

	for _, itf := range itfs {
		lan.maybeRegisterInterface(itf)
	}

	for _, itf := range lan.interfacesStatic {
		utils.Logger.Info().Str("itf", itf).Str("action", "force").Msg("disc")
		lan.registerInterface(itf)
	}
	return nil
}

func (lan *Agent) maybeRegisterInterface(itf string) {
	for _, pattern0 := range lan.interfacesDiscoverPatterns {
		if len(pattern0) < 2 {
			continue
		}
		pattern := pattern0
		not := pattern[0] == '!'
		if not {
			pattern = pattern[1:]
		}
		if match, err := regexp.MatchString(pattern, itf); err != nil {
			utils.Logger.Warn().Str("pattern", pattern0).Str("itf", itf).Err(err).Msg("disc")
		} else if !match {
			continue
		} else if !not {
			utils.Logger.Info().Str("pattern", pattern0).Str("itf", itf).Str("action", "add").Msg("disc")
			lan.registerInterface(itf)
		} else {
			utils.Logger.Debug().Str("pattern", pattern0).Str("itf", itf).Str("action", "skip").Msg("disc")
		}
		return
	}
}

func (lan *Agent) registerInterface(itf string) {
	lan.interfaces.Add(NewNIC(itf))
}

func (lan *Agent) learnSingleCameraSync(ctx context.Context, generation uint32, discovered networking.ClientInfo) error {
	// Preliminary check of the existence of the camera, before starting expensive queries
	lan.dataLock.Lock()
	devInPlace, already := lan.devices.Get(discovered.Uuid)
	if already && generation > devInPlace.generation {
		devInPlace.generation = generation
	}
	lan.dataLock.Unlock()
	if already {
		return nil
	}

	appliance, err := sdk.NewDevice(ctx, discovered, networking.ClientAuth{
		Username: lan.cfg.User,
		Password: lan.cfg.Password,
	}, &lan.httpClient)
	if err != nil {
		return errors.Trace(err)
	}

	// An identifier is what a LIST reply is made of, and the fields there are
	// separated by spaces. This one comes from the device, so it is checked
	// before it can make a reply ambiguous.
	camID := appliance.GetUUID()
	if camID == "" || strings.ContainsAny(camID, " \t\r\n") {
		return errors.NotValidf("camera identifier %q", camID)
	}

	// Here come the http requests
	ctl, agent, err := lan.newCamera(appliance)
	if err != nil {
		return errors.Annotate(err, "camera agent")
	}

	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()

	// If the camera is already know, it's very unlikely, but it may happen in case of a stale discovery,
	// let's just update its generation counter
	devInPlace, already = lan.devices.Get(camID)
	if already {
		if generation > devInPlace.generation {
			devInPlace.generation = generation
		} // else ... stale discovery (why not?)

		// The agent we just built is redundant. It has already bound its bus
		// endpoint, so dropping it on the floor would leak that endpoint.
		lan.discard(ctl, agent)
		return nil
	}

	entry := &cameraEntry{
		id:         camID,
		ctl:        ctl,
		generation: generation,
	}
	lan.devices.Add(entry)
	utils.Logger.Info().
		Str("key", entry.PK()).
		Str("endpoint", discovered.Xaddr).
		Uint32("gen", generation).
		Str("action", "add").
		Msg("device")

	lan.camsSwarm.Run(agent.Run)
	return nil
}

// discard releases a camera agent that was built but never started.
func (lan *Agent) discard(ctl camctrl.CameraController, agent CameraAgent) {
	if err := ctl.Close(); err != nil {
		utils.Logger.Warn().Err(err).Str("action", "discard").Msg("device")
	}
	if err := agent.Close(); err != nil {
		utils.Logger.Warn().Err(err).Str("action", "discard").Msg("device")
	}
}

func (lan *Agent) learnAllCamerasSync(ctx context.Context, gen uint32, discovered []networking.ClientInfo) {
	// Update the devices that match the
	for _, dev := range discovered {
		if err := lan.learnSingleCameraSync(ctx, gen, dev); err != nil {
			utils.Logger.Warn().Str("url", dev.Xaddr).Err(err).Msg("invalid device discovered")
		}
	}

	// The entries are removed from the registry as they are selected, so each
	// one is shut down exactly once even when several discovery goroutines run
	// a round at the same time.
	toBePurged := lan.takeCamerasToPurge(gen)
	if len(toBePurged) > 0 {
		utils.Logger.Info().Str("action", "purge").Interface("count", len(toBePurged)).Msg("lan")
	}

	for _, entry := range toBePurged {
		// Outside the lock: both calls talk to the camera agent over the bus and
		// may wait for it. Exit stops the agent for good, which is what a purge
		// means; its Run then returns and leaves the camera swarm.
		if err := entry.ctl.Exit(); err != nil {
			utils.Logger.Warn().Err(err).Str("key", entry.PK()).Str("action", "purge").Msg("device")
		}
		if err := entry.ctl.Close(); err != nil {
			utils.Logger.Warn().Err(err).Str("key", entry.PK()).Str("action", "purge").Msg("device")
		}
	}
}

// takeCamerasToPurge removes the cameras that have been missing for too long
// and hands them to the caller, which owns them from then on.
//
// Selecting and removing happen under one acquisition of the lock on purpose.
// They used to be two, and two discovery goroutines could therefore claim the
// same camera: the second Remove was a silent no-op, and the second Exit and
// Close ran on a controller the first had already shut, with a window as wide
// as a bus round trip.
func (lan *Agent) takeCamerasToPurge(gen uint32) []*cameraEntry {
	out := make([]*cameraEntry, 0)

	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()

	// Zero is documented as disabling the purge, and it is the production
	// default, so it is checked before anything else rather than left to fall
	// out of the comparison below.
	if lan.GraceGenerations == 0 {
		return out
	}

	// Forget the devices that have been missing for longer than the grace: a
	// camera seen in this very round has a delta of zero and must be kept.
	//
	// The comparison used to be "<", which selected the cameras seen most
	// recently -- so setting the grace to anything non-zero destroyed every
	// healthy camera on the round that discovered it, and never collected one
	// that had actually gone.
	//
	// Walking backwards matters now that the removal happens here: it keeps the
	// indices of the entries still to be examined valid.
	for i := len(lan.devices); i > 0; i-- {
		dev := lan.devices[i-1]
		if delta(gen, dev.generation) > lan.GraceGenerations {
			out = append(out, dev)
			lan.devices.Remove(dev.PK())
		}
	}

	return out
}

type Unsigned interface {
	uint | uint32 | uint16 | uint8
}

// delta is the number of generations from lo up to hi.
//
// Unsigned subtraction wraps in Go, which is exactly the arithmetic a circular
// counter wants, so no special case is needed for the wraparound. The previous
// version added 2^n-1 on that branch, which is the same as subtracting one, and
// under-reported every age by a generation once the counter had wrapped.
func delta[T Unsigned](hi, lo T) T {
	return hi - lo
}

func (lan *Agent) triggerRescanAsync(ctx context.Context) {
	gen := atomic.AddUint32(&lan.generation, 1)
	for _, itf := range lan.interfaces {
		itf.TriggerRescanAsync(ctx, gen)
	}
}

func (lan *Agent) PK() string { return "lan" }
