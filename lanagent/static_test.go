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
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juju/errors"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/go-swarm"
	"github.com/ontavu/onvif/v2/networking"
	"github.com/ontavu/onvif/v2/sdk"
)

// fakeAppliance is a device as far as the LAN agent looks at one: its
// identifier. The embedded interface stays nil, and calling anything else on it
// is a test that has wandered outside what it means to cover.
type fakeAppliance struct {
	sdk.Appliance
	uuid string
}

func (f fakeAppliance) GetUUID() string { return f.uuid }

type openCall struct {
	info networking.ClientInfo
	auth networking.ClientAuth
}

// fakeNetwork stands in for Agent.openDevice: what each address answers, and
// who was contacted with what.
type fakeNetwork struct {
	mutex sync.Mutex
	uuids map[string]string // the endpoint reference each address reports
	down  map[string]bool
	calls []openCall

	// contacts, when set, receives the address of every device contacted,
	// for a test that has to wait for a round it did not run itself.
	contacts chan string
}

func newFakeNetwork() *fakeNetwork {
	return &fakeNetwork{uuids: map[string]string{}, down: map[string]bool{}}
}

func (n *fakeNetwork) open(_ context.Context, info networking.ClientInfo, auth networking.ClientAuth) (sdk.Appliance, error) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	n.calls = append(n.calls, openCall{info: info, auth: auth})
	if n.contacts != nil {
		select {
		case n.contacts <- info.Xaddr:
		default:
			// Nobody waits for this one; the calls slice still has it.
		}
	}
	if n.down[info.Xaddr] {
		return nil, errors.New("fake device down")
	}
	uuid := info.Uuid
	if uuid == "" {
		uuid = n.uuids[info.Xaddr]
	}
	return fakeAppliance{uuid: uuid}, nil
}

func (n *fakeNetwork) setDown(xaddr string, down bool) {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	n.down[xaddr] = down
}

func (n *fakeNetwork) contacted() []openCall {
	n.mutex.Lock()
	defer n.mutex.Unlock()
	return append([]openCall(nil), n.calls...)
}

// fakeFactory builds fake cameras and records the credentials each was given.
type fakeFactory struct {
	mutex sync.Mutex
	auths []networking.ClientAuth
}

func (f *fakeFactory) build(_ sdk.Appliance, auth networking.ClientAuth) (camctrl.CameraController, CameraAgent, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.auths = append(f.auths, auth)
	return &fakeController{}, &fakeAgent{}, nil
}

func (f *fakeFactory) built() []networking.ClientAuth {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]networking.ClientAuth(nil), f.auths...)
}

// newStaticAgent builds an agent over a fake network, with the camera swarm
// that Run would have started, so that the learning paths can be driven one
// round at a time.
func newStaticAgent(t *testing.T, cfg Config, network *fakeNetwork, factory *fakeFactory) *Agent {
	t.Helper()

	lan, err := New(cfg, factory.build, testURL(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if network != nil {
		lan.open = network.open
	}

	lan.camsSwarm = swarm.New(context.Background())
	t.Cleanup(func() {
		lan.camsSwarm.Cancel()
		lan.camsSwarm.Wait()
		if err := lan.Close(); err != nil {
			t.Errorf("agent close: %v", err)
		}
	})
	return lan
}

// entrySnapshot is a copy of a registry entry, taken under the lock.
type entrySnapshot struct {
	id     string
	xaddr  string
	static bool
}

func snapshot(lan *Agent) []entrySnapshot {
	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()
	out := make([]entrySnapshot, 0, len(lan.devices))
	for _, entry := range lan.devices {
		out = append(out, entrySnapshot{id: entry.id, xaddr: entry.xaddr, static: entry.static})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func roundCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCanonicalXaddr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"192.168.1.64", "192.168.1.64:80"},
		{"192.168.1.64:80", "192.168.1.64:80"},
		{"192.168.1.64:8000", "192.168.1.64:8000"},
		{"Cam.LAN:8080", "cam.lan:8080"},
		{"[fe80::1]", "[fe80::1]:80"},
		{"fe80::1", "[fe80::1]:80"},
		{"[fe80::1]:8000", "[fe80::1]:8000"},
	} {
		if got := canonicalXaddr(tc.in); got != tc.want {
			t.Errorf("canonicalXaddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNew_ConfiguredCameras(t *testing.T) {
	for _, tc := range []struct {
		name string
		cam  CameraConfig
		ok   bool
	}{
		{"a host", CameraConfig{Address: "192.168.1.64"}, true},
		{"a host and a port", CameraConfig{Address: "cam.lan:8080"}, true},
		{"an IPv6 literal", CameraConfig{Address: "[fe80::1]:80"}, true},
		{"no address", CameraConfig{UUID: "garage"}, false},
		{"a URL", CameraConfig{Address: "http://192.168.1.64"}, false},
		{"a path", CameraConfig{Address: "192.168.1.64/onvif/device_service"}, false},
		{"a blank", CameraConfig{Address: "192.168.1.64 "}, false},
		{"an identifier with a blank", CameraConfig{Address: "192.168.1.64", UUID: "my cam"}, false},
		{"an IPv6 literal without brackets", CameraConfig{Address: "fe80::1"}, false},
		{"a port that is not a number", CameraConfig{Address: "192.168.1.64:abc"}, false},
		{"an empty port", CameraConfig{Address: "192.168.1.64:"}, false},
		{"port zero", CameraConfig{Address: "192.168.1.64:0"}, false},
		{"a port out of range", CameraConfig{Address: "192.168.1.64:70000"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan, err := New(Config{Cameras: []CameraConfig{tc.cam}}, nil, testURL(t))
			if err == nil {
				if cerr := lan.Close(); cerr != nil {
					t.Errorf("agent close: %v", cerr)
				}
			}
			if (err == nil) != tc.ok {
				t.Fatalf("New with %+v: err = %v, want accepted = %v", tc.cam, err, tc.ok)
			}
		})
	}
}

// The second of two cameras on one address, or under one uuid, would be taken
// for the first and never contacted.
func TestNew_RefusesTwoCamerasAsOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		cams []CameraConfig
	}{
		{"one address written two ways", []CameraConfig{{Address: "192.168.1.64"}, {Address: "192.168.1.64:80"}}},
		{"one uuid", []CameraConfig{{Address: "192.168.1.64", UUID: "garage"}, {Address: "192.168.1.65", UUID: "garage"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan, err := New(Config{Cameras: tc.cams}, nil, testURL(t))
			if err == nil {
				if cerr := lan.Close(); cerr != nil {
					t.Errorf("agent close: %v", cerr)
				}
				t.Fatalf("New accepted %+v", tc.cams)
			}
		})
	}
}

// A camera's own credentials replace the agent's one field at a time, and they
// reach both the ONVIF exchange and the camera agent, which opens the RTSP
// session with them.
func TestStatic_LearnedWithItsOwnCredentials(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:cam-1"
	factory := &fakeFactory{}
	lan := newStaticAgent(t, Config{
		User:     "admin",
		Password: "agent-secret",
		Cameras:  []CameraConfig{{Address: "192.168.1.64", Password: "camera-secret"}},
	}, network, factory)

	lan.learnStaticCamerasSync(roundCtx(t), 1)

	want := networking.ClientAuth{Username: "admin", Password: "camera-secret"}
	calls := network.contacted()
	if len(calls) != 1 || calls[0].auth != want {
		t.Fatalf("contacted %+v, want once with %+v", calls, want)
	}
	if built := factory.built(); len(built) != 1 || built[0] != want {
		t.Fatalf("camera agents built with %+v, want once with %+v", built, want)
	}
	got := snapshot(lan)
	if len(got) != 1 || got[0] != (entrySnapshot{id: "urn:uuid:cam-1", xaddr: "192.168.1.64:80", static: true}) {
		t.Fatalf("registry = %+v", got)
	}
}

func TestStatic_ConfiguredUUIDIsTheIdentifier(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:cam-1"
	lan := newStaticAgent(t, Config{
		Cameras: []CameraConfig{{Address: "192.168.1.64", UUID: "garage"}},
	}, network, &fakeFactory{})

	lan.learnStaticCamerasSync(roundCtx(t), 1)

	if calls := network.contacted(); len(calls) != 1 || calls[0].info.Uuid != "garage" {
		t.Fatalf("contacted %+v, want once with the configured uuid", calls)
	}
	if got := lan.Cameras(); len(got) != 1 || got[0] != "garage" {
		t.Fatalf("cameras = %v, want [garage]", got)
	}
}

// Once known, a configured camera belongs to its camera agent, and the rounds
// leave it alone.
func TestStatic_NotContactedOnceKnown(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:cam-1"
	factory := &fakeFactory{}
	lan := newStaticAgent(t, Config{
		Cameras: []CameraConfig{{Address: "192.168.1.64"}},
	}, network, factory)

	for gen := uint32(1); gen <= 3; gen++ {
		lan.learnStaticCamerasSync(roundCtx(t), gen)
	}

	if calls := network.contacted(); len(calls) != 1 {
		t.Fatalf("contacted %d times, want once", len(calls))
	}
	if built := factory.built(); len(built) != 1 {
		t.Fatalf("%d camera agents built, want one", len(built))
	}
}

// A camera that is down when the agent starts is picked up by a later round.
func TestStatic_RetriedUntilItAnswers(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:cam-1"
	network.setDown("192.168.1.64", true)
	lan := newStaticAgent(t, Config{
		Cameras: []CameraConfig{{Address: "192.168.1.64"}},
	}, network, &fakeFactory{})

	lan.learnStaticCamerasSync(roundCtx(t), 1)
	if got := lan.Cameras(); len(got) != 0 {
		t.Fatalf("cameras = %v while the device is down", got)
	}

	network.setDown("192.168.1.64", false)
	lan.learnStaticCamerasSync(roundCtx(t), 2)
	if got := lan.Cameras(); len(got) != 1 || got[0] != "urn:uuid:cam-1" {
		t.Fatalf("cameras = %v once the device answers", got)
	}
}

// A device that reports no identifier, and has none in the configuration, is
// refused before a camera agent is built for it.
func TestStatic_WithoutIdentifierRefused(t *testing.T) {
	factory := &fakeFactory{}
	lan := newStaticAgent(t, Config{
		Cameras: []CameraConfig{{Address: "192.168.1.64"}},
	}, newFakeNetwork(), factory)

	lan.learnStaticCamerasSync(roundCtx(t), 1)

	if got := lan.Cameras(); len(got) != 0 {
		t.Fatalf("cameras = %v", got)
	}
	if built := factory.built(); len(built) != 0 {
		t.Fatalf("%d camera agents built for a device without an identifier", len(built))
	}
}

func TestStatic_NeverPurged(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:static"
	lan := newStaticAgent(t, Config{
		GraceGenerations: 1,
		Cameras:          []CameraConfig{{Address: "192.168.1.64"}},
	}, network, &fakeFactory{})

	lan.learnStaticCamerasSync(roundCtx(t), 1)
	registerAtGeneration(lan, "urn:uuid:discovered", 1, &fakeController{})

	if got := purgedIDs(lan, 10); len(got) != 1 || got[0] != "urn:uuid:discovered" {
		t.Fatalf("purged %v, want only the discovered camera", got)
	}
	if got := lan.Cameras(); len(got) != 1 || got[0] != "urn:uuid:static" {
		t.Fatalf("cameras = %v after the purge", got)
	}
}

// Discovery reaching a configured camera first leaves it to the static loop.
// Learning it there would register it under the identifier WS-Discovery gave
// and with the agent's credentials, and which of the two loops won the race
// would decide what the hub sees.
func TestDiscovered_LeavesAConfiguredAddressToTheStaticLoop(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:cam-1"
	factory := &fakeFactory{}
	lan := newStaticAgent(t, Config{
		User:     "admin",
		Password: "agent-secret",
		Cameras:  []CameraConfig{{Address: "192.168.1.64:80", UUID: "garage", Password: "camera-secret"}},
	}, network, factory)

	lan.learnAllCamerasSync(roundCtx(t), 1, []networking.ClientInfo{
		{Xaddr: "192.168.1.64", Uuid: "urn:uuid:cam-1"},
	})
	if calls := network.contacted(); len(calls) != 0 {
		t.Fatalf("discovery contacted a configured camera: %+v", calls)
	}

	lan.learnStaticCamerasSync(roundCtx(t), 1)

	if got := lan.Cameras(); len(got) != 1 || got[0] != "garage" {
		t.Fatalf("cameras = %v, want [garage]", got)
	}
	want := networking.ClientAuth{Username: "admin", Password: "camera-secret"}
	if built := factory.built(); len(built) != 1 || built[0] != want {
		t.Fatalf("camera agents built with %+v, want once with %+v", built, want)
	}
}

// A configured camera that answers with the identifier of a camera known at
// another address is not learned, and the camera already there is left as it
// was -- purgeable, since nobody configured it.
func TestStatic_SameIdentifierElsewhereIsNotMerged(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.65"] = "urn:uuid:cam-1"
	factory := &fakeFactory{}
	lan := newStaticAgent(t, Config{
		GraceGenerations: 1,
		Cameras:          []CameraConfig{{Address: "192.168.1.65"}},
	}, network, factory)

	lan.learnAllCamerasSync(roundCtx(t), 1, []networking.ClientInfo{
		{Xaddr: "192.168.1.64", Uuid: "urn:uuid:cam-1"},
	})
	lan.learnStaticCamerasSync(roundCtx(t), 1)

	got := snapshot(lan)
	if len(got) != 1 || got[0] != (entrySnapshot{id: "urn:uuid:cam-1", xaddr: "192.168.1.64:80", static: false}) {
		t.Fatalf("registry = %+v", got)
	}
	if built := factory.built(); len(built) != 1 {
		t.Fatalf("%d camera agents built, want one", len(built))
	}
	if purged := purgedIDs(lan, 10); len(purged) != 1 || purged[0] != "urn:uuid:cam-1" {
		t.Fatalf("purged %v: the discovered camera became unpurgeable", purged)
	}
}

// A device whose identifier turns out to be known once it is asked gets no
// agent built for it. In production the agent's endpoint is named after the
// identifier, so building a second one would not even bind.
func TestLearn_IdentifierKnownBeforeBuilding(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:cam-1"
	factory := &fakeFactory{}
	lan := newStaticAgent(t, Config{}, network, factory)

	lan.learnAllCamerasSync(roundCtx(t), 1, []networking.ClientInfo{
		{Xaddr: "192.168.1.64", Uuid: "urn:uuid:cam-1"},
	})
	// The same device, from a probe reply that carried no endpoint reference.
	lan.learnAllCamerasSync(roundCtx(t), 2, []networking.ClientInfo{
		{Xaddr: "192.168.1.64"},
	})

	if built := factory.built(); len(built) != 1 {
		t.Fatalf("%d camera agents built, want one", len(built))
	}
}

// barrierFactory holds every camera it builds until two have been asked for,
// so that two learners are inside learnSingleCameraSync at once.
type barrierFactory struct {
	t       *testing.T
	arrived atomic.Int32
	release chan struct{}

	mutex  sync.Mutex
	agents []*fakeAgent
}

func (f *barrierFactory) build(_ sdk.Appliance, _ networking.ClientAuth) (camctrl.CameraController, CameraAgent, error) {
	if f.arrived.Add(1) == 2 {
		close(f.release)
	}
	select {
	case <-f.release:
	case <-time.After(10 * time.Second):
		f.t.Error("the second learner never reached the factory")
	}

	agent := &fakeAgent{}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.agents = append(f.agents, agent)
	return &fakeController{}, agent, nil
}

// Two interfaces discovering one camera at the same time: both build it, one
// registers it, and the other's agent is released rather than leaked.
func TestLearn_ConcurrentDiscoveriesBuildOneCamera(t *testing.T) {
	for round := 0; round < 20; round++ {
		factory := &barrierFactory{t: t, release: make(chan struct{})}
		lan := newStaticAgent(t, Config{}, newFakeNetwork(), &fakeFactory{})
		lan.newCamera = factory.build

		var wg sync.WaitGroup
		for _, itf := range []string{"eth0", "eth1"} {
			wg.Add(1)
			go func(itf string) {
				defer wg.Done()
				lan.learnAllCamerasSync(roundCtx(t), 1, []networking.ClientInfo{
					{Xaddr: "192.168.1.64", Uuid: "urn:uuid:cam-1"},
				})
			}(itf)
		}
		wg.Wait()

		closed := 0
		for _, agent := range factory.agents {
			if agent.wasClosed() {
				closed++
			}
		}
		if got := lan.Cameras(); len(got) != 1 || len(factory.agents) != 2 || closed != 1 {
			t.Fatalf("round %d: cameras %v, %d built, %d released; want 1, 2, 1",
				round, got, len(factory.agents), closed)
		}
	}
}

// Run starts the static loop and triggers it, with no interface selected at
// all, and leaves nothing behind when it returns: no loop, no camera agent, and
// no controller left open.
func TestRun_LearnsTheConfiguredCameras(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:cam-1"
	network.contacts = make(chan string, 1)
	factory := &stoppableFactory{}
	lan, err := New(Config{
		Cameras:     []CameraConfig{{Address: "192.168.1.64"}},
		ScanPeriod:  time.Hour,
		CheckPeriod: time.Hour,
	}, factory.build, testURL(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lan.open = network.open
	t.Cleanup(func() {
		if err := lan.Close(); err != nil {
			t.Errorf("agent close: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		lan.Run(ctx)
	}()

	select {
	case <-network.contacts:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never contacted the configured camera")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return on a cancelled context")
	}

	// The round in flight when the context ended still completes: the loops are
	// joined before the cameras are, so the camera it learned was started, then
	// stopped and forgotten with the rest.
	if n := factory.count(); n != 1 {
		t.Fatalf("%d camera agents built, want one", n)
	}
	if ctl, _ := factory.pair(0); ctl.closes() != 1 {
		t.Fatalf("the camera's controller was closed %d times, want once", ctl.closes())
	}
	if got := lan.Cameras(); len(got) != 0 {
		t.Fatalf("cameras = %v after Run returned", got)
	}
	if n := lan.camsSwarm.Count(); n != 0 {
		t.Fatalf("%d camera agents still running after Run returned", n)
	}
	if n := lan.nicsGroup.Count(); n != 0 {
		t.Fatalf("%d loops still running after Run returned", n)
	}
}

// The other way round: discovery finds, at its address, a camera the
// configuration named under another identifier.
func TestDiscovered_DefersToAConfiguredCameraAtItsAddress(t *testing.T) {
	network := newFakeNetwork()
	lan := newStaticAgent(t, Config{
		Cameras: []CameraConfig{{Address: "192.168.1.64", UUID: "garage"}},
	}, network, &fakeFactory{})

	lan.learnStaticCamerasSync(roundCtx(t), 1)
	lan.learnAllCamerasSync(roundCtx(t), 2, []networking.ClientInfo{
		{Xaddr: "192.168.1.64", Uuid: "urn:uuid:cam-1"},
	})

	if calls := network.contacted(); len(calls) != 1 {
		t.Fatalf("contacted %d times, want once", len(calls))
	}
	if got := lan.Cameras(); len(got) != 1 || got[0] != "garage" {
		t.Fatalf("cameras = %v, want [garage]", got)
	}
}

// Between discovered cameras the address merges nothing: the second one may
// have taken the address the first has left.
func TestDiscovered_SameAddressDifferentCameras(t *testing.T) {
	network := newFakeNetwork()
	lan := newStaticAgent(t, Config{}, network, &fakeFactory{})

	lan.learnAllCamerasSync(roundCtx(t), 1, []networking.ClientInfo{
		{Xaddr: "192.168.1.64", Uuid: "urn:uuid:old"},
	})
	lan.learnAllCamerasSync(roundCtx(t), 2, []networking.ClientInfo{
		{Xaddr: "192.168.1.64", Uuid: "urn:uuid:new"},
	})

	got := lan.Cameras()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "urn:uuid:new" || got[1] != "urn:uuid:old" {
		t.Fatalf("cameras = %v, want both", got)
	}
}

// The loop runs a round per trigger, and returns with its context once the
// round in flight is over.
func TestRunStaticLoop_RoundPerTrigger(t *testing.T) {
	network := newFakeNetwork()
	network.uuids["192.168.1.64"] = "urn:uuid:cam-1"
	network.contacts = make(chan string, 1)
	lan := newStaticAgent(t, Config{
		Cameras: []CameraConfig{{Address: "192.168.1.64"}},
	}, network, &fakeFactory{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		lan.runStaticLoop(ctx)
	}()

	lan.triggerStaticAsync(ctx, 1)
	select {
	case <-network.contacts:
	case <-time.After(10 * time.Second):
		t.Fatal("no round ran after the trigger")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runStaticLoop did not return on a cancelled context")
	}
	if got := lan.Cameras(); len(got) != 1 || got[0] != "urn:uuid:cam-1" {
		t.Fatalf("cameras = %v after the round", got)
	}
}

// ---------- the real exchange with a device ----------

// onvifStub serves the three calls openDevice makes. An empty reference makes
// it refuse GetEndpointReference, the way a device that does not implement it
// answers.
func onvifStub(t *testing.T, reference string) (xaddr string, asked *atomic.Int32) {
	t.Helper()

	asked = &atomic.Int32{}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("stub read: %v", err)
			return
		}
		req := string(body)
		host := strings.TrimPrefix(srv.URL, "http://")

		var reply string
		w.Header().Set("Content-Type", "application/soap+xml")
		switch {
		case strings.Contains(req, "GetSystemDateAndTime"):
			reply = soap(`<tds:GetSystemDateAndTimeResponse/>`)
		case strings.Contains(req, "GetCapabilities"):
			reply = soap(`<tds:GetCapabilitiesResponse><tds:Capabilities>` +
				`<tt:Device><tt:XAddr>http://` + host + `/onvif/device_service</tt:XAddr></tt:Device>` +
				`</tds:Capabilities></tds:GetCapabilitiesResponse>`)
		case strings.Contains(req, "GetEndpointReference") && reference != "":
			asked.Add(1)
			reply = soap(`<tds:GetEndpointReferenceResponse><tds:GUID>` + reference +
				`</tds:GUID></tds:GetEndpointReferenceResponse>`)
		default:
			asked.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			reply = soap(`<env:Fault>` +
				`<env:Code><env:Value>env:Sender</env:Value>` +
				`<env:Subcode><env:Value>ter:ActionNotSupported</env:Value></env:Subcode></env:Code>` +
				`<env:Reason><env:Text xml:lang="en">Operation not supported</env:Text></env:Reason>` +
				`</env:Fault>`)
		}
		if _, err := w.Write([]byte(reply)); err != nil {
			t.Errorf("stub write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), asked
}

func soap(body string) string {
	return `<?xml version="1.0"?><s:Envelope ` +
		`xmlns:s="http://www.w3.org/2003/05/soap-envelope" ` +
		`xmlns:env="http://www.w3.org/2003/05/soap-envelope" ` +
		`xmlns:ter="http://www.onvif.org/ver10/error" ` +
		`xmlns:tds="http://www.onvif.org/ver10/device/wsdl" ` +
		`xmlns:tt="http://www.onvif.org/ver10/schema"><s:Body>` + body + `</s:Body></s:Envelope>`
}

func TestOpenDevice_AsksTheDeviceForItsIdentifier(t *testing.T) {
	xaddr, asked := onvifStub(t, "urn:uuid:from-device")
	lan := newAgent(t)

	dev, err := lan.openDevice(roundCtx(t), networking.ClientInfo{Xaddr: xaddr}, networking.ClientAuth{})
	if err != nil {
		t.Fatalf("openDevice: %v", err)
	}
	if got := dev.GetUUID(); got != "urn:uuid:from-device" {
		t.Fatalf("identifier %q, want the device's endpoint reference", got)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("GetEndpointReference asked %d times, want once", n)
	}
}

func TestOpenDevice_KeepsTheIdentifierItWasGiven(t *testing.T) {
	xaddr, asked := onvifStub(t, "urn:uuid:from-device")
	lan := newAgent(t)

	dev, err := lan.openDevice(roundCtx(t), networking.ClientInfo{Xaddr: xaddr, Uuid: "garage"}, networking.ClientAuth{})
	if err != nil {
		t.Fatalf("openDevice: %v", err)
	}
	if got := dev.GetUUID(); got != "garage" {
		t.Fatalf("identifier %q, want the one it was given", got)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("GetEndpointReference asked %d times, want never", n)
	}
}

func TestOpenDevice_NoIdentifierAnywhere(t *testing.T) {
	xaddr, _ := onvifStub(t, "")
	lan := newAgent(t)

	if _, err := lan.openDevice(roundCtx(t), networking.ClientInfo{Xaddr: xaddr}, networking.ClientAuth{}); err == nil {
		t.Fatal("openDevice accepted a device with no identifier")
	}
}

// The whole static path against a device that speaks ONVIF, with the opener
// the agent really uses.
func TestStatic_LearnedFromADevice(t *testing.T) {
	xaddr, _ := onvifStub(t, "urn:uuid:from-device")
	lan := newStaticAgent(t, Config{Cameras: []CameraConfig{{Address: xaddr}}}, nil, &fakeFactory{})

	lan.learnStaticCamerasSync(roundCtx(t), 1)

	if got := lan.Cameras(); len(got) != 1 || got[0] != "urn:uuid:from-device" {
		t.Fatalf("cameras = %v", got)
	}
}

// ---------- a camera that stops on its own ----------

// stoppableAgent is a camera agent that returns when told to, the way a real
// one does when its bundle stops: the upload to the hub broke, and the media
// sink returned.
type stoppableAgent struct {
	fakeAgent
	stop chan struct{}
}

func (a *stoppableAgent) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-a.stop:
	}
}

// stoppableFactory builds stoppable cameras, and keeps each pair.
type stoppableFactory struct {
	mutex  sync.Mutex
	ctls   []*fakeController
	agents []*stoppableAgent
}

func (f *stoppableFactory) build(_ sdk.Appliance, _ networking.ClientAuth) (camctrl.CameraController, CameraAgent, error) {
	ctl := &fakeController{}
	agent := &stoppableAgent{stop: make(chan struct{})}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.ctls = append(f.ctls, ctl)
	f.agents = append(f.agents, agent)
	return ctl, agent, nil
}

func (f *stoppableFactory) count() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.ctls)
}

func (f *stoppableFactory) pair(i int) (*fakeController, *stoppableAgent) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.ctls[i], f.agents[i]
}

func (f *fakeController) closes() int {
	count := 0
	for _, call := range f.seen() {
		if call == "Close" {
			count++
		}
	}
	return count
}

func (f *fakeController) closed() bool { return f.closes() > 0 }

// A camera whose agent stops on its own is forgotten, its controller closed,
// and the next round builds it again. It used to stay registered with a
// controller to a bus nobody served: a known camera is never learned again, so
// every PLAY failed until the process restarted.
func TestCamera_StoppedOnItsOwnIsLearnedAgain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   Config
		learn func(lan *Agent, gen uint32)
	}{
		{
			name: "discovered",
			learn: func(lan *Agent, gen uint32) {
				lan.learnAllCamerasSync(roundCtx(t), gen, []networking.ClientInfo{
					{Xaddr: "192.168.1.64", Uuid: "urn:uuid:cam-1"},
				})
			},
		},
		{
			name: "configured",
			cfg:  Config{Cameras: []CameraConfig{{Address: "192.168.1.64", UUID: "urn:uuid:cam-1"}}},
			learn: func(lan *Agent, gen uint32) {
				lan.learnStaticCamerasSync(roundCtx(t), gen)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			factory := &stoppableFactory{}
			lan := newStaticAgent(t, tc.cfg, newFakeNetwork(), &fakeFactory{})
			lan.newCamera = factory.build

			tc.learn(lan, 1)
			ctl, agent := factory.pair(0)
			close(agent.stop)
			// Its goroutine, forget included, is the only member: once it is
			// over, Wait returns, and the swarm still takes new cameras.
			lan.camsSwarm.Wait()

			if got := lan.Cameras(); len(got) != 0 {
				t.Fatalf("cameras = %v after the camera stopped", got)
			}
			if !ctl.closed() {
				t.Fatal("the stopped camera's controller was left open")
			}

			tc.learn(lan, 2)
			if got := lan.Cameras(); len(got) != 1 || got[0] != "urn:uuid:cam-1" {
				t.Fatalf("cameras = %v after the next round", got)
			}
			if n := factory.count(); n != 2 {
				t.Fatalf("%d camera agents built, want the camera built again", n)
			}
		})
	}
}

// The purge takes the entry and closes the controller itself; the agent it
// stopped then returns, and forget must leave that controller alone.
func TestCamera_PurgedIsClosedOnce(t *testing.T) {
	factory := &stoppableFactory{}
	lan := newStaticAgent(t, Config{GraceGenerations: 1}, newFakeNetwork(), &fakeFactory{})
	lan.newCamera = factory.build

	lan.learnAllCamerasSync(roundCtx(t), 1, []networking.ClientInfo{
		{Xaddr: "192.168.1.64", Uuid: "urn:uuid:cam-1"},
	})
	lan.learnAllCamerasSync(roundCtx(t), 10, nil)

	ctl, agent := factory.pair(0)
	// The EXIT the purge sent is what stops a real camera agent.
	close(agent.stop)
	lan.camsSwarm.Wait()

	if n := ctl.closes(); n != 1 {
		t.Fatalf("controller closed %d times, want once", n)
	}
}

// An agent that returns late, after its camera was learned again, must not take
// the newer entry with it.
func TestForget_LeavesANewerEntryAlone(t *testing.T) {
	lan := newAgent(t)
	oldCtl, newCtl := &fakeController{}, &fakeController{}
	older := &cameraEntry{id: "urn:uuid:cam-1", ctl: oldCtl}
	newer := &cameraEntry{id: "urn:uuid:cam-1", ctl: newCtl}
	lan.dataLock.Lock()
	lan.devices.Add(newer)
	lan.dataLock.Unlock()

	lan.forget(context.Background(), older)

	if got := lan.Cameras(); len(got) != 1 {
		t.Fatalf("cameras = %v, want the newer entry kept", got)
	}
	if oldCtl.closed() || newCtl.closed() {
		t.Fatal("forget closed a controller it did not own")
	}
}

// ---------- a device that never answers ----------

// A device that accepts the connection and never replies holds back neither
// the round nor the devices after it, on either path.
func TestLearn_ADeviceThatNeverAnswersHoldsBackNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   Config
		learn func(lan *Agent)
	}{
		{
			name: "discovered",
			learn: func(lan *Agent) {
				lan.learnAllCamerasSync(context.Background(), 1, []networking.ClientInfo{
					{Xaddr: "192.168.1.64", Uuid: "urn:uuid:tarpit"},
					{Xaddr: "192.168.1.65", Uuid: "urn:uuid:cam-2"},
				})
			},
		},
		{
			name: "configured",
			cfg: Config{Cameras: []CameraConfig{
				{Address: "192.168.1.64"},
				{Address: "192.168.1.65", UUID: "urn:uuid:cam-2"},
			}},
			learn: func(lan *Agent) { lan.learnStaticCamerasSync(context.Background(), 1) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			network := newFakeNetwork()
			lan := newStaticAgent(t, tc.cfg, nil, &fakeFactory{})
			lan.contactTimeout = 50 * time.Millisecond
			lan.open = func(ctx context.Context, info networking.ClientInfo, auth networking.ClientAuth) (sdk.Appliance, error) {
				if info.Xaddr == "192.168.1.64" {
					<-ctx.Done()
					return nil, errors.Trace(ctx.Err())
				}
				// The real opener honours its context for every device. Doing
				// the same here is what tells a deadline per device from one
				// shared by the round, which the silent device would have
				// spent for everybody after it.
				if err := ctx.Err(); err != nil {
					return nil, errors.Trace(err)
				}
				return network.open(ctx, info, auth)
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.learn(lan)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("a device that never answers held the round")
			}

			if got := lan.Cameras(); len(got) != 1 || got[0] != "urn:uuid:cam-2" {
				t.Fatalf("cameras = %v, want the device after the silent one", got)
			}
		})
	}
}

// ---------- the purge, when the agent stops ----------

// cancellingController ends the round's context when it is told to exit, the
// way SIGTERM can land while a purge is under way.
type cancellingController struct {
	fakeController
	cancel context.CancelFunc
}

func (c *cancellingController) Exit() error {
	c.cancel()
	return c.fakeController.Exit()
}

func (f *fakeController) exits() int {
	count := 0
	for _, call := range f.seen() {
		if call == "Exit" {
			count++
		}
	}
	return count
}

// A round the agent's stopping has interrupted purges nothing: an EXIT to a
// camera agent already gone would wait out the send deadline, once per camera.
func TestPurge_NotOnceTheAgentIsStopping(t *testing.T) {
	lan := newStaticAgent(t, Config{GraceGenerations: 1}, newFakeNetwork(), &fakeFactory{})
	ctl := &fakeController{}
	registerAtGeneration(lan, "urn:uuid:stale", 1, ctl)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lan.learnAllCamerasSync(ctx, 10, nil)

	if got := lan.Cameras(); len(got) != 1 {
		t.Fatalf("cameras = %v: a stopping agent purged", got)
	}
	if n := ctl.exits(); n != 0 {
		t.Fatalf("EXIT sent %d times by a stopping agent", n)
	}
}

// Stopping in the middle of a purge: no EXIT after that, and every controller
// already taken from the registry is still closed, since nothing else will.
func TestPurge_StoppingMidwaySendsNoMoreExit(t *testing.T) {
	lan := newStaticAgent(t, Config{GraceGenerations: 1}, newFakeNetwork(), &fakeFactory{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctls := []*cancellingController{{cancel: cancel}, {cancel: cancel}}
	registerAtGeneration(lan, "urn:uuid:stale-a", 1, ctls[0])
	registerAtGeneration(lan, "urn:uuid:stale-b", 1, ctls[1])

	lan.learnAllCamerasSync(ctx, 10, nil)

	exits := ctls[0].exits() + ctls[1].exits()
	if exits != 1 {
		t.Fatalf("EXIT sent %d times, want only the one before the stop", exits)
	}
	for i, ctl := range ctls {
		if n := ctl.closes(); n != 1 {
			t.Fatalf("controller %d closed %d times, want once", i, n)
		}
	}
	if got := lan.Cameras(); len(got) != 0 {
		t.Fatalf("cameras = %v after the purge", got)
	}
}
