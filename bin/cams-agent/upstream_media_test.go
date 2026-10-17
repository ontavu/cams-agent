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

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ontavu/cams-agent/mediabus"
	"github.com/ontavu/cams-api/pb"
)

// TestFrameTypeToPB checks the one place where the internal bus meets the gRPC
// contract. The mapping is deliberately a switch rather than a cast, so this is
// what would catch it drifting.
func TestFrameTypeToPB(t *testing.T) {
	for _, tc := range []struct {
		from mediabus.FrameType
		want pb.DownstreamMediaFrameType
	}{
		{mediabus.FrameSDP, pb.DownstreamMediaFrameType_DOWNSTREAM_MEDIA_FRAME_TYPE_SDP},
		{mediabus.FrameRTP, pb.DownstreamMediaFrameType_DOWNSTREAM_MEDIA_FRAME_TYPE_RTP},
		{mediabus.FrameRTCP, pb.DownstreamMediaFrameType_DOWNSTREAM_MEDIA_FRAME_TYPE_RTCP},
	} {
		t.Run(tc.from.String(), func(t *testing.T) {
			got, err := frameTypeToPB(tc.from)
			if err != nil {
				t.Fatalf("frameTypeToPB(%v): %v", tc.from, err)
			}
			if got != tc.want {
				t.Fatalf("frameTypeToPB(%v) = %v, want %v", tc.from, got, tc.want)
			}
		})
	}
}

// TestFrameTypeToPB_EveryTypeIsMapped guards against a frame type being added
// to the bus and quietly failing to reach the hub.
func TestFrameTypeToPB_EveryTypeIsMapped(t *testing.T) {
	for b := 0; b <= 255; b++ {
		ft := mediabus.FrameType(b)
		_, err := frameTypeToPB(ft)
		if ft.Valid() && err != nil {
			t.Fatalf("frame type %d is valid on the bus but has no gRPC mapping", b)
		}
		if !ft.Valid() && err == nil {
			t.Fatalf("frame type %d is invalid on the bus but was mapped anyway", b)
		}
	}
}

// TestFrameTypeToPB_Rejected pins that an unknown type is refused rather than
// sent as UNSPECIFIED, which the hub could only discard.
func TestFrameTypeToPB_Rejected(t *testing.T) {
	for _, b := range []byte{0, 4, 99, 255} {
		got, err := frameTypeToPB(mediabus.FrameType(b))
		if err == nil {
			t.Fatalf("frame type %d was mapped to %v", b, got)
		}
		if got != pb.DownstreamMediaFrameType_DOWNSTREAM_MEDIA_FRAME_TYPE_UNSPECIFIED {
			t.Fatalf("a refused mapping returned %v, want UNSPECIFIED", got)
		}
	}
}

// TestConfig_ProjectsOntoUpstreamAgent checks the mapping that keeps the
// on-disk format out of the upstream agent. A field silently lost here would
// disable a feature with no error anywhere.
func TestConfig_ProjectsOntoUpstreamAgent(t *testing.T) {
	cfg := AgentConfig{
		User:            "someone",
		RegisterPeriod:  7,
		UpstreamControl: UpstreamConfig{Address: "hub.example:6000"},
		// The media endpoint belongs to the per-camera sink, not to this agent,
		// so it must not leak into the projection.
		UpstreamMedia: MediaConfig{Address: "media.example:6001"},
	}

	got := cfg.upstream()

	if got.User != "someone" {
		t.Fatalf("user came through as %q", got.User)
	}
	if got.ControlAddress != "hub.example:6000" {
		t.Fatalf("control address came through as %q", got.ControlAddress)
	}
	if got.RegisterPeriod != 7*time.Second {
		t.Fatalf("register period came through as %v, want 7s", got.RegisterPeriod)
	}
}

// TestConfig_UpstreamLeavesZeroAlone pins the contract with the agent: an
// unset period is passed through as zero rather than turned into some number
// here, because zero is what the agent reads as "use your default".
func TestConfig_UpstreamLeavesZeroAlone(t *testing.T) {
	cfg := AgentConfig{}
	got := cfg.upstream()
	if got.RegisterPeriod != 0 {
		t.Fatalf("an unset period was mapped to %v, want zero", got.RegisterPeriod)
	}
}

// recordingUpload is the client half of an upload stream, as far as onFrame
// uses it: Send. The embedded interface stays nil.
type recordingUpload struct {
	pb.Uploader_MediaUploadClient
	sent []*pb.DownstreamMediaFrame
	err  error
}

func (u *recordingUpload) Send(f *pb.DownstreamMediaFrame) error {
	u.sent = append(u.sent, f)
	return u.err
}

// Every frame of a stream goes through onFrame, so a successful Send has to
// come back as nil: wrapped with %w, it would be an error, and the upload
// would end on its first packet. A failed one comes back wrapped.
func TestOnFrame_ReportsOnlyAFailedSend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sendErr error
	}{
		{"sent", nil},
		{"refused", errors.New("stream reset")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upload := &recordingUpload{err: tc.sendErr}
			sink := &grpcMediaSink{upload: upload}

			err := sink.onFrame(context.Background(),
				mediabus.Frame{Type: mediabus.FrameRTP, Track: 1, Payload: []byte{0x80}})

			if tc.sendErr == nil && err != nil {
				t.Fatalf("onFrame: %v after a successful send", err)
			}
			if tc.sendErr != nil && !errors.Is(err, tc.sendErr) {
				t.Fatalf("onFrame: %v, want the send's own error wrapped", err)
			}
			if len(upload.sent) != 1 {
				t.Fatalf("%d frames sent, want one", len(upload.sent))
			}
		})
	}
}
