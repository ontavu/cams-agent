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

package upagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/lanctrl"
	"github.com/ontavu/cams-agent/upctrl"
	"github.com/ontavu/cams-api/pb"
	"github.com/ontavu/cams-api/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// fakeLan stands in for lanctrl.Client. Standing it in is the point of the
// LanController interface: no socket to the LAN agent is involved here.
type fakeLan struct {
	mutex  sync.Mutex
	ids    []string
	calls  []string
	failOn string
	media  map[string]camctrl.Media
}

func (f *fakeLan) record(name string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.calls = append(f.calls, name)
	if f.failOn == name {
		return errors.New("fake failure on " + name)
	}
	return nil
}

func (f *fakeLan) seen() []string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeLan) Media(camID string) (camctrl.Media, error) {
	if err := f.record("Media"); err != nil {
		return camctrl.Media{}, err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	m, known := f.media[camID]
	if !known {
		// What a camera nobody has played reports.
		return camctrl.Media{}, nil
	}
	return m, nil
}

func (f *fakeLan) List() ([]string, error) {
	if err := f.record("List"); err != nil {
		return nil, err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]string(nil), f.ids...), nil
}

func (f *fakeLan) Play(camID string) error  { return f.record("Play " + camID) }
func (f *fakeLan) Pause(camID string) error { return f.record("Pause " + camID) }

// fakeStream replays a canned sequence of hub orders and then fails, the way a
// real stream does when the connection ends.
type fakeStream struct {
	requests []*pb.DownstreamControlRequest
	pos      int
	err      error
}

func (f *fakeStream) Recv() (*pb.DownstreamControlRequest, error) {
	if f.pos >= len(f.requests) {
		if f.err != nil {
			return nil, f.err
		}
		return nil, io.EOF
	}
	req := f.requests[f.pos]
	f.pos++
	return req, nil
}

// fakeRegistrar records what was announced to the hub.
type fakeRegistrar struct {
	registered []string
	users      []string
	sessions   []string
	requests   []*pb.RegisterRequest
	err        error
}

func (f *fakeRegistrar) Register(ctx context.Context, in *pb.RegisterRequest, _ ...grpc.CallOption) (*pb.None, error) {
	// The metadata is read even on the failing path: what the hub receives is
	// a property of the call, not of its outcome.
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		f.sessions = append(f.sessions, strings.Join(md.Get(wire.KeySession), ","))
	} else {
		f.sessions = append(f.sessions, "")
	}
	if f.err != nil {
		return nil, f.err
	}
	f.registered = append(f.registered, in.Id.Stream)
	f.users = append(f.users, in.Id.User)
	f.requests = append(f.requests, in)
	return &pb.None{}, nil
}

func testURL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("inproc://cams/test/up-%s-%d", t.Name(), time.Now().UnixNano())
}

// newAgent builds an agent whose endpoint nobody serves, for the tests that
// exercise the Go API directly.
func newAgent(t *testing.T, lan lanctrl.LanController) *Agent {
	t.Helper()

	us, err := New(Config{User: "someone"}, lan, testURL(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if err := us.Close(); err != nil {
			t.Errorf("agent close: %v", err)
		}
	})
	return us
}

func play(camID string) *pb.DownstreamControlRequest {
	return &pb.DownstreamControlRequest{
		StreamID: camID,
		Command:  pb.DownstreamCommandType_DOWNSTREAM_COMMAND_TYPE_PLAY,
	}
}

func stop(camID string) *pb.DownstreamControlRequest {
	return &pb.DownstreamControlRequest{
		StreamID: camID,
		Command:  pb.DownstreamCommandType_DOWNSTREAM_COMMAND_TYPE_STOP,
	}
}

// ---------- the hub's orders ----------

func TestOnCommand_Forwarded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request *pb.DownstreamControlRequest
		want    string
	}{
		{"play", play("cam-1"), "Play cam-1"},
		{"stop", stop("cam-1"), "Pause cam-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lan := &fakeLan{}
			us := newAgent(t, lan)

			if err := us.onCommand(tc.request); err != nil {
				t.Fatalf("onCommand: %v", err)
			}
			if got := lan.seen(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("LAN controller saw %v, want [%s]", got, tc.want)
			}
		})
	}
}

func TestOnCommand_UnknownType(t *testing.T) {
	lan := &fakeLan{}
	us := newAgent(t, lan)

	request := &pb.DownstreamControlRequest{
		StreamID: "cam-1",
		Command:  pb.DownstreamCommandType(42),
	}
	if err := us.onCommand(request); err == nil {
		t.Fatal("an unknown hub command was accepted")
	}
	if got := lan.seen(); len(got) != 0 {
		t.Fatalf("LAN controller saw %v, want nothing", got)
	}
}

func TestOnCommand_ReportsFailure(t *testing.T) {
	lan := &fakeLan{failOn: "Play cam-1"}
	us := newAgent(t, lan)

	if err := us.onCommand(play("cam-1")); err == nil {
		t.Fatal("a refused Play was reported as a success")
	}
}

// TestReadControl_DispatchesInOrder checks the loop as a whole: every order
// reaches the LAN, in the order the hub sent them, and the end of the stream is
// what ends the loop.
func TestReadControl_DispatchesInOrder(t *testing.T) {
	lan := &fakeLan{}
	us := newAgent(t, lan)

	stream := &fakeStream{requests: []*pb.DownstreamControlRequest{
		play("cam-1"), play("cam-2"), stop("cam-1"),
	}}

	err := us.readControl(context.Background(), stream)
	if err == nil {
		t.Fatal("readControl returned no error when the stream ended")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("readControl returned %v, want the stream error", err)
	}

	want := []string{"Play cam-1", "Play cam-2", "Pause cam-1"}
	got := lan.seen()
	if len(got) != len(want) {
		t.Fatalf("LAN controller saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("LAN controller saw %v, want %v", got, want)
		}
	}
}

// TestReadControl_SurvivesARefusal is the behaviour change worth pinning: one
// camera refusing must not cost every other camera its control channel.
func TestReadControl_SurvivesARefusal(t *testing.T) {
	lan := &fakeLan{failOn: "Play cam-1"}
	us := newAgent(t, lan)

	stream := &fakeStream{requests: []*pb.DownstreamControlRequest{
		play("cam-1"), play("cam-2"),
	}}

	if err := us.readControl(context.Background(), stream); !errors.Is(err, io.EOF) {
		t.Fatalf("readControl returned %v, want the stream error", err)
	}

	// The second order has to have been attempted despite the first failing.
	got := lan.seen()
	if len(got) != 2 || got[1] != "Play cam-2" {
		t.Fatalf("LAN controller saw %v, want the second order attempted", got)
	}
}

// TestReadControl_UnknownTypeSurvives covers a hub of a newer revision sending
// a command this build does not know.
func TestReadControl_UnknownTypeSurvives(t *testing.T) {
	lan := &fakeLan{}
	us := newAgent(t, lan)

	stream := &fakeStream{requests: []*pb.DownstreamControlRequest{
		{StreamID: "cam-1", Command: pb.DownstreamCommandType(99)},
		play("cam-2"),
	}}

	if err := us.readControl(context.Background(), stream); !errors.Is(err, io.EOF) {
		t.Fatalf("readControl returned %v, want the stream error", err)
	}
	if got := lan.seen(); len(got) != 1 || got[0] != "Play cam-2" {
		t.Fatalf("LAN controller saw %v, want only the known order", got)
	}
}

// ---------- registration ----------

func TestRegisterAll_EveryCamera(t *testing.T) {
	lan := &fakeLan{ids: []string{"cam-a", "cam-b"}}
	us := newAgent(t, lan)

	reg := &fakeRegistrar{}
	if err := us.registerAll(context.Background(), reg); err != nil {
		t.Fatalf("registerAll: %v", err)
	}

	if len(reg.registered) != 2 || reg.registered[0] != "cam-a" || reg.registered[1] != "cam-b" {
		t.Fatalf("registered %v, want [cam-a cam-b]", reg.registered)
	}
	for i, user := range reg.users {
		if user != "someone" {
			t.Fatalf("registration %d carried user %q, want %q", i, user, "someone")
		}
	}
}

// TestRegisterAll_CarriesTheSession is the join column between this agent's
// logs and the hub's. registerAll builds its metadata with NewOutgoingContext,
// which replaces the block, so the tag is the one thing there that a plausible
// reordering would silently drop.
func TestRegisterAll_CarriesTheSession(t *testing.T) {
	lan := &fakeLan{ids: []string{"cam-a"}}
	us := newAgent(t, lan)

	reg := &fakeRegistrar{}
	if err := us.registerAll(context.Background(), reg); err != nil {
		t.Fatalf("registerAll: %v", err)
	}

	if len(reg.sessions) != 1 {
		t.Fatalf("%d registrations, want 1", len(reg.sessions))
	}
	if reg.sessions[0] != wire.SessionID {
		t.Fatalf("registration carried session %q, want %q", reg.sessions[0], wire.SessionID)
	}
	if len(reg.users) != 1 || reg.users[0] != "someone" {
		t.Fatalf("the user was lost alongside it: %v", reg.users)
	}
}

func TestRegisterAll_NoCamera(t *testing.T) {
	us := newAgent(t, &fakeLan{})

	reg := &fakeRegistrar{}
	if err := us.registerAll(context.Background(), reg); err != nil {
		t.Fatalf("registerAll: %v", err)
	}
	if len(reg.registered) != 0 {
		t.Fatalf("registered %v with no camera known", reg.registered)
	}
}

// TestRegisterAll_LanFailureSkipsTheRound is the second behaviour change: a LAN
// that cannot be asked costs one round, not the hub connection.
func TestRegisterAll_LanFailureSkipsTheRound(t *testing.T) {
	us := newAgent(t, &fakeLan{failOn: "List"})

	reg := &fakeRegistrar{}
	if err := us.registerAll(context.Background(), reg); err != nil {
		t.Fatalf("a LAN failure ended the connection: %v", err)
	}
	if len(reg.registered) != 0 {
		t.Fatalf("registered %v after a LAN failure", reg.registered)
	}
}

// TestRegisterAll_HubFailureIsFatal is the half that stays fatal: a hub
// refusing a registration is evidence the link is bad.
func TestRegisterAll_HubFailureIsFatal(t *testing.T) {
	us := newAgent(t, &fakeLan{ids: []string{"cam-a"}})

	reg := &fakeRegistrar{err: errors.New("hub is unwell")}
	if err := us.registerAll(context.Background(), reg); err == nil {
		t.Fatal("a hub refusing a registration was reported as a success")
	}
}

// ---------- over the bus ----------

// serveAgent starts the bus loop of an agent and returns a controller for it.
func serveAgent(t *testing.T) (*Agent, upctrl.UpstreamController) {
	t.Helper()

	url := testURL(t)
	us, err := New(Config{User: "someone"}, &fakeLan{}, url)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		us.bus.Serve(ctx, us.dispatch)
	}()

	cli, err := upctrl.New(url, upctrl.WithTimeout(2*time.Second))
	if err != nil {
		cancel()
		<-served
		t.Fatalf("upctrl.New: %v", err)
	}

	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Errorf("controller close: %v", err)
		}
		cancel()
		<-served
	})

	return us, cli
}

func TestBus_Ping(t *testing.T) {
	_, cli := serveAgent(t)

	if err := cli.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestBus_StateDownBeforeConnecting is why the bus is served beside the
// connection attempts rather than inside one: the state is worth asking for
// precisely when the hub is unreachable.
func TestBus_StateDownBeforeConnecting(t *testing.T) {
	_, cli := serveAgent(t)

	got, err := cli.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != upctrl.StateDown {
		t.Fatalf("State = %q, want DOWN", string(got))
	}
}

func TestBus_StateFollowsTheLink(t *testing.T) {
	us, cli := serveAgent(t)

	us.up.Store(true)
	got, err := cli.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != upctrl.StateUp {
		t.Fatalf("State = %q, want UP", string(got))
	}

	us.up.Store(false)
	got, err = cli.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if got != upctrl.StateDown {
		t.Fatalf("State = %q, want DOWN", string(got))
	}
}

func TestBus_UnknownVerbRefused(t *testing.T) {
	us, cli := serveAgent(t)

	reply, exit := us.dispatch(context.Background(), "PIROUETTE", "")
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

// TestBus_ArgumentRefused pins that neither verb takes one, rather than
// quietly ignoring whatever arrives.
func TestBus_ArgumentRefused(t *testing.T) {
	us, _ := serveAgent(t)

	for _, verb := range []string{string(upctrl.CommandPing), string(upctrl.CommandState)} {
		reply, _ := us.dispatch(context.Background(), verb, "unexpected")
		if _, err := decodeForTest(reply); err == nil {
			t.Fatalf("%s accepted an argument", verb)
		}
	}
}

// decodeForTest reads a reply the way a controller would, so that a test can
// assert on what dispatch produced without going through a socket.
func decodeForTest(reply []byte) (string, error) {
	return agentbus.DecodeReply(reply)
}

// blockingStream never yields an order and never fails, the way a healthy hub
// behaves when no viewer is asking for anything. It unblocks only when its
// context is cancelled, which is what a real gRPC client stream does.
type blockingStream struct {
	ctx      context.Context
	recvs    atomic.Uint32
	released chan struct{}
}

func newBlockingStream(ctx context.Context) *blockingStream {
	return &blockingStream{ctx: ctx, released: make(chan struct{})}
}

func (b *blockingStream) Recv() (*pb.DownstreamControlRequest, error) {
	b.recvs.Add(1)
	<-b.ctx.Done()
	close(b.released)
	return nil, b.ctx.Err()
}

// TestReadControl_ReleasedByTheStreamContext is the regression test for the
// wedge: the reader parks on a healthy stream, and the only thing that can free
// it is the context the stream was opened with. Before the fix the members ran
// under a context that could not reach the stream, so a registrar failure left
// this goroutine parked for the lifetime of the process -- no reconnect, and
// STATE answering UP forever.
func TestReadControl_ReleasedByTheStreamContext(t *testing.T) {
	us := newAgent(t, &fakeLan{})

	streamCtx, stopStream := context.WithCancel(context.Background())
	stream := newBlockingStream(streamCtx)

	done := make(chan error, 1)
	go func() {
		// The context passed here is deliberately a different one, to prove it
		// is not what releases the reader.
		done <- us.readControl(context.Background(), stream)
	}()

	// Let the reader reach Recv.
	deadline := time.Now().Add(5 * time.Second)
	for stream.recvs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stream.recvs.Load() == 0 {
		stopStream()
		t.Fatal("the reader never reached Recv")
	}

	select {
	case err := <-done:
		stopStream()
		t.Fatalf("readControl returned %v before anything cancelled the stream", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Cancelling the stream's own context is what has to free it.
	stopStream()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readControl returned no error after the stream was cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the stream context did not release the reader")
	}

	select {
	case <-stream.released:
	case <-time.After(5 * time.Second):
		t.Fatal("Recv never returned")
	}
}

// TestRun_IsASingleton pins the guard the doc comment promises. It had none:
// the comment claimed "one agent, one Run" while nothing enforced it, so two
// Runs would have put two serving loops on one REP socket, each receiving and
// each replying, and either could close the socket under the other.
func TestRun_IsASingleton(t *testing.T) {
	us := newAgent(t, &fakeLan{})

	// Standing in for a Run already in progress, deterministically.
	if !us.singletonLock.TryLock() {
		t.Fatal("a fresh agent already holds its singleton lock")
	}
	defer us.singletonLock.Unlock()

	defer func() {
		if recover() == nil {
			t.Error("a second Run was allowed")
		}
	}()
	us.Run(context.Background())
}

// What the hub needs before it commits a pipeline to a camera.
func TestRegisterAll_CarriesTheMedia(t *testing.T) {
	lan := &fakeLan{
		ids: []string{"cam-h264", "cam-h265", "cam-idle"},
		media: map[string]camctrl.Media{
			"cam-h264": {Encoding: camctrl.EncodingH264, Width: 640, Height: 480, GopLength: 30},
			"cam-h265": {Encoding: camctrl.EncodingH265, Width: 2560, Height: 1920},
			// cam-idle is absent: a camera nobody has asked to play.
		},
	}
	us := newAgent(t, lan)

	reg := &fakeRegistrar{}
	if err := us.registerAll(context.Background(), reg); err != nil {
		t.Fatalf("registerAll: %v", err)
	}
	if len(reg.requests) != 3 {
		t.Fatalf("registered %d cameras, want 3", len(reg.requests))
	}

	for _, tc := range []struct {
		at       int
		encoding pb.VideoEncoding
		width    uint32
		height   uint32
		gop      uint32
	}{
		{0, pb.VideoEncoding_VIDEO_ENCODING_H264, 640, 480, 30},
		{1, pb.VideoEncoding_VIDEO_ENCODING_H265, 2560, 1920, 0},
		// Never played, so nothing is known -- and never "no video".
		{2, pb.VideoEncoding_VIDEO_ENCODING_UNSPECIFIED, 0, 0, 0},
	} {
		got := reg.requests[tc.at]
		if got.Encoding != tc.encoding {
			t.Fatalf("%s carried encoding %v, want %v", got.Id.Stream, got.Encoding, tc.encoding)
		}
		if got.Width != tc.width || got.Height != tc.height {
			t.Fatalf("%s carried %dx%d, want %dx%d", got.Id.Stream,
				got.Width, got.Height, tc.width, tc.height)
		}
		if got.GopLength != tc.gop {
			t.Fatalf("%s carried a GOP of %d, want %d", got.Id.Stream, got.GopLength, tc.gop)
		}
	}
}

// A LAN agent of an older revision does not understand the question. Losing the
// registration over it would cost the hub the camera altogether, which is worse
// than not knowing what the camera produces.
func TestRegisterAll_MediaFailureStillRegisters(t *testing.T) {
	lan := &fakeLan{ids: []string{"cam-a"}, failOn: "Media"}
	us := newAgent(t, lan)

	reg := &fakeRegistrar{}
	if err := us.registerAll(context.Background(), reg); err != nil {
		t.Fatalf("registerAll: %v", err)
	}
	if len(reg.requests) != 1 {
		t.Fatalf("registered %d cameras, want 1", len(reg.requests))
	}
	if got := reg.requests[0].Encoding; got != pb.VideoEncoding_VIDEO_ENCODING_UNSPECIFIED {
		t.Fatalf("carried encoding %v, want UNSPECIFIED", got)
	}
}

func TestEncodingToPB(t *testing.T) {
	for _, tc := range []struct {
		in   camctrl.Encoding
		want pb.VideoEncoding
	}{
		{camctrl.EncodingH264, pb.VideoEncoding_VIDEO_ENCODING_H264},
		{camctrl.EncodingH265, pb.VideoEncoding_VIDEO_ENCODING_H265},
		{camctrl.EncodingJPEG, pb.VideoEncoding_VIDEO_ENCODING_JPEG},
		{camctrl.EncodingUnspecified, pb.VideoEncoding_VIDEO_ENCODING_UNSPECIFIED},
		{"", pb.VideoEncoding_VIDEO_ENCODING_UNSPECIFIED},
		// Something a newer agent reports.
		{camctrl.Encoding("AV1"), pb.VideoEncoding_VIDEO_ENCODING_UNSPECIFIED},
	} {
		if got := encodingToPB(tc.in); got != tc.want {
			t.Fatalf("encodingToPB(%q) = %v, want %v", string(tc.in), got, tc.want)
		}
	}
}
