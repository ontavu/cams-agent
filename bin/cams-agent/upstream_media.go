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
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ontavu/cams-agent/errkind"
	"github.com/ontavu/cams-agent/mediabus"
	"github.com/ontavu/cams-api/pb"
	"github.com/ontavu/cams-api/wire"
	"github.com/ontavu/cams-go-utils/utils"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// frameTypeToPB maps a bus frame onto the gRPC enum.
//
// The mapping is a switch and not a cast on purpose. cams-api's hub.proto is
// the source of truth for the wire and may be renumbered; the bus has its own
// values, and neither should be able to silently reinterpret the other. An
// unknown type is refused rather than sent as UNSPECIFIED, which the hub could
// only discard.
func frameTypeToPB(t mediabus.FrameType) (pb.DownstreamMediaFrameType, error) {
	switch t {
	case mediabus.FrameSDP:
		return pb.DownstreamMediaFrameType_DOWNSTREAM_MEDIA_FRAME_TYPE_SDP, nil
	case mediabus.FrameRTP:
		return pb.DownstreamMediaFrameType_DOWNSTREAM_MEDIA_FRAME_TYPE_RTP, nil
	case mediabus.FrameRTCP:
		return pb.DownstreamMediaFrameType_DOWNSTREAM_MEDIA_FRAME_TYPE_RTCP, nil
	default:
		return pb.DownstreamMediaFrameType_DOWNSTREAM_MEDIA_FRAME_TYPE_UNSPECIFIED,
			fmt.Errorf("media frame type %d %w", byte(t), errkind.ErrNotValid)
	}
}

// grpcMediaSink is the media upstream of one camera: it pulls the frames the
// camera pushes and forwards them to the hub.
//
// It is the passive side of the media bus, so it binds at construction and the
// camera connects to it. Its state is touched only from the serving goroutine,
// so it needs no lock.
type grpcMediaSink struct {
	puller *mediabus.Puller

	userID   string
	camID    string
	endpoint string

	// The stream in progress, if any. A session description opens one and
	// replaces whatever was there.
	cnx    *grpc.ClientConn
	upload pb.Uploader_MediaUploadClient

	droppedNoStream atomic.Uint64
}

// NewGrpcMediaSink binds the media endpoint at bindURL. Nothing is dialled
// towards the hub yet: a gRPC stream is opened when a camera announces one.
func NewGrpcMediaSink(userID, camID, endpoint, bindURL string) (*grpcMediaSink, error) {
	puller, err := mediabus.ListenPull("cam "+camID, bindURL)
	if err != nil {
		return nil, err
	}
	return &grpcMediaSink{
		puller:   puller,
		userID:   userID,
		camID:    camID,
		endpoint: endpoint,
	}, nil
}

// Close releases the media endpoint of a sink that will never Run.
func (sink *grpcMediaSink) Close() error {
	return sink.puller.Close()
}

// Run consumes the media of the camera until the context is cancelled.
func (sink *grpcMediaSink) Run(ctx context.Context) {
	defer sink.closeStream()
	sink.puller.Serve(ctx, sink.onFrame)
}

// onFrame forwards one frame, opening a stream when the camera announces one.
func (sink *grpcMediaSink) onFrame(ctx context.Context, f mediabus.Frame) error {
	if f.Type == mediabus.FrameSDP {
		// A banner starts an attempt. Whatever came before is over: a camera
		// only sends one after establishing a fresh RTSP session.
		sink.closeStream()
		if err := sink.openStream(ctx); err != nil {
			return fmt.Errorf("open upload: %w", err)
		}
	}

	if sink.upload == nil {
		// Media with no banner before it: the hub could not interpret it, so it
		// is counted rather than forwarded blindly.
		sink.droppedNoStream.Add(1)
		return nil
	}

	kind, err := frameTypeToPB(f.Type)
	if err != nil {
		return err
	}

	// The payload is only valid until this returns, and marshalling the message
	// copies it, so it does not need copying here.
	//
	// The timestamp is taken here rather than at capture: the bus hop is
	// in-process, so the difference is not measurable, and this is the last
	// point that still belongs to the agent. It tells the hub how long a packet
	// took to arrive, which nothing else on the wire can express -- the RTP
	// timestamps are the camera's own clock, with no relation to wall time.
	frame := &pb.DownstreamMediaFrame{
		Type:               kind,
		Payload:            f.Payload,
		Track:              uint32(f.Track),
		ForwardedUnixNanos: time.Now().UnixNano(),
	}
	// Guarded: this is every frame's path, and %w would make an error of the
	// nil a successful Send returns.
	if err := sink.upload.Send(frame); err != nil {
		return fmt.Errorf("upload %s: %w", f.Type, err)
	}
	return nil
}

func (sink *grpcMediaSink) openStream(ctx context.Context) error {
	cnx, err := utils.DialInsecure(ctx, sink.endpoint, wire.DialOptions()...)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	client := pb.NewUploaderClient(cnx)
	ctx = wire.WithSession(metadata.AppendToOutgoingContext(ctx,
		wire.KeyUser, sink.userID,
		wire.KeyStream, sink.camID))

	upload, err := client.MediaUpload(ctx)
	if err != nil {
		if cerr := cnx.Close(); cerr != nil {
			utils.Logger.Warn().Err(cerr).Str("cam", sink.camID).Msg("upload close")
		}
		return fmt.Errorf("call: %w", err)
	}

	sink.cnx = cnx
	sink.upload = upload
	return nil
}

func (sink *grpcMediaSink) closeStream() {
	if sink.upload != nil {
		if err := sink.upload.CloseSend(); err != nil {
			utils.Logger.Warn().Err(err).Str("cam", sink.camID).Msg("upload close send")
		}
		sink.upload = nil
	}
	if sink.cnx != nil {
		if err := sink.cnx.Close(); err != nil {
			utils.Logger.Warn().Err(err).Str("cam", sink.camID).Msg("upload close")
		}
		sink.cnx = nil
	}
	if dropped := sink.droppedNoStream.Swap(0); dropped > 0 {
		utils.Logger.Warn().
			Uint64("frames", dropped).
			Str("cam", sink.camID).
			Msg("media frames arrived with no stream open")
	}
}
