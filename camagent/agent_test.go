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

package camagent_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juju/errors"
	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/camagent"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/mediabus"
	"github.com/ontavu/onvif/v2/sdk"
	"github.com/ontavu/onvif/v2/xsd"
	"github.com/ontavu/onvif/v2/xsd/onvif"
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/req"

	_ "go.nanomsg.org/mangos/v3/transport/inproc"
)

// fakeAppliance implements camagent.Appliance, which is the whole of what the
// agent uses of an ONVIF device: an identity and the media profiles on offer.
// It is deliberately not built on sdk.Appliance -- the SDK reaches the profiles
// through *sdk.ProfileS, a concrete type over an unexported client, so this
// fixture could not exist at that boundary.
type fakeAppliance struct {
	uuid string
	uri  string
}

var _ camagent.Appliance = (*fakeAppliance)(nil)

func (f *fakeAppliance) GetUUID() string { return f.uuid }

// MediaProfiles offers one H.264 profile carrying f.uri, which is what
// chooseProfile picks from. The agent asks for the profiles rather than for a
// stream URI, because the SDK's own FetchStreamURI returns just one of them.
func (f *fakeAppliance) MediaProfiles(_ context.Context) (sdk.MediaProfiles, error) {
	const token onvif.ReferenceToken = "main"

	profile := &sdk.MediaProfile{}
	profile.Profile.Token = token
	profile.Profile.VideoEncoderConfiguration.Encoding = "H264"
	profile.Profile.VideoEncoderConfiguration.Resolution.Width = 640
	profile.Profile.VideoEncoderConfiguration.Resolution.Height = 480
	profile.Uris.Stream.Uri = xsd.AnyURI(f.uri)

	return sdk.MediaProfiles{
		Profiles: map[onvif.ReferenceToken]*sdk.MediaProfile{token: profile},
	}, nil
}

// mediaEndpoint binds a puller that drains whatever the camera pushes, and
// returns the URL for the camera to connect to. A camera that never reaches a
// real RTSP server never pushes anything, but the endpoint has to exist.
func mediaEndpoint(t *testing.T) string {
	t.Helper()

	url := fmt.Sprintf("inproc://cams/test/media-%s-%d", t.Name(), time.Now().UnixNano())
	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("mediabus.ListenPull: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		puller.Serve(ctx, func(context.Context, mediabus.Frame) error { return nil })
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	return url
}

// newFixture starts an agent on a bus address unique to the test, and returns a
// controller for it. The stream URI points nowhere reachable and the agent is
// built with NoRetry, so a PLAY exercises the state machine and then gives up
// instead of dialling a real camera in a loop.
func newFixture(t *testing.T) (*camctrl.Client, *sync.WaitGroup, context.CancelFunc) {
	t.Helper()

	id := fmt.Sprintf("cam-%s-%d", t.Name(), time.Now().UnixNano())
	url := "inproc://cams/test/" + id

	appliance := &fakeAppliance{uuid: id, uri: "rtsp://127.0.0.1:1/nowhere"}

	agent, err := camagent.New(appliance, url, mediaEndpoint(t), camagent.NoRetry())
	if err != nil {
		t.Fatalf("camagent.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		agent.Run(ctx)
	}()

	ctl, err := camctrl.New(id, url, camctrl.WithTimeout(2*time.Second))
	if err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("camctrl.New: %v", err)
	}

	t.Cleanup(func() {
		if err := ctl.Close(); err != nil {
			t.Errorf("controller close: %v", err)
		}
		cancel()
		wg.Wait()
	})

	return ctl, wg, cancel
}

// TestAgent_DialBeforeRun pins the ordering that the whole factory depends on:
// New must bind, so a controller can be built before Run is ever scheduled.
func TestAgent_DialBeforeRun(t *testing.T) {
	url := "inproc://cams/test/dial-before-run"
	appliance := &fakeAppliance{uuid: "dial-before-run", uri: "rtsp://127.0.0.1:1/x"}

	agent, err := camagent.New(appliance, url, mediaEndpoint(t))
	if err != nil {
		t.Fatalf("camagent.New: %v", err)
	}
	defer func() {
		if err := agent.Close(); err != nil {
			t.Errorf("agent close: %v", err)
		}
	}()

	// Run has deliberately not been called.
	ctl, err := camctrl.New("dial-before-run", url)
	if err != nil {
		t.Fatalf("a controller could not dial an agent that is bound but not running: %v", err)
	}
	if err := ctl.Close(); err != nil {
		t.Errorf("controller close: %v", err)
	}
}

// TestAgent_DuplicateBind checks that two agents cannot share an endpoint. In
// cams-agent the address is derived from the camera UUID, so this is what turns
// a duplicate discovery into an error instead of a silent second listener.
func TestAgent_DuplicateBind(t *testing.T) {
	url := "inproc://cams/test/duplicate-bind"
	appliance := &fakeAppliance{uuid: "duplicate-bind", uri: "rtsp://127.0.0.1:1/x"}

	first, err := camagent.New(appliance, url, mediaEndpoint(t))
	if err != nil {
		t.Fatalf("first camagent.New: %v", err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Errorf("agent close: %v", err)
		}
	}()

	second, err := camagent.New(appliance, url, mediaEndpoint(t))
	if err == nil {
		_ = second.Close()
		t.Fatal("a second agent bound the same address")
	}
	if !errors.Is(err, mangos.ErrAddrInUse) {
		t.Fatalf("want ErrAddrInUse, got %v", err)
	}
}

func TestAgent_StateAfterStart(t *testing.T) {
	ctl, _, _ := newFixture(t)

	st, err := ctl.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != camctrl.StateIdle {
		t.Fatalf("State = %q, want IDLE", string(st))
	}
}

func TestAgent_PlayThenState(t *testing.T) {
	ctl, _, _ := newFixture(t)

	if err := ctl.Play(); err != nil {
		t.Fatalf("Play: %v", err)
	}

	st, err := ctl.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != camctrl.StatePlaying {
		t.Fatalf("State = %q, want PLAYING", string(st))
	}
}

// TestAgent_PlayIsIdempotent matters because the hub re-sends its expectations:
// a second PLAY must be accepted rather than upset the state machine.
func TestAgent_PlayIsIdempotent(t *testing.T) {
	ctl, _, _ := newFixture(t)

	for i := 0; i < 3; i++ {
		if err := ctl.Play(); err != nil {
			t.Fatalf("Play #%d: %v", i+1, err)
		}
	}

	st, err := ctl.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != camctrl.StatePlaying {
		t.Fatalf("State = %q, want PLAYING", string(st))
	}
}

func TestAgent_PauseAccepted(t *testing.T) {
	ctl, _, _ := newFixture(t)

	if err := ctl.Play(); err != nil {
		t.Fatalf("Play: %v", err)
	}
	if err := ctl.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// The camera leaves PAUSING for IDLE only once the media goroutines are
	// gone, and PING is what lets it notice. Either state is legitimate here.
	if err := ctl.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	st, err := ctl.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != camctrl.StateIdle && st != camctrl.StatePausing {
		t.Fatalf("State = %q, want IDLE or PAUSING", string(st))
	}
}

// TestAgent_PauseBeforePlay covers the camera that the hub never asks for and
// then pauses anyway. It used to be the shape that panicked, because the media
// group is nil until the first PLAY.
func TestAgent_PauseBeforePlay(t *testing.T) {
	ctl, _, _ := newFixture(t)

	if err := ctl.Pause(); err != nil {
		t.Fatalf("Pause before any Play: %v", err)
	}
	if err := ctl.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	st, err := ctl.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != camctrl.StateIdle {
		t.Fatalf("State = %q, want IDLE", string(st))
	}
}

func TestAgent_ExitStopsRun(t *testing.T) {
	id := fmt.Sprintf("cam-exit-%d", time.Now().UnixNano())
	url := "inproc://cams/test/" + id

	appliance := &fakeAppliance{uuid: id, uri: "rtsp://127.0.0.1:1/nowhere"}
	agent, err := camagent.New(appliance, url, mediaEndpoint(t), camagent.NoRetry())
	if err != nil {
		t.Fatalf("camagent.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		agent.Run(ctx)
	}()

	ctl, err := camctrl.New(id, url, camctrl.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("camctrl.New: %v", err)
	}
	defer func() {
		if err := ctl.Close(); err != nil {
			t.Errorf("controller close: %v", err)
		}
	}()

	// Exit is answered before the agent goes away, so this must not error.
	if err := ctl.Exit(); err != nil {
		t.Fatalf("Exit: %v", err)
	}

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after Exit")
	}
}

// TestAgent_ContextCancelStopsRun is the shutdown path of a camera that never
// played: the media group is nil, and tearing it down must not panic.
func TestAgent_ContextCancelStopsRun(t *testing.T) {
	id := fmt.Sprintf("cam-cancel-%d", time.Now().UnixNano())
	url := "inproc://cams/test/" + id

	appliance := &fakeAppliance{uuid: id, uri: "rtsp://127.0.0.1:1/nowhere"}
	agent, err := camagent.New(appliance, url, mediaEndpoint(t), camagent.NoRetry())
	if err != nil {
		t.Fatalf("camagent.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		agent.Run(ctx)
	}()

	ctl, err := camctrl.New(id, url, camctrl.WithTimeout(2*time.Second))
	if err != nil {
		cancel()
		t.Fatalf("camctrl.New: %v", err)
	}
	// Make sure Run is serving before cancelling it.
	if _, err := ctl.State(); err != nil {
		cancel()
		t.Fatalf("State: %v", err)
	}
	if err := ctl.Close(); err != nil {
		t.Errorf("controller close: %v", err)
	}

	cancel()

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// TestAgent_UnknownVerbRefused checks that a request the agent does not know is
// answered rather than dropped, and that the exchange survives it. It bypasses
// camctrl.Client on purpose: the typed API cannot produce a bad verb.
func TestAgent_UnknownVerbRefused(t *testing.T) {
	id := fmt.Sprintf("cam-verb-%d", time.Now().UnixNano())
	url := "inproc://cams/test/" + id

	appliance := &fakeAppliance{uuid: id, uri: "rtsp://127.0.0.1:1/nowhere"}
	agent, err := camagent.New(appliance, url, mediaEndpoint(t), camagent.NoRetry())
	if err != nil {
		t.Fatalf("camagent.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		agent.Run(ctx)
	}()
	defer func() {
		cancel()
		wg.Wait()
	}()

	sock, err := req.NewSocket()
	if err != nil {
		t.Fatalf("req socket: %v", err)
	}
	defer func() {
		if err := sock.Close(); err != nil {
			t.Errorf("socket close: %v", err)
		}
	}()
	if err := sock.SetOption(mangos.OptionRetryTime, time.Duration(0)); err != nil {
		t.Fatalf("set retry: %v", err)
	}
	if err := sock.SetOption(mangos.OptionRecvDeadline, 2*time.Second); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if err := sock.Dial(url); err != nil {
		t.Fatalf("dial: %v", err)
	}

	if err := sock.Send([]byte("PIROUETTE")); err != nil {
		t.Fatalf("send: %v", err)
	}
	reply, err := sock.Recv()
	if err != nil {
		t.Fatalf("an unknown verb went unanswered: %v", err)
	}
	if _, err := agentbus.DecodeReply(reply); err == nil {
		t.Fatalf("an unknown verb was accepted, reply %q", string(reply))
	}

	// The agent has to still be serving afterwards.
	ctl, err := camctrl.New(id, url, camctrl.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("camctrl.New after a bad verb: %v", err)
	}
	defer func() {
		if err := ctl.Close(); err != nil {
			t.Errorf("controller close: %v", err)
		}
	}()
	if _, err := ctl.State(); err != nil {
		t.Fatalf("the agent stopped serving after a bad verb: %v", err)
	}
}

// TestAgent_ConcurrentControllers is the reason each request opens its own
// mangos context. Sharing the socket's default context would make one of these
// callers fail with ErrCanceled even though the agent had run its command.
func TestAgent_ConcurrentControllers(t *testing.T) {
	ctl, _, _ := newFixture(t)

	const callers = 8
	const rounds = 5

	var failures atomic.Uint32
	start := make(chan struct{})
	wg := sync.WaitGroup{}

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for r := 0; r < rounds; r++ {
				if _, err := ctl.State(); err != nil {
					failures.Add(1)
					t.Errorf("State: %v", err)
					return
				}
				if err := ctl.Ping(); err != nil {
					failures.Add(1)
					t.Errorf("Ping: %v", err)
					return
				}
			}
		}()
	}

	close(start)
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Fatalf("%d concurrent callers failed", n)
	}
}

// A registered camera nobody has asked to play has never described a stream, so
// it has nothing to report. That is an ordinary state, not an error: the hub
// reads it as "not known yet".
func TestMedia_UnknownBeforeAnyPlay(t *testing.T) {
	cli, wg, cancel := newFixture(t)
	defer func() {
		cancel()
		wg.Wait()
	}()

	got, err := cli.Media()
	if err != nil {
		t.Fatalf("Media: %v", err)
	}
	if !got.Unknown() {
		t.Fatalf("a camera that never streamed reports %+v", got)
	}
}

// After a PLAY the agent has chosen a profile, and it keeps what it observed
// even once the attempt has failed and the camera is idle again: the encoding
// of a camera does not change while it runs.
func TestMedia_RememberedAfterAPlay(t *testing.T) {
	cli, wg, cancel := newFixture(t)
	defer func() {
		cancel()
		wg.Wait()
	}()

	if err := cli.Play(); err != nil {
		t.Fatalf("Play: %v", err)
	}

	// The stream URI points nowhere, so the attempt fails; the profile is
	// chosen before the RTSP dial, which is what this waits for.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := cli.Media()
		if err != nil {
			t.Fatalf("Media: %v", err)
		}
		if !got.Unknown() {
			// fakeAppliance offers one H.264 profile at 640x480.
			want := camctrl.Media{
				Encoding: camctrl.EncodingH264,
				Width:    640,
				Height:   480,
			}
			if got != want {
				t.Fatalf("reported %+v, want %+v", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never reported what it chose")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
