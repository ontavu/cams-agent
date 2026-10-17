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

package mediabus_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ontavu/cams-agent/mediabus"
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/push"

	_ "go.nanomsg.org/mangos/v3/transport/inproc"
)

// ---------- framing ----------

func TestFrame_RoundTrip(t *testing.T) {
	for _, ft := range []mediabus.FrameType{
		mediabus.FrameSDP, mediabus.FrameRTP, mediabus.FrameRTCP,
	} {
		t.Run(ft.String(), func(t *testing.T) {
			for _, payload := range [][]byte{
				{},
				[]byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"),
				{0x80, 0x60, 0x00, 0x01, 0xff, 0x00},
			} {
				for _, track := range []byte{0, 1, 7, 0xff} {
					got, err := mediabus.DecodeFrame(mediabus.EncodeFrame(ft, track, payload))
					if err != nil {
						t.Fatalf("DecodeFrame: %v", err)
					}
					if got.Type != ft {
						t.Fatalf("type = %v, want %v", got.Type, ft)
					}
					if got.Track != track {
						t.Fatalf("track = %d, want %d", got.Track, track)
					}
					if !bytes.Equal(got.Payload, payload) {
						t.Fatalf("payload = %v, want %v", got.Payload, payload)
					}
				}
			}
		})
	}
}

// TestDecodeFrame_Rejected is the reason zero is not a valid type: an empty or
// truncated message must be an error rather than an accidental banner. The
// header is two bytes, so a lone valid type is truncated as well.
func TestDecodeFrame_Rejected(t *testing.T) {
	for _, msg := range [][]byte{
		nil,
		{},
		{0},             // the zero type
		{4},             // past the last defined type
		{1},             // a valid type with no track byte
		{0, 0},          // the zero type, full header
		{0xff, 0, 1, 2}, // nonsense with a payload
	} {
		if f, err := mediabus.DecodeFrame(msg); err == nil {
			t.Fatalf("DecodeFrame(%v) = %v, want an error", msg, f.Type)
		}
	}
}

// TestDecodeFrame_EmptyPayload pins that a frame carrying nothing but its
// header is valid: the payload is what may be empty, not the header.
func TestDecodeFrame_EmptyPayload(t *testing.T) {
	f, err := mediabus.DecodeFrame([]byte{byte(mediabus.FrameRTP), 3})
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.Type != mediabus.FrameRTP || f.Track != 3 || len(f.Payload) != 0 {
		t.Fatalf("frame = %v/%d/%v", f.Type, f.Track, f.Payload)
	}
}

func TestFrameType_Valid(t *testing.T) {
	for _, ft := range []mediabus.FrameType{mediabus.FrameSDP, mediabus.FrameRTP, mediabus.FrameRTCP} {
		if !ft.Valid() {
			t.Fatalf("%v reported invalid", ft)
		}
		if ft.String() == "INVALID" {
			t.Fatalf("%d has no name", byte(ft))
		}
	}
	for _, ft := range []mediabus.FrameType{0, 4, 200} {
		if ft.Valid() {
			t.Fatalf("%d reported valid", byte(ft))
		}
	}
}

func TestIsOverflow(t *testing.T) {
	if !mediabus.IsOverflow(fmt.Errorf("push RTP: %w", mangos.ErrSendTimeout)) {
		t.Fatal("an annotated send timeout is not recognised as an overflow")
	}
	for _, err := range []error{
		nil,
		errors.New("something else"),
		fmt.Errorf("push RTP: %w", mangos.ErrClosed),
		fmt.Errorf("push RTP: %w", mangos.ErrNoPeers),
	} {
		if mediabus.IsOverflow(err) {
			t.Fatalf("%v mistaken for an overflow", err)
		}
	}
}

// ---------- transport ----------

func testURL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("inproc://cams/test/media-%s-%d", t.Name(), time.Now().UnixNano())
}

type collected struct {
	ft      mediabus.FrameType
	track   byte
	payload string
}

// serve starts a puller feeding a channel, and returns a pusher aimed at it.
func serve(t *testing.T, depth int) (*mediabus.Pusher, <-chan collected) {
	t.Helper()

	url := testURL(t)
	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("ListenPull: %v", err)
	}

	out := make(chan collected, depth)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		puller.Serve(ctx, func(_ context.Context, f mediabus.Frame) error {
			// Copied: the payload only lives until the handler returns.
			out <- collected{f.Type, f.Track, string(f.Payload)}
			return nil
		})
	}()

	pusher, err := mediabus.DialPush("test", url)
	if err != nil {
		cancel()
		<-served
		t.Fatalf("DialPush: %v", err)
	}

	t.Cleanup(func() {
		if err := pusher.Close(); err != nil {
			t.Errorf("pusher close: %v", err)
		}
		cancel()
		<-served
	})

	return pusher, out
}

func TestPushPull_RoundTrip(t *testing.T) {
	pusher, out := serve(t, 8)

	sent := []collected{
		{mediabus.FrameSDP, 0, "v=0"},
		{mediabus.FrameRTP, 0, "\x80\x60rtp"},
		{mediabus.FrameRTCP, 0, "\x80\xc8rtcp"},
		// A second media, to prove the track is not fixed at zero.
		{mediabus.FrameRTP, 1, "\x80\x61rtp"},
		{mediabus.FrameRTCP, 1, "\x80\xc9rtcp"},
	}
	for _, f := range sent {
		if err := pusher.Send(f.ft, f.track, []byte(f.payload)); err != nil {
			t.Fatalf("Send(%v): %v", f.ft, err)
		}
	}

	// Order is preserved with a single peer, which the camera relies on: the
	// banner has to reach the upstream before the packets it describes.
	for i, want := range sent {
		select {
		case got := <-out:
			if got != want {
				t.Fatalf("frame %d = %v, want %v", i, got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("frame %d never arrived", i)
		}
	}
}

func TestPusher_RejectsBadType(t *testing.T) {
	pusher, _ := serve(t, 1)

	if err := pusher.Send(mediabus.FrameType(0), 0, []byte("x")); err == nil {
		t.Fatal("the zero frame type was accepted")
	}
	if err := pusher.Send(mediabus.FrameType(99), 0, []byte("x")); err == nil {
		t.Fatal("an undefined frame type was accepted")
	}
}

// TestPusher_Concurrent is the property that let the camera drop its channels:
// the RTP and RTCP callbacks push from different goroutines onto one socket.
func TestPusher_Concurrent(t *testing.T) {
	const perKind = 300

	pusher, out := serve(t, 4*perKind)

	drained := make(chan map[mediabus.FrameType]int)
	go func() {
		counts := map[mediabus.FrameType]int{}
		for i := 0; i < 2*perKind; i++ {
			c := <-out
			counts[c.ft]++
		}
		drained <- counts
	}()

	var failures atomic.Uint32
	wg := sync.WaitGroup{}
	for _, ft := range []mediabus.FrameType{mediabus.FrameRTP, mediabus.FrameRTCP} {
		wg.Add(1)
		go func(ft mediabus.FrameType) {
			defer wg.Done()
			for i := 0; i < perKind; i++ {
				if err := pusher.Send(ft, 0, []byte(ft.String())); err != nil {
					failures.Add(1)
					t.Errorf("Send(%v): %v", ft, err)
					return
				}
			}
		}(ft)
	}
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Fatalf("%d concurrent senders failed", n)
	}

	select {
	case counts := <-drained:
		if counts[mediabus.FrameRTP] != perKind || counts[mediabus.FrameRTCP] != perKind {
			t.Fatalf("received %v, want %d of each", counts, perKind)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the frames never all arrived")
	}
}

// TestPusher_OverflowIsReported pins the drop policy: a puller that stops
// consuming makes the queues fill, and the sender is told so rather than
// blocking forever or silently discarding.
func TestPusher_OverflowIsReported(t *testing.T) {
	url := testURL(t)
	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("ListenPull: %v", err)
	}

	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		puller.Serve(ctx, func(context.Context, mediabus.Frame) error {
			<-release
			return nil
		})
	}()
	defer func() {
		close(release)
		cancel()
		<-served
	}()

	pusher, err := mediabus.DialPush("test", url,
		mediabus.WithQueueDepth(8),
		mediabus.WithSendTimeout(5*time.Millisecond))
	if err != nil {
		t.Fatalf("DialPush: %v", err)
	}
	defer func() {
		if err := pusher.Close(); err != nil {
			t.Errorf("pusher close: %v", err)
		}
	}()

	// The queues are small and nothing is being consumed, so this must start
	// failing well before the loop ends.
	var overflow int
	for i := 0; i < 8+mediabus.DefaultReceiveDepth+64; i++ {
		err := pusher.Send(mediabus.FrameRTP, 0, []byte("packet"))
		if err == nil {
			continue
		}
		if !mediabus.IsOverflow(err) {
			t.Fatalf("send %d failed with %v, want an overflow", i, err)
		}
		overflow++
	}

	if overflow == 0 {
		t.Fatal("a stalled puller never produced an overflow, the queues are not bounded")
	}
}

// TestPusher_UpstreamGone checks that a stopped upstream is reported as
// something other than an overflow, which is how the camera tells "too slow"
// from "gone" and ends the attempt.
func TestPusher_UpstreamGone(t *testing.T) {
	url := testURL(t)
	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("ListenPull: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		puller.Serve(ctx, func(context.Context, mediabus.Frame) error { return nil })
	}()

	pusher, err := mediabus.DialPush("test", url)
	if err != nil {
		cancel()
		<-served
		t.Fatalf("DialPush: %v", err)
	}
	defer func() {
		if err := pusher.Close(); err != nil {
			t.Errorf("pusher close: %v", err)
		}
	}()

	cancel()
	<-served

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		err := pusher.Send(mediabus.FrameRTP, 0, []byte("packet"))
		if err == nil {
			continue
		}
		if mediabus.IsOverflow(err) {
			t.Fatalf("a stopped upstream was reported as an overflow: %v", err)
		}
		return // a non-overflow failure is what the camera acts on
	}
	t.Fatal("pushing into a stopped upstream never failed")
}

// TestDialPush_NoUpstream is why the dial is synchronous: a camera has to learn
// at once that there is nothing to push to.
func TestDialPush_NoUpstream(t *testing.T) {
	if pusher, err := mediabus.DialPush("test", testURL(t)); err == nil {
		_ = pusher.Close()
		t.Fatal("dialling an address nobody serves succeeded")
	}
}

func TestListenPull_DuplicateAddress(t *testing.T) {
	url := testURL(t)

	first, err := mediabus.ListenPull("first", url)
	if err != nil {
		t.Fatalf("first ListenPull: %v", err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Errorf("puller close: %v", err)
		}
	}()

	second, err := mediabus.ListenPull("second", url)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second puller bound the same address")
	}
	if !errors.Is(err, mangos.ErrAddrInUse) {
		t.Fatalf("want ErrAddrInUse, got %v", err)
	}
}

// TestListenPull_BeforeServe is the ordering the camera factory relies on: the
// upstream binds when it is built, so a camera may connect before it is served.
func TestListenPull_BeforeServe(t *testing.T) {
	url := testURL(t)

	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("ListenPull: %v", err)
	}
	defer func() {
		if err := puller.Close(); err != nil {
			t.Errorf("puller close: %v", err)
		}
	}()

	// Serve has deliberately not been called.
	pusher, err := mediabus.DialPush("test", url)
	if err != nil {
		t.Fatalf("a pusher could not dial a bound but idle puller: %v", err)
	}
	if err := pusher.Close(); err != nil {
		t.Errorf("pusher close: %v", err)
	}
}

func TestServe_StopsOnCancel(t *testing.T) {
	url := testURL(t)
	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("ListenPull: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		puller.Serve(ctx, func(context.Context, mediabus.Frame) error { return nil })
	}()

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

// TestServe_StopsOnHandlerError checks that a failing upstream takes the puller
// down, which is what makes the camera's next send fail instead of piling up
// frames nobody reads.
func TestServe_StopsOnHandlerError(t *testing.T) {
	url := testURL(t)
	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("ListenPull: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		puller.Serve(ctx, func(context.Context, mediabus.Frame) error {
			return errors.New("upstream is unwell")
		})
	}()

	pusher, err := mediabus.DialPush("test", url)
	if err != nil {
		t.Fatalf("DialPush: %v", err)
	}
	defer func() {
		if err := pusher.Close(); err != nil {
			t.Errorf("pusher close: %v", err)
		}
	}()

	if err := pusher.Send(mediabus.FrameRTP, 0, []byte("packet")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the handler failed")
	}
}

// TestServe_SurvivesAMalformedFrame goes under the Pusher, which cannot produce
// an invalid frame. One bad message must not cost the whole stream.
func TestServe_SurvivesAMalformedFrame(t *testing.T) {
	url := testURL(t)
	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("ListenPull: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan collected, 4)
	served := make(chan struct{})
	go func() {
		defer close(served)
		puller.Serve(ctx, func(_ context.Context, f mediabus.Frame) error {
			out <- collected{f.Type, f.Track, string(f.Payload)}
			return nil
		})
	}()
	defer func() {
		cancel()
		<-served
	}()

	// A raw PUSH socket, so that a frame with no type byte can be sent.
	raw, err := rawPusher(url)
	if err != nil {
		t.Fatalf("raw pusher: %v", err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			t.Errorf("raw close: %v", err)
		}
	}()

	if err := raw.Send([]byte{}); err != nil {
		t.Fatalf("send empty: %v", err)
	}
	if err := raw.Send([]byte{0x00, 0x00, 'x'}); err != nil {
		t.Fatalf("send zero type: %v", err)
	}
	if err := raw.Send(mediabus.EncodeFrame(mediabus.FrameRTP, 0, []byte("good"))); err != nil {
		t.Fatalf("send good: %v", err)
	}

	select {
	case got := <-out:
		if got.ft != mediabus.FrameRTP || got.payload != "good" {
			t.Fatalf("got %v, want a good RTP frame", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the puller stopped on a malformed frame")
	}
}

// rawPusher opens a plain PUSH socket, so that a test can put bytes on the bus
// that the Pusher would refuse to encode.
func rawPusher(url string) (mangos.Socket, error) {
	sock, err := push.NewSocket()
	if err != nil {
		return nil, err
	}
	if err := sock.SetOption(mangos.OptionSendDeadline, 2*time.Second); err != nil {
		_ = sock.Close()
		return nil, err
	}
	if err := sock.Dial(url); err != nil {
		_ = sock.Close()
		return nil, err
	}
	return sock, nil
}
