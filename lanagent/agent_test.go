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
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/lanctrl"
)

// fakeController records what the LAN agent asks of a camera. Standing in for
// camctrl.Client is the whole point of the camctrl.CameraController interface: no
// socket to a camera is involved in these tests.
type fakeController struct {
	mutex  sync.Mutex
	calls  []string
	failOn string
	media  camctrl.Media
}

func (f *fakeController) record(name string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, name)
	if f.failOn == name {
		return errors.New("fake failure on " + name)
	}
	return nil
}

func (f *fakeController) seen() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeController) Media() (camctrl.Media, error) {
	if err := f.record("Media"); err != nil {
		return camctrl.Media{}, err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.media, nil
}

func (f *fakeController) Play() error  { return f.record("Play") }
func (f *fakeController) Pause() error { return f.record("Pause") }
func (f *fakeController) Ping() error  { return f.record("Ping") }
func (f *fakeController) Exit() error  { return f.record("Exit") }
func (f *fakeController) Close() error { return f.record("Close") }

// fakeAgent stands in for the runnable half of a camera. It must be closable,
// because the LAN agent has to release an agent it decides not to start.
type fakeAgent struct {
	mutex  sync.Mutex
	closed bool
}

func (f *fakeAgent) Run(ctx context.Context) { <-ctx.Done() }

func (f *fakeAgent) Close() error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.closed = true
	return nil
}

func (f *fakeAgent) wasClosed() bool {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.closed
}

func testURL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("inproc://cams/test/lan-%s-%d", t.Name(), time.Now().UnixNano())
}

// newAgent builds an agent whose endpoint nobody serves, for the tests that
// exercise the Go API directly.
func newAgent(t *testing.T) *Agent {
	t.Helper()

	lan, err := New(Config{}, nil, testURL(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := lan.Close(); err != nil {
			t.Errorf("agent close: %v", err)
		}
	})
	return lan
}

// register puts a camera in the registry without going through the ONVIF
// discovery, which needs a network.
func register(lan *Agent, id string, ctl camctrl.CameraController) {
	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()
	lan.devices.Add(&cameraEntry{id: id, ctl: ctl, generation: 1})
}

func TestUpdateStreamExpectation_UnknownCamera(t *testing.T) {
	lan := newAgent(t)

	err := lan.UpdateStreamExpectation("nope", lanctrl.CommandPlay)
	if err == nil {
		t.Fatal("a command for an unknown camera was accepted")
	}
	if !errors.Is(err, ErrNoSuchCamera) {
		t.Fatalf("error %v does not match ErrNoSuchCamera", err)
	}
}

func TestUpdateStreamExpectation_Forwarded(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  lanctrl.Command
		want string
	}{
		{"play", lanctrl.CommandPlay, "Play"},
		{"pause", lanctrl.CommandPause, "Pause"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan := newAgent(t)
			ctl := &fakeController{}
			register(lan, "cam-1", ctl)

			if err := lan.UpdateStreamExpectation("cam-1", tc.cmd); err != nil {
				t.Fatalf("UpdateStreamExpectation: %v", err)
			}

			got := ctl.seen()
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("controller saw %v, want [%s]", got, tc.want)
			}
		})
	}
}

// TestUpdateStreamExpectation_ReportsFailure is the point of routing commands
// through a request/reply seam: a camera that refuses has to be visible to the
// caller instead of disappearing into a queue.
func TestUpdateStreamExpectation_ReportsFailure(t *testing.T) {
	lan := newAgent(t)
	register(lan, "cam-1", &fakeController{failOn: "Play"})

	if err := lan.UpdateStreamExpectation("cam-1", lanctrl.CommandPlay); err == nil {
		t.Fatal("a refused Play was reported as a success")
	}
}

func TestUpdateStreamExpectation_UnknownCommand(t *testing.T) {
	lan := newAgent(t)
	ctl := &fakeController{}
	register(lan, "cam-1", ctl)

	if err := lan.UpdateStreamExpectation("cam-1", lanctrl.Command("PIROUETTE")); err == nil {
		t.Fatal("an unknown command was accepted")
	}
	if got := ctl.seen(); len(got) != 0 {
		t.Fatalf("controller saw %v, want nothing", got)
	}
}

func TestCameras_ReturnsIdentifiers(t *testing.T) {
	lan := newAgent(t)
	register(lan, "cam-b", &fakeController{})
	register(lan, "cam-a", &fakeController{})

	got := lan.Cameras()
	sort.Strings(got)

	if len(got) != 2 || got[0] != "cam-a" || got[1] != "cam-b" {
		t.Fatalf("Cameras() = %v, want [cam-a cam-b]", got)
	}
}

// TestDiscard_ReleasesTheAgent covers the path taken when two NICs discover the
// same camera at once. The loser has already bound its bus endpoint, so it has
// to be closed rather than dropped on the floor.
func TestDiscard_ReleasesTheAgent(t *testing.T) {
	lan := newAgent(t)
	ctl := &fakeController{}
	agent := &fakeAgent{}

	lan.discard(ctl, agent)

	if !agent.wasClosed() {
		t.Fatal("the discarded agent was not closed, its bus endpoint leaks")
	}
	got := ctl.seen()
	if len(got) != 1 || got[0] != "Close" {
		t.Fatalf("controller saw %v, want [Close]", got)
	}
}

func TestCounts_UnderTheLock(t *testing.T) {
	lan := newAgent(t)
	register(lan, "cam-a", &fakeController{})
	lan.registerInterface("eth0")

	devices, interfaces := lan.counts()
	if devices != 1 || interfaces != 1 {
		t.Fatalf("counts() = (%d, %d), want (1, 1)", devices, interfaces)
	}
}

// ---------- over the bus ----------

// serveAgent starts the bus loop of an agent and returns a controller for it,
// which is how the upstream agent reaches the LAN agent in production.
func serveAgent(t *testing.T) (*Agent, *lanctrl.Client) {
	t.Helper()

	url := testURL(t)
	lan, err := New(Config{}, nil, url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		lan.bus.Serve(ctx, lan.dispatch)
	}()

	cli, err := lanctrl.New(url, lanctrl.WithTimeout(2*time.Second))
	if err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("lanctrl.New: %v", err)
	}

	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Errorf("controller close: %v", err)
		}
		cancel()
		wg.Wait()
	})

	return lan, cli
}

func TestBus_ListEmpty(t *testing.T) {
	_, cli := serveAgent(t)

	got, err := cli.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List() = %v, want empty", got)
	}
}

func TestBus_ListReportsCameras(t *testing.T) {
	lan, cli := serveAgent(t)
	register(lan, "cam-b", &fakeController{})
	register(lan, "cam-a", &fakeController{})

	got, err := cli.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "cam-a" || got[1] != "cam-b" {
		t.Fatalf("List() = %v, want [cam-a cam-b]", got)
	}
}

func TestBus_PlayAndPauseReachTheCamera(t *testing.T) {
	lan, cli := serveAgent(t)
	target := &fakeController{}
	other := &fakeController{}
	register(lan, "cam-1", target)
	register(lan, "cam-2", other)

	if err := cli.Play("cam-1"); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if err := cli.Pause("cam-1"); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	got := target.seen()
	if len(got) != 2 || got[0] != "Play" || got[1] != "Pause" {
		t.Fatalf("target saw %v, want [Play Pause]", got)
	}
	if got := other.seen(); len(got) != 0 {
		t.Fatalf("the other camera saw %v, want nothing", got)
	}
}

func TestBus_UnknownCameraRefused(t *testing.T) {
	_, cli := serveAgent(t)

	if err := cli.Play("nope"); err == nil {
		t.Fatal("a Play for an unknown camera was accepted")
	}
	// The agent has to still be serving afterwards.
	if err := cli.Ping(); err != nil {
		t.Fatalf("the agent stopped serving: %v", err)
	}
}

// TestBus_PlayWithoutCamera checks the argument is required rather than
// defaulted: a PLAY naming nobody must not silently do nothing.
func TestBus_PlayWithoutCamera(t *testing.T) {
	lan, cli := serveAgent(t)
	ctl := &fakeController{}
	register(lan, "cam-1", ctl)

	reply, exit := lan.dispatch(context.Background(), string(lanctrl.CommandPlay), "")
	if exit {
		t.Fatal("a bad PLAY asked the agent to stop")
	}
	if _, err := decodeForTest(reply); err == nil {
		t.Fatalf("PLAY without a camera was accepted, reply %q", string(reply))
	}
	if got := ctl.seen(); len(got) != 0 {
		t.Fatalf("controller saw %v, want nothing", got)
	}

	if err := cli.Ping(); err != nil {
		t.Fatalf("the agent stopped serving: %v", err)
	}
}

func TestBus_UnknownVerbRefused(t *testing.T) {
	lan, cli := serveAgent(t)

	reply, exit := lan.dispatch(context.Background(), "PIROUETTE", "")
	if exit {
		t.Fatal("an unknown verb asked the agent to stop")
	}
	if _, err := decodeForTest(reply); err == nil {
		t.Fatalf("an unknown verb was accepted, reply %q", string(reply))
	}

	if err := cli.Ping(); err != nil {
		t.Fatalf("the agent stopped serving: %v", err)
	}
}

// TestBus_WhitespaceIdentifierRejected pins the guard that keeps a LIST reply
// unambiguous. A device reporting such a UUID must not reach the registry.
func TestBus_WhitespaceIdentifierRejected(t *testing.T) {
	lan, cli := serveAgent(t)
	register(lan, "cam-a", &fakeController{})

	// EncodeIDs is the last line of defence, below the registry guard.
	if got := lanctrl.EncodeIDs([]string{"cam-a", "bad id", "", "cam-b"}); got != "cam-a cam-b" {
		t.Fatalf("EncodeIDs kept an ambiguous identifier: %q", got)
	}

	got, err := cli.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0] != "cam-a" {
		t.Fatalf("List() = %v, want [cam-a]", got)
	}
}

// decodeForTest reads a reply the way a controller would, so that a test can
// assert on what dispatch produced without going through a socket.
func decodeForTest(reply []byte) (string, error) {
	return agentbus.DecodeReply(reply)
}

// TestRun_LeavesNothingRunning pins the ownership rule from ARCHITECTURE.md:
// when an owner exits it must leave no background agent running. A cancelled
// context makes every member return at once, so what is asserted is the
// teardown, not the timing.
func TestRun_LeavesNothingRunning(t *testing.T) {
	lan, err := New(Config{}, nil, testURL(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		lan.Run(ctx)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return on a cancelled context")
	}

	if n := lan.camsSwarm.Count(); n != 0 {
		t.Fatalf("%d camera agents still running after Run returned", n)
	}
	if n := lan.nicsGroup.Count(); n != 0 {
		t.Fatalf("%d nic goroutines still running after Run returned", n)
	}
}

// ---------- the purge ----------

// registerAtGeneration puts a camera in the registry as if it had last been
// seen in a given discovery round.
func registerAtGeneration(lan *Agent, id string, gen uint32, ctl camctrl.CameraController) {
	lan.dataLock.Lock()
	defer lan.dataLock.Unlock()
	lan.devices.Add(&cameraEntry{id: id, ctl: ctl, generation: gen})
}

// purgedIDs runs one purge round and reports what it claimed. Note that the
// entries are removed by the call, so a second round sees nothing.
func purgedIDs(lan *Agent, gen uint32) []string {
	out := make([]string, 0)
	for _, entry := range lan.takeCamerasToPurge(gen) {
		out = append(out, entry.id)
	}
	sort.Strings(out)
	return out
}

// TestPurge_ZeroGraceDisablesIt pins the documented meaning of the default. It
// is also the reason the inverted comparison went unnoticed: production never
// reaches the loop.
func TestPurge_ZeroGraceDisablesIt(t *testing.T) {
	lan := newAgent(t)
	lan.GraceGenerations = 0

	registerAtGeneration(lan, "fresh", 10, &fakeController{})
	registerAtGeneration(lan, "ancient", 1, &fakeController{})

	if got := purgedIDs(lan, 10); len(got) != 0 {
		t.Fatalf("purged %v with the grace at zero, want nothing", got)
	}
}

// TestPurge_KeepsTheCamerasJustSeen is the regression test for the inversion.
// The old predicate selected exactly these and destroyed them.
func TestPurge_KeepsTheCamerasJustSeen(t *testing.T) {
	lan := newAgent(t)
	lan.GraceGenerations = 3

	// Seen in this very round, and within the grace.
	registerAtGeneration(lan, "now", 10, &fakeController{})
	registerAtGeneration(lan, "one-behind", 9, &fakeController{})
	registerAtGeneration(lan, "at-the-limit", 7, &fakeController{})

	if got := purgedIDs(lan, 10); len(got) != 0 {
		t.Fatalf("purged %v, want nothing: none of them is past a grace of 3", got)
	}
}

func TestPurge_ForgetsWhatIsPastTheGrace(t *testing.T) {
	lan := newAgent(t)
	lan.GraceGenerations = 3

	registerAtGeneration(lan, "now", 10, &fakeController{})
	registerAtGeneration(lan, "at-the-limit", 7, &fakeController{})
	registerAtGeneration(lan, "one-too-old", 6, &fakeController{})
	registerAtGeneration(lan, "long-gone", 1, &fakeController{})

	got := purgedIDs(lan, 10)
	if len(got) != 2 || got[0] != "long-gone" || got[1] != "one-too-old" {
		t.Fatalf("purged %v, want [long-gone one-too-old]", got)
	}
}

// TestPurge_AcrossTheGenerationWrap is why delta has to use wrapping
// arithmetic: a camera seen just before the counter turned over has a numerically
// larger generation than the current one.
func TestPurge_AcrossTheGenerationWrap(t *testing.T) {
	lan := newAgent(t)
	lan.GraceGenerations = 3

	const wrapped = uint32(2)
	registerAtGeneration(lan, "just-before-the-wrap", math.MaxUint32-1, &fakeController{})
	registerAtGeneration(lan, "long-before-the-wrap", math.MaxUint32-100, &fakeController{})

	// delta(2, MaxUint32-1) == 4, which is past a grace of 3.
	got := purgedIDs(lan, wrapped)
	if len(got) != 2 {
		t.Fatalf("purged %v, want both entries: neither is within the grace", got)
	}
}

// TestPurge_ClaimsEachCameraOnce is the regression test for the double purge.
// Selection and removal used to be two separate acquisitions of the lock, so
// two discovery goroutines could both claim the same camera and both shut it
// down -- the second on a controller the first had already closed.
func TestPurge_ClaimsEachCameraOnce(t *testing.T) {
	lan := newAgent(t)
	lan.GraceGenerations = 1

	const cameras = 20
	for i := 0; i < cameras; i++ {
		registerAtGeneration(lan, fmt.Sprintf("cam-%02d", i), 1, &fakeController{})
	}

	// Several rounds at once, all of which consider every camera stale.
	const rounds = 8
	claimed := make(chan []*cameraEntry, rounds)
	start := make(chan struct{})
	wg := sync.WaitGroup{}
	for r := 0; r < rounds; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claimed <- lan.takeCamerasToPurge(100)
		}()
	}
	close(start)
	wg.Wait()
	close(claimed)

	seen := map[string]int{}
	total := 0
	for batch := range claimed {
		for _, entry := range batch {
			seen[entry.id]++
			total++
		}
	}

	if total != cameras {
		t.Fatalf("%d claims for %d cameras: each has to be claimed exactly once", total, cameras)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("camera %s was claimed %d times", id, n)
		}
	}

	// And the registry is empty afterwards.
	if got := lan.Cameras(); len(got) != 0 {
		t.Fatalf("%v left in the registry after the purge", got)
	}
}

func TestCameraMedia_Relayed(t *testing.T) {
	lan := newAgent(t)
	ctl := &fakeController{
		media: camctrl.Media{Encoding: camctrl.EncodingH264, Width: 640, Height: 480, GopLength: 30},
	}
	register(lan, "cam-1", ctl)

	got, err := lan.CameraMedia("cam-1")
	if err != nil {
		t.Fatalf("CameraMedia: %v", err)
	}
	if got != ctl.media {
		t.Fatalf("relayed %+v, want %+v", got, ctl.media)
	}
	if seen := ctl.seen(); len(seen) != 1 || seen[0] != "Media" {
		t.Fatalf("the camera saw %v, want [Media]", seen)
	}
}

func TestCameraMedia_UnknownCamera(t *testing.T) {
	lan := newAgent(t)

	if _, err := lan.CameraMedia("nope"); err == nil {
		t.Fatal("media for an unknown camera was answered")
	} else if !errors.Is(err, ErrNoSuchCamera) {
		t.Fatalf("error %v does not match ErrNoSuchCamera", err)
	}
}

func TestCameraMedia_ReportsFailure(t *testing.T) {
	lan := newAgent(t)
	register(lan, "cam-1", &fakeController{failOn: "Media"})

	if _, err := lan.CameraMedia("cam-1"); err == nil {
		t.Fatal("a failing camera was reported as answering")
	}
}

// Across the bus, which is the hop upagent actually makes.
func TestBus_MediaReachesTheCamera(t *testing.T) {
	lan, cli := serveAgent(t)
	want := camctrl.Media{Encoding: camctrl.EncodingH265, Width: 2560, Height: 1920}
	register(lan, "cam-1", &fakeController{media: want})

	got, err := cli.Media("cam-1")
	if err != nil {
		t.Fatalf("Media: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// The check tick nudges every camera, which is what finishes a transition the
// camera could not finish alone.
//
// A camera asked to play while its previous stream is still winding down parks
// in StateResuming: onCmdPlay returns rather than restarting, and onCmdPing is
// what completes the move afterwards. Nothing sent that ping -- the verb was on
// *Client and not on the interface the LAN agent holds -- so the camera never
// came back. This is the case that would have said so.
func TestPingCameras_ReachesEveryCamera(t *testing.T) {
	lan := newAgent(t)

	first := &fakeController{}
	second := &fakeController{}
	register(lan, "cam-one", first)
	register(lan, "cam-two", second)

	lan.pingCameras()

	for name, ctl := range map[string]*fakeController{"cam-one": first, "cam-two": second} {
		if got := ctl.seen(); len(got) != 1 || got[0] != "Ping" {
			t.Errorf("%s saw %v, want one Ping", name, got)
		}
	}
}

// One camera that will not answer must not keep the others from being nudged:
// the loop is a sweep and not a transaction.
func TestPingCameras_CarriesOnPastAFailure(t *testing.T) {
	lan := newAgent(t)

	broken := &fakeController{failOn: "Ping"}
	working := &fakeController{}
	register(lan, "cam-broken", broken)
	register(lan, "cam-working", working)

	lan.pingCameras()

	if got := working.seen(); len(got) != 1 || got[0] != "Ping" {
		t.Errorf("the second camera saw %v after the first refused", got)
	}
}

// The registry lock is released before the pings go out: a ping is a
// synchronous round trip, and the discovery goroutines write to the same
// registry. Holding it across the sweep would stall them.
func TestPingCameras_DoesNotHoldTheRegistryLock(t *testing.T) {
	lan := newAgent(t)

	reached := make(chan struct{})
	register(lan, "cam", &lockProbe{onPing: func() {
		// Taken from inside the ping, which deadlocks if the sweep still holds
		// it.
		lan.dataLock.Lock()
		lan.dataLock.Unlock()
		close(reached)
	}})

	done := make(chan struct{})
	go func() {
		defer close(done)
		lan.pingCameras()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep held the registry lock across a ping")
	}
	<-reached
}

// And the sweep is actually wired to the tick.
//
// Worth its own case because the three above call pingCameras directly: with
// the call removed from runTimers they all still pass, and the camera still
// never comes back. The loop is safe to run here -- a test agent has no
// interface registered, so the rescan branch iterates nothing.
func TestRunTimers_PingsOnTheCheckTick(t *testing.T) {
	lan, err := New(Config{CheckPeriod: 10 * time.Millisecond}, nil, testURL(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if cerr := lan.Close(); cerr != nil {
			t.Errorf("agent close: %v", cerr)
		}
	})

	ctl := &fakeController{}
	register(lan, "cam", ctl)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		lan.runTimers(ctx)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(ctl.seen()) > 0 {
			cancel()
			<-stopped
			return
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-stopped
	t.Fatal("the check tick came and went without nudging the camera")
}

// A controller whose Ping runs a function of the case's choosing.
type lockProbe struct {
	fakeController
	onPing func()
}

func (p *lockProbe) Ping() error {
	p.onPing()
	return nil
}
