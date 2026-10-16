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

package camagent

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/ontavu/cams-agent/mediabus"
)

func sinkTestURL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("inproc://cams/test/sink-%s-%d", t.Name(), time.Now().UnixNano())
}

// newSinkFixture wires a sink to a puller that collects the frames, and reports
// whether the sink declared the stream over.
func newSinkFixture(t *testing.T, handler mediabus.Handler) (*mediaSink, *atomic.Bool) {
	t.Helper()

	url := sinkTestURL(t)
	puller, err := mediabus.ListenPull("test", url)
	if err != nil {
		t.Fatalf("ListenPull: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		puller.Serve(ctx, handler)
	}()

	pusher, err := mediabus.DialPush("test", url)
	if err != nil {
		cancel()
		<-served
		t.Fatalf("DialPush: %v", err)
	}

	aborted := &atomic.Bool{}
	sink := newMediaSink(pusher, func() { aborted.Store(true) })

	t.Cleanup(func() {
		if err := pusher.Close(); err != nil {
			t.Errorf("pusher close: %v", err)
		}
		cancel()
		<-served
	})

	return sink, aborted
}

// TestMediaSink_ForwardsPackets is the happy path: what the callbacks push comes
// out of the puller, with its type and bytes intact.
func TestMediaSink_ForwardsPackets(t *testing.T) {
	type frame struct {
		t       mediabus.FrameType
		track   byte
		payload string
	}

	got := make(chan frame, 8)
	sink, aborted := newSinkFixture(t, func(_ context.Context, f mediabus.Frame) error {
		// The payload aliases the message, so it is copied before travelling.
		got <- frame{f.Type, f.Track, string(f.Payload)}
		return nil
	})

	sink.push(mediabus.FrameRTP, 0, []byte("rtp-one"), &sink.droppedRTP)
	sink.push(mediabus.FrameRTCP, 0, []byte("rtcp-one"), &sink.droppedRTCP)
	// A second media: the track has to survive the hop, or the upstream cannot
	// tell one media's packets from another's.
	sink.push(mediabus.FrameRTP, 1, []byte("rtp-two"), &sink.droppedRTP)

	want := []frame{
		{mediabus.FrameRTP, 0, "rtp-one"},
		{mediabus.FrameRTCP, 0, "rtcp-one"},
		{mediabus.FrameRTP, 1, "rtp-two"},
	}
	for i, w := range want {
		select {
		case g := <-got:
			if g != w {
				t.Fatalf("frame %d = %v, want %v", i, g, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("frame %d never arrived", i)
		}
	}

	if aborted.Load() {
		t.Fatal("the sink declared the stream over on the happy path")
	}
	if n := sink.droppedRTP.Load() + sink.droppedRTCP.Load(); n != 0 {
		t.Fatalf("%d packets dropped with a puller keeping up", n)
	}
	if err := sink.Err(); err != nil {
		t.Fatalf("sink reported %v", err)
	}
}

// TestMediaSink_CountsOverflow is the drop policy. The handler blocks, both
// queues fill, and from then on packets are counted rather than forwarded --
// and crucially the stream is not declared over.
func TestMediaSink_CountsOverflow(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once

	sink, aborted := newSinkFixture(t, func(context.Context, mediabus.Frame) error {
		// Park the very first frame, so nothing is consumed afterwards.
		once.Do(func() { <-release })
		return nil
	})
	defer close(release)

	// Fill both queues and then keep going. The depths are known, so this is a
	// bounded loop rather than a wait for a condition.
	total := mediabus.DefaultQueueDepth + mediabus.DefaultReceiveDepth + 64
	for i := 0; i < total; i++ {
		sink.push(mediabus.FrameRTP, 0, []byte("packet"), &sink.droppedRTP)
	}

	if n := sink.droppedRTP.Load(); n == 0 {
		t.Fatal("a stalled puller caused no drop at all, the queues are not bounded")
	}
	if aborted.Load() {
		t.Fatalf("an overflow ended the stream: %v", sink.Err())
	}
	if err := sink.Err(); err != nil {
		t.Fatalf("an overflow was recorded as fatal: %v", err)
	}
}

// TestMediaSink_UpstreamGoneIsFatal is the other half of the classification: a
// puller that has stopped is not a slow puller, and the attempt has to end.
func TestMediaSink_UpstreamGoneIsFatal(t *testing.T) {
	url := sinkTestURL(t)
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

	aborted := &atomic.Bool{}
	sink := newMediaSink(pusher, func() { aborted.Store(true) })

	// Stop the upstream, then keep pushing.
	cancel()
	<-served

	deadline := time.Now().Add(5 * time.Second)
	for !aborted.Load() && time.Now().Before(deadline) {
		sink.push(mediabus.FrameRTP, 0, []byte("packet"), &sink.droppedRTP)
	}

	if !aborted.Load() {
		t.Fatal("pushing into a stopped upstream never ended the stream")
	}
	if err := sink.Err(); err == nil {
		t.Fatal("the stream ended with no recorded cause")
	}
}

// TestMediaSink_FirstFatalWins keeps the reported cause stable: the callbacks
// run concurrently, and the error the caller sees should be the first one.
func TestMediaSink_FirstFatalWins(t *testing.T) {
	sink, aborted := newSinkFixture(t, func(context.Context, mediabus.Frame) error { return nil })

	first := fmt.Errorf("first")
	sink.abort(first)
	sink.abort(fmt.Errorf("second"))

	if !aborted.Load() {
		t.Fatal("abort did not release the stream")
	}
	if got := sink.Err(); got != first {
		t.Fatalf("sink reported %v, want %v", got, first)
	}
}

// TestMediaSink_ConcurrentPush is the property that let the channels go: the
// RTP and RTCP callbacks run on different goroutines and both push directly.
func TestMediaSink_ConcurrentPush(t *testing.T) {
	var seen atomic.Uint64
	sink, aborted := newSinkFixture(t, func(context.Context, mediabus.Frame) error {
		seen.Add(1)
		return nil
	})

	const perKind = 200
	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < perKind; i++ {
			sink.push(mediabus.FrameRTP, 0, []byte("rtp"), &sink.droppedRTP)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < perKind; i++ {
			sink.push(mediabus.FrameRTCP, 0, []byte("rtcp"), &sink.droppedRTCP)
		}
	}()
	wg.Wait()

	if aborted.Load() {
		t.Fatalf("concurrent pushes ended the stream: %v", sink.Err())
	}

	// Everything either arrived or was counted; nothing may vanish.
	dropped := sink.droppedRTP.Load() + sink.droppedRTCP.Load()
	deadline := time.Now().Add(5 * time.Second)
	for seen.Load()+dropped < 2*perKind && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := seen.Load() + dropped; got != 2*perKind {
		t.Fatalf("%d frames accounted for, want %d", got, 2*perKind)
	}
}

// TestTrackIndex is the mapping the upstream depends on: a packet's callback
// names its media, and the hub needs the position of that media in the session
// description it was sent.
func TestTrackIndex(t *testing.T) {
	medias := []*description.Media{
		{Type: description.MediaTypeVideo},
		{Type: description.MediaTypeAudio},
		{Type: description.MediaTypeApplication},
	}

	idx := trackIndex(medias)
	if len(idx) != len(medias) {
		t.Fatalf("trackIndex covered %d of %d medias", len(idx), len(medias))
	}
	for want, m := range medias {
		if got := idx[m]; got != byte(want) {
			t.Fatalf("media %d indexed as %d", want, got)
		}
	}
}

// TestTrackIndex_Empty pins that a description with no media is not an error:
// the result is usable without a nil check, and a lookup yields the zero track.
func TestTrackIndex_Empty(t *testing.T) {
	idx := trackIndex(nil)
	if idx == nil {
		t.Fatal("trackIndex(nil) returned nil, want an empty map")
	}
	if got := idx[&description.Media{}]; got != 0 {
		t.Fatalf("an unknown media indexed as %d", got)
	}
}
