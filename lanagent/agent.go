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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jfsmig/go-bags"
	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/errkind"
	"github.com/ontavu/onvif/v2/device"
	"github.com/ontavu/onvif/v2/networking"
	"github.com/ontavu/onvif/v2/sdk"

	// Renamed because every method here names its receiver lan, which would
	// hide the package.
	hostlan "github.com/ontavu/cams-agent/lan"
	"github.com/ontavu/cams-agent/lanctrl"
	"github.com/ontavu/cams-go-utils/utils"
	"github.com/ontavu/go-swarm"
)

// ErrNoSuchCamera is returned for a command naming a camera the agent does not
// know. Note that only its message crosses the bus, not its identity.
var ErrNoSuchCamera = errors.New("no such camera")

// deviceContactTimeout bounds the ONVIF exchange with one device, discovered or
// configured. Each round learns its devices one after the other, and the HTTP
// client has no timeout of its own, so a device -- or any host answering a
// probe with an address that accepts the connection and never replies -- would
// otherwise hold that loop, and every device after it, for as long as the
// agent runs.
const deviceContactTimeout = 15 * time.Second

// CameraAgent is the runnable half of a camera. The LAN agent owns its
// lifetime: it either submits Run to the camera swarm, or closes it when a
// concurrent discovery turns out to have registered the same camera already.
// Closing matters because the agent binds its bus endpoint when it is built,
// not when it is run.
type CameraAgent interface {
	Run(ctx context.Context)
	Close() error
}

// CameraFactory instantiates the agent of a freshly learned camera and returns a
// controller for it, together with the agent itself. auth is what the device was
// reached with, and what its media session has to be opened with: a camera the
// configuration names may have credentials of its own.
//
// The LAN agent does not know how a camera agent is built, which is what keeps
// camagent out of every package but the process entry point. It does name
// camctrl, for the contract only: a controller package is a dependency this
// agent is allowed to have, an agent package is not.
type CameraFactory func(appliance sdk.Appliance, auth networking.ClientAuth) (camctrl.CameraController, CameraAgent, error)

// deviceOpener contacts a device and settles its identifier. Agent.openDevice
// is the one in use; the tests put a camera of their own in its place.
type deviceOpener func(ctx context.Context, info networking.ClientInfo, auth networking.ClientAuth) (sdk.Appliance, error)

// cameraEntry is what the LAN agent keeps for each camera: the controller, plus
// the discovery bookkeeping that belongs to the manager rather than to the
// camera itself.
type cameraEntry struct {
	id  string
	ctl camctrl.CameraController

	// generation is the last discovery round in which the camera showed up.
	generation uint32

	// xaddr is where the device was reached, in canonicalXaddr's form.
	xaddr string

	// static marks a camera the configuration names. It is never purged: it is
	// there because somebody asked for it, not because it answered a probe.
	static bool
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
	interfacesDiscoverPatterns []interfacePattern

	// staticAddresses are the addresses of devicesStatic, in canonicalXaddr's
	// form: the ones discovery leaves to the static loop. Read-only after New.
	staticAddresses map[string]bool

	open deviceOpener

	// contactTimeout is deviceContactTimeout, a field so that a test need not
	// wait that long for a device that never answers.
	contactTimeout time.Duration

	// staticTrigger asks runStaticLoop for a round, the way Nic.trigger does
	// for an interface.
	staticTrigger chan uint32

	nicsGroup swarm.Swarm
	camsSwarm swarm.Swarm
}

// New builds the LAN agent and binds its bus endpoint at bindURL.
//
// Binding here rather than in Run is what lets main create a controller for the
// agent straight away: a mangos dial is synchronous and would be refused by an
// endpoint that is not bound yet.
//
// A discover pattern or a configured camera that cannot work is refused here,
// before anything is bound, so that a mistake in the configuration stops the
// agent at start-up instead of leaving it to run without the camera.
func New(cfg Config, newCamera CameraFactory, bindURL string) (*Agent, error) {
	patterns, err := compilePatterns(cfg.DiscoverPatterns)
	if err != nil {
		return nil, err
	}
	staticAddresses, err := validateStaticCameras(cfg.Cameras)
	if err != nil {
		return nil, err
	}

	lan := &Agent{
		cfg:        cfg,
		newCamera:  newCamera,
		httpClient: http.Client{},

		devices:    make([]*cameraEntry, 0),
		interfaces: make([]*Nic, 0),

		interfacesDiscoverPatterns: patterns,
		interfacesStatic:           cfg.Interfaces,
		devicesStatic:              cfg.Cameras,
		staticAddresses:            staticAddresses,

		staticTrigger:  make(chan uint32, rescanBacklog),
		contactTimeout: deviceContactTimeout,

		GraceGenerations: cfg.GraceGenerations,
	}
	lan.open = lan.openDevice

	for _, itf := range cfg.Interfaces {
		lan.registerInterface(itf)
	}

	bus, err := agentbus.Listen("lan", bindURL)
	if err != nil {
		return nil, err
	}
	lan.bus = bus

	return lan, nil
}

// Close releases the bus endpoint of an agent that will never Run. Run closes
// it on its way out, so this is only for the construction paths that give up
// before starting; calling it twice is harmless.
func (lan *Agent) Close() error {
	return lan.bus.Close()
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
			return agentbus.EncodeErr(fmt.Errorf("argument %q to %s %w", arg, verb, errkind.ErrNotValid)), false
		}
		return agentbus.EncodeOK(lanctrl.EncodeIDs(lan.Cameras())), false

	case lanctrl.CommandPing:
		return agentbus.EncodeOK(""), false

	case lanctrl.CommandPlay, lanctrl.CommandPause:
		if arg == "" {
			return agentbus.EncodeErr(fmt.Errorf("%s without a camera %w", verb, errkind.ErrNotValid)), false
		}
		if err := lan.UpdateStreamExpectation(arg, cmd); err != nil {
			utils.Logger.Warn().Err(err).Str("cam", arg).Msg("lan bus command")
			return agentbus.EncodeErr(err), false
		}
		return agentbus.EncodeOK(""), false

	case lanctrl.CommandMedia:
		if arg == "" {
			return agentbus.EncodeErr(fmt.Errorf("%s without a camera %w", verb, errkind.ErrNotValid)), false
		}
		media, err := lan.CameraMedia(arg)
		if err != nil {
			utils.Logger.Warn().Err(err).Str("cam", arg).Msg("lan bus command")
			return agentbus.EncodeErr(err), false
		}
		return agentbus.EncodeOK(camctrl.EncodeMedia(media)), false

	default:
		// ParseCommand accepted it, so this package is missing a case.
		err := fmt.Errorf("command %q %w", string(cmd), errkind.ErrNotSupported)
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
		return camctrl.Media{}, fmt.Errorf("%s: %w", camID, ErrNoSuchCamera)
	}
	media, err := entry.ctl.Media()
	if err != nil {
		return media, fmt.Errorf("media: %w", err)
	}
	return media, nil
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
		return fmt.Errorf("%s: %w", camId, ErrNoSuchCamera)
	}

	switch cmd {
	case lanctrl.CommandPlay:
		if err := entry.ctl.Play(); err != nil {
			return fmt.Errorf("play: %w", err)
		}
	case lanctrl.CommandPause:
		if err := entry.ctl.Pause(); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
	default:
		return fmt.Errorf("command %q %w", string(cmd), errkind.ErrNotSupported)
	}
	return nil
}

func (lan *Agent) Run(ctx context.Context) {
	if !lan.singletonLock.TryLock() {
		panic("BUG: the LAN agent coroutine is a singleton")
	}
	defer lan.singletonLock.Unlock()

	utils.Logger.Info().Str("action", "start").Msg("lan")

	// Cameras may come ang go, so a simple goroutine swarm if enough.
	lan.camsSwarm = swarm.New(ctx)

	// ... This is not the case for network interfaces that are rather stable.
	lan.nicsGroup = swarm.NewEnsemble(ctx)

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

	// The configured cameras do not depend on any interface being selected, so
	// they have a loop of their own.
	if len(lan.devicesStatic) > 0 {
		lan.nicsGroup.Run(func(c context.Context) { lan.runStaticLoop(c) })
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
		return err
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
	selected, decidedBy := selectInterface(lan.interfacesDiscoverPatterns, itf)
	switch {
	case selected:
		utils.Logger.Info().Str("pattern", decidedBy).Str("itf", itf).Str("action", "add").Msg("disc")
		lan.registerInterface(itf)
	case decidedBy != "":
		utils.Logger.Debug().Str("pattern", decidedBy).Str("itf", itf).Str("action", "skip").Msg("disc")
	default:
		utils.Logger.Debug().Str("itf", itf).Str("action", "unmatched").Msg("disc")
	}
}

// registerInterface adds an interface to probe, once. The registry keeps
// duplicates, and an interface reached twice -- forced by the configuration and
// selected by a pattern, or forced and then forced again by discoverNics --
// would otherwise run two rescan loops probing the same link.
func (lan *Agent) registerInterface(itf string) {
	if lan.interfaces.Has(itf) {
		return
	}
	lan.interfaces.Add(NewNIC(itf))
}

// openDevice contacts a device and settles its identifier.
//
// A discovered device arrives with the endpoint reference WS-Discovery
// reported. A configured one arrives with the uuid of its configuration, often
// none, and is then asked for its endpoint reference: ONVIF answers it with the
// address of the device service's endpoint reference, which is the identifier
// discovery advertises, so a camera reached both ways is registered once and
// under one name. Not every device implements GetEndpointReference; one that
// does not needs its uuid in the configuration.
func (lan *Agent) openDevice(ctx context.Context, info networking.ClientInfo, auth networking.ClientAuth) (sdk.Appliance, error) {
	client, err := networking.NewClient(info, &lan.httpClient)
	if err != nil {
		return nil, fmt.Errorf("onvif client: %w", err)
	}

	// Wrapped first: it is the liveness probe, and it measures the device's
	// clock, which the credentials of every later call are stamped against.
	appliance, err := sdk.WrapClient(ctx, client, auth)
	if err != nil {
		return nil, fmt.Errorf("onvif device: %w", err)
	}

	if client.GetUUID() == "" {
		ref, err := device.Call_GetEndpointReference(ctx, client, device.GetEndpointReference{})
		if err != nil {
			return nil, fmt.Errorf("no identifier, and GetEndpointReference failed: %w", err)
		}
		client.SetUUID(strings.TrimSpace(ref.GUID))
	}
	return appliance, nil
}

// findLocked returns the entry a device is already known under, or nil. The
// caller holds dataLock.
//
// By identifier, and for a configured camera by address too, since one of
// those is known before its identifier is: it was learned at that address, by
// the static loop and nothing else, because discovery leaves the configured
// addresses alone. Between discovered cameras the address proves nothing: one
// of them may have taken an address another has left.
func (lan *Agent) findLocked(id, xaddr string, static bool) *cameraEntry {
	if id != "" {
		if entry, ok := lan.devices.Get(id); ok {
			return entry
		}
	}
	if static {
		for _, entry := range lan.devices {
			if entry.static && entry.xaddr == xaddr {
				return entry
			}
		}
	}
	return nil
}

// refreshLocked records that a known camera showed up again. The caller holds
// dataLock.
func refreshLocked(entry *cameraEntry, xaddr string, generation uint32, static bool) {
	if generation > entry.generation {
		entry.generation = generation
	} // else ... stale discovery (why not?)

	if static && entry.xaddr != xaddr {
		// A configured camera that answered with the identifier of a camera
		// known elsewhere: a clone shipped with the same uuid, one device on
		// two addresses, or a host that claimed the identifier first. It is not
		// learned, since one identifier names one camera; the entry already
		// there is left as it was, and the operator is the one who can tell
		// which case this is.
		utils.Logger.Warn().
			Str("key", entry.id).
			Str("address", xaddr).
			Str("known_at", entry.xaddr).
			Msg("configured camera reports the identifier of another; set its uuid")
	}
}

// refreshKnown is findLocked and refreshLocked under one acquisition of the
// lock, and reports whether the camera was known.
func (lan *Agent) refreshKnown(id, xaddr string, generation uint32, static bool) bool {
	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()
	entry := lan.findLocked(id, xaddr, static)
	if entry != nil {
		refreshLocked(entry, xaddr, generation, static)
	}
	return entry != nil
}

// learnSingleCameraSync makes one device a camera of the agent, unless it is
// one already. static says it is named by the configuration rather than
// discovered, and auth is what it is reached with.
func (lan *Agent) learnSingleCameraSync(ctx context.Context, generation uint32, discovered networking.ClientInfo, auth networking.ClientAuth, static bool) error {
	xaddr := canonicalXaddr(discovered.Xaddr)

	// Preliminary check of the existence of the camera, before starting expensive queries
	if lan.refreshKnown(discovered.Uuid, xaddr, generation, static) {
		return nil
	}

	appliance, err := lan.open(ctx, discovered, auth)
	if err != nil {
		return err
	}

	// An identifier is what a LIST reply is made of, and the fields there are
	// separated by spaces. This one comes from the device, so it is checked
	// before it can make a reply ambiguous.
	camID := appliance.GetUUID()
	if camID == "" || strings.ContainsAny(camID, " \t\r\n") {
		return fmt.Errorf("camera identifier %q %w", camID, errkind.ErrNotValid)
	}

	// Checked again under the identifier the device gave, which for a
	// configured camera is the first one there is. Building an agent for a
	// camera already known would bind an endpoint only to release it.
	if lan.refreshKnown(camID, xaddr, generation, static) {
		return nil
	}

	// Here come the http requests
	ctl, agent, err := lan.newCamera(appliance, auth)
	if err != nil {
		return fmt.Errorf("camera agent: %w", err)
	}

	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()

	// Another round may have learned the same camera while this one was
	// talking to it: the interfaces and the configured cameras are learned
	// concurrently.
	if devInPlace := lan.findLocked(camID, xaddr, static); devInPlace != nil {
		refreshLocked(devInPlace, xaddr, generation, static)

		// The agent we just built is redundant. It has already bound its bus
		// endpoint, so dropping it on the floor would leak that endpoint.
		lan.discard(ctl, agent)
		return nil
	}

	entry := &cameraEntry{
		id:         camID,
		ctl:        ctl,
		generation: generation,
		xaddr:      xaddr,
		static:     static,
	}
	lan.devices.Add(entry)
	utils.Logger.Info().
		Str("key", entry.PK()).
		Str("endpoint", discovered.Xaddr).
		Uint32("gen", generation).
		Bool("static", static).
		Str("action", "add").
		Msg("device")

	// The camera runs under the swarm's context, never under ctx: that one is
	// the contact deadline of this exchange, and would stop the camera
	// deviceContactTimeout after it was learned.
	lan.camsSwarm.Run(func(runCtx context.Context) {
		agent.Run(runCtx)
		lan.forget(runCtx, entry)
	})
	return nil
}

// forget drops a camera whose agent has returned, so that a later round learns
// it again.
//
// A camera agent returns when its context ends; on an EXIT, which only the
// purge sends, having taken the entry already; or because its bundle stopped --
// the upload to the hub broke, and the media sink with it. The entry used to
// stay in that last case, holding a controller to a bus nobody served, and
// since a known camera is never learned again, every PLAY failed until the
// process restarted.
//
// Only the entry this agent was registered under is dropped. By now the
// registry may hold none, if the purge took it, or a newer one for the same
// camera; either way its controller belongs to whoever removed or added it.
func (lan *Agent) forget(ctx context.Context, entry *cameraEntry) {
	lan.dataLock.Lock()
	current, ok := lan.devices.Get(entry.id)
	owned := ok && current == entry
	if owned {
		lan.devices.Remove(entry.id)
	}
	lan.dataLock.Unlock()

	if !owned {
		return
	}
	if ctx.Err() == nil {
		// Not a shutdown: the camera stopped on its own.
		utils.Logger.Warn().Str("key", entry.PK()).Str("action", "forget").
			Msg("device stopped, until the next rescan learns it again")
	}
	if err := entry.ctl.Close(); err != nil {
		utils.Logger.Warn().Err(err).Str("key", entry.PK()).Str("action", "forget").Msg("device")
	}
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
	auth := networking.ClientAuth{Username: lan.cfg.User, Password: lan.cfg.Password}

	// Update the devices that match the
	for _, dev := range discovered {
		if lan.staticAddresses[canonicalXaddr(dev.Xaddr)] {
			// Named by the configuration, with an identity and credentials of
			// its own: the static loop learns it, and only the static loop.
			utils.Logger.Debug().Str("url", dev.Xaddr).Str("action", "configured").Msg("device")
			continue
		}
		if err := lan.learnDiscoveredCamera(ctx, gen, dev, auth); err != nil {
			utils.Logger.Warn().Str("url", dev.Xaddr).Err(err).Msg("invalid device discovered")
		}
	}

	// A round that the agent's stopping has interrupted purges nothing. The
	// cameras are stopping with the agent, and forget closes each controller
	// still registered. An EXIT sent to a camera agent that has already gone
	// finds no peer, and waits out the whole send deadline before failing: one
	// such wait per camera, against the grace a supervisor gives SIGTERM.
	if ctx.Err() != nil {
		return
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
		//
		// Not once the agent is stopping, for the reason above: the camera
		// swarm stops what is left. The controller is still closed here, since
		// the entry is out of the registry and forget will not see it.
		if ctx.Err() == nil {
			if err := entry.ctl.Exit(); err != nil {
				utils.Logger.Warn().Err(err).Str("key", entry.PK()).Str("action", "purge").Msg("device")
			}
		}
		if err := entry.ctl.Close(); err != nil {
			utils.Logger.Warn().Err(err).Str("key", entry.PK()).Str("action", "purge").Msg("device")
		}
	}
}

// learnDiscoveredCamera is learnSingleCameraSync for one discovered device,
// within deviceContactTimeout.
func (lan *Agent) learnDiscoveredCamera(ctx context.Context, gen uint32, dev networking.ClientInfo, auth networking.ClientAuth) error {
	ctx, cancel := context.WithTimeout(ctx, lan.contactTimeout)
	defer cancel()
	return lan.learnSingleCameraSync(ctx, gen, dev, auth, false)
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
		// A configured camera is not discovered, so its generation says nothing
		// about whether it is still there.
		if dev.static {
			continue
		}
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
	// Only when runStaticLoop is running to take it, as Run decides.
	if len(lan.devicesStatic) > 0 {
		lan.triggerStaticAsync(ctx, gen)
	}
}

func (lan *Agent) PK() string { return "lan" }
