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
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/liberrors"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/errkind"
	"github.com/ontavu/cams-agent/mediabus"
	"github.com/ontavu/cams-go-utils/utils"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"golang.org/x/sync/errgroup"
)

const (
	rtspReadTimeout  = 5 * time.Second
	rtspWriteTimeout = 5 * time.Second
)

// retryPeriod is the pause between two attempts at a camera. It exists to keep
// a stream that fails instantly from becoming a busy loop: a media bus with no
// listener, or an unparseable stream URI, both return in microseconds.
const retryPeriod = time.Second

func (cam *Agent) runStream(ctx context.Context) {
	for ctx.Err() == nil {
		cam.debug().Msg("cam stream starting")

		if err := cam.runStreamOnce(ctx); err != nil {
			cam.debug().Err(err).Msg("cam stream aborted")
		}

		if !cam.flagRetry {
			break
		}

		// The pause guards every attempt, and the failing one above all. It
		// used to guard only the success branch, so a camera whose stream died
		// immediately -- an upstream that is not listening, an ONVIF endpoint
		// answering nonsense -- spun at full speed, re-dialling the camera and
		// emitting a log line per iteration.
		select {
		case <-ctx.Done():
			// Waiting out the pause after a cancellation would delay the
			// teardown for no reason.
		case <-time.After(retryPeriod):
		}
	}
	cam.debug().Str("url", cam.id).Msg("cam stream exiting")
}

// mediaSink pushes the captured packets to the upstream.
//
// It is a thin wrapper around the bus rather than a queue of its own: a PUSH
// socket serialises concurrent sends internally, so the RTP and the RTCP
// callbacks -- which run on separate goroutines while the transport is UDP --
// may both push directly. The bounded queue that used to live here now lives
// inside the socket.
type mediaSink struct {
	pusher *mediabus.Pusher

	droppedRTP  atomic.Uint64
	droppedRTCP atomic.Uint64

	// fatal holds the first error that means the stream is over, as opposed to
	// a frame the upstream could not keep up with.
	fatal     atomic.Pointer[error]
	onFatal   func()
	fatalOnce sync.Once
}

func newMediaSink(pusher *mediabus.Pusher, onFatal func()) *mediaSink {
	return &mediaSink{pusher: pusher, onFatal: onFatal}
}

// sdpTrack is the track a session description travels under: it describes every
// media, so it belongs to none of them in particular.
const sdpTrack byte = 0

// trackIndex maps each media onto its position in the session description,
// which is the track number the upstream and the hub agree on.
//
// A description with more medias than a byte can number is not something this
// project expects; the excess is left at zero rather than wrapping, so that a
// bad index cannot silently name the wrong media.
func trackIndex(medias []*description.Media) map[*description.Media]byte {
	out := make(map[*description.Media]byte, len(medias))
	for i, m := range medias {
		if i > 0xFF {
			break
		}
		out[m] = byte(i)
	}
	return out
}

// push forwards one packet, tolerating an upstream that falls behind.
//
// A callback runs on a reading goroutine of the RTSP client, so it must not
// dwell here: an overflow costs the send timeout and then the packet, which is
// far cheaper than stalling the session. Anything else ends the attempt, the
// way the draining goroutine used to end it by returning an error.
func (sink *mediaSink) push(t mediabus.FrameType, track byte, payload []byte, dropped *atomic.Uint64) {
	err := sink.pusher.Send(t, track, payload)
	switch {
	case err == nil:
	case mediabus.IsOverflow(err):
		dropped.Add(1)
	default:
		sink.abort(err)
	}
}

// abort records the first fatal error and releases the stream.
func (sink *mediaSink) abort(err error) {
	sink.fatalOnce.Do(func() {
		sink.fatal.Store(&err)
		sink.onFatal()
	})
}

// Err returns the error that ended the stream, if the upstream is what ended it.
func (sink *mediaSink) Err() error {
	if p := sink.fatal.Load(); p != nil {
		return *p
	}
	return nil
}

func (cam *Agent) runStreamOnce(ctx context.Context) error {
	// Prepare the camera RTSP side
	sourceUrl, err := cam.queryMediaUrl(ctx)
	if err != nil {
		return fmt.Errorf("queryMediaUrl: %w", err)
	}

	cam.debug().Str("url", sourceUrl.Host).Msg("cam streaming")

	// A client is single-use: following a redirection rewrites its Scheme and
	// its Host, so reusing one would carry the target of the previous attempt.
	client := &gortsplib.Client{
		Scheme:        sourceUrl.Scheme,
		Host:          sourceUrl.Host,
		ReadTimeout:   rtspReadTimeout,
		WriteTimeout:  rtspWriteTimeout,
		AnyPortEnable: true,
		OnTransportSwitch: func(err error) {
			cam.debug().Err(err).Msg("rtsp transport switch")
		},
		OnPacketsLost: func(lost uint64) {
			cam.debug().Uint64("lost", lost).Msg("rtp packets lost")
		},
		OnDecodeError: func(err error) {
			cam.warn(err).Msg("rtsp decode")
		},
	}

	if err = client.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	// Closing is also what tears the session down at the camera.
	defer client.Close()

	desc, sdpResp, err := client.Describe(sourceUrl)
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}
	sdp := string(sdpResp.Body)
	cam.debug().
		Str("url", desc.BaseURL.String()).
		Str("sdp", sdp).
		Interface("medias", desc.Medias).
		Msg("streams described")

	// The stream context is created here rather than just before Play, because
	// the callbacks below cancel it when the upstream breaks.
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()

	// Prepare the upstream side. A synchronous dial means an upstream that has
	// stopped is reported now instead of swallowing a stream's worth of frames.
	pusher, err := mediabus.DialPush("cam "+cam.id, cam.mediaURL)
	if err != nil {
		return fmt.Errorf("dial media bus: %w", err)
	}
	defer func() {
		if cerr := pusher.Close(); cerr != nil {
			cam.warn(cerr).Msg("media bus close")
		}
	}()

	// Leave the choice of the ports to the library: it honours the even/odd
	// convention of RFC 3550 and it rejects a pair that is not consecutive.
	for _, m := range desc.Medias {
		if m.Type != description.MediaTypeVideo {
			continue
		}
		if _, err = client.Setup(desc.BaseURL, m, 0, 0); err != nil {
			return fmt.Errorf("RTSP Setup: %w", err)
		}
		utils.Logger.Info().Interface("media", *m).Msg("RTSP Setup")
	}

	// Both registrations walk the medias that Setup has installed, so they only
	// have an effect once every Setup is done. They survive a switch to the
	// interleaved transport, which replays Setup and carries them over.
	// The upstream needs to know which "m=" section a packet belongs to, and the
	// packet itself cannot say: the callbacks name the media, so the index is
	// resolved here, where the description is still in scope.
	tracks := trackIndex(desc.Medias)

	sink := newMediaSink(pusher, stopStream)
	client.OnPacketRTPAny(func(m *description.Media, _ format.Format, pkt *rtp.Packet) {
		raw, merr := pkt.Marshal()
		if merr != nil {
			cam.warn(merr).Msg("marshal rtp")
			return
		}
		sink.push(mediabus.FrameRTP, tracks[m], raw, &sink.droppedRTP)
	})
	client.OnPacketRTCPAny(func(m *description.Media, pkt rtcp.Packet) {
		raw, merr := pkt.Marshal()
		if merr != nil {
			cam.warn(merr).Msg("marshal rtcp")
			return
		}
		sink.push(mediabus.FrameRTCP, tracks[m], raw, &sink.droppedRTCP)
	})

	// The banner is not droppable: an upstream that never receives it holds a
	// stream it cannot interpret. The queue is empty at this point, so the only
	// way this fails is an upstream that is not there.
	if err = pusher.Send(mediabus.FrameSDP, sdpTrack, []byte(sdp)); err != nil {
		return fmt.Errorf("send sdp banner: %w", err)
	}

	// Spawn goroutines that will consume the camera stream
	if _, err = client.Play(nil); err != nil {
		return fmt.Errorf("play: %w", err)
	}

	g, gctx := errgroup.WithContext(streamCtx)

	// The client knows nothing about contexts: closing it is what releases the
	// call to Wait below.
	g.Go(func() error {
		<-gctx.Done()
		client.Close()
		return nil
	})
	// Whichever of the session and the upstream ends first stops the other.
	g.Go(func() error {
		defer stopStream()
		return client.Wait()
	})

	err = g.Wait()

	if dropped := sink.droppedRTP.Load(); dropped > 0 {
		cam.debug().Uint64("rtp", dropped).
			Uint64("rtcp", sink.droppedRTCP.Load()).
			Msg("packets dropped, upstream too slow")
	}

	// A deliberate Close is the ordinary way out, not a failure.
	var terminated liberrors.ErrClientTerminated
	if errors.As(err, &terminated) {
		err = nil
	}

	// A broken upstream cancels the stream, so the session reports an ordinary
	// teardown and the real cause is the one the sink recorded.
	if err == nil {
		if serr := sink.Err(); serr != nil {
			return fmt.Errorf("upstream: %w", serr)
		}
	}
	return err
}

func (cam *Agent) queryMediaUrl(ctx context.Context) (*base.URL, error) {
	// Asked again on every attempt, deliberately. A camera that reboots may
	// come back reconfigured -- another encoder, another resolution -- and the
	// only way to notice is to look. Nothing here is cached, and the SDK caches
	// nothing either: sdk.deviceWrapper holds a client and no profile state.
	offered, err := cam.onvifClient.MediaProfiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("profiles: %w", err)
	}
	chosen, ok := chooseProfile(videoProfilesOf(offered))
	if !ok {
		return nil, fmt.Errorf("no ONVIF profile offering a video stream %w", errkind.ErrNotFound)
	}
	cam.debug().
		Str("profile", chosen.token).
		Str("encoding", chosen.encoding).
		Int("width", chosen.width).
		Int("height", chosen.height).
		Int("gop", chosen.gop).
		Str("uri", chosen.uri).
		Msg("PROFILE")

	// Recorded here rather than after Describe, because this is what was asked
	// for: what the camera then sends is checked by the hub, which parses the
	// description it receives.
	cam.noteMedia(camctrl.Media{
		Encoding:  camctrl.ParseEncoding(chosen.encoding),
		Width:     chosen.width,
		Height:    chosen.height,
		GopLength: chosen.gop,
	})

	streamURI, err := cam.credentialled(chosen.uri)
	if err != nil {
		return nil, fmt.Errorf("credentials: %w", err)
	}
	sourceUrl, err := base.ParseURL(streamURI)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	// Logged without the credentials. This used to print the URL twice, both
	// times with the camera's password in it.
	safe := url.URL(*sourceUrl)
	safe.User = nil
	cam.debug().Str("source", safe.String()).Msg("STREAM")
	return sourceUrl, nil
}

// credentialled puts the camera's credentials where gortsplib looks for them,
// which is the URL's userinfo.
//
// Through net/url rather than by surgery on the "rtsp://" prefix, which is what
// the SDK does: a password holding a slash or an at-sign survives this, and any
// credentials the camera put in the URI itself are replaced rather than
// producing two sets.
func (cam *Agent) credentialled(raw string) (string, error) {
	if cam.user == "" {
		return raw, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	parsed.User = url.UserPassword(cam.user, cam.password)
	return parsed.String(), nil
}
