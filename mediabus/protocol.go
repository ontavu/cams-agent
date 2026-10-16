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

// Package mediabus carries the media of one camera from the agent that captures
// it to the upstream that forwards it.
//
// It is the data plane, and a sibling of agentbus rather than a part of it: this
// is PUSH/PULL carrying binary frames at packet rates, where agentbus is REQ/REP
// carrying one line of text per command. The camera is the active side and
// connects; the upstream is the passive side and binds.
//
// A frame is one type byte, one track byte, then the payload. Zero is not a
// valid type, so an empty or truncated message is an error rather than an
// accidental banner.
package mediabus

import (
	"github.com/juju/errors"
	"go.nanomsg.org/mangos/v3"
)

// FrameType tells what a frame carries.
//
// The values are this bus's own. They are mapped onto the gRPC enum explicitly
// by whoever forwards them, so that a renumbering of the protocol buffer cannot
// silently reinterpret what travels here.
type FrameType byte

const (
	// FrameSDP is the session description of a stream. It opens a stream: a
	// camera emits one before any packet of a new attempt.
	FrameSDP FrameType = 1

	// FrameRTP is one RTP packet, marshalled.
	FrameRTP FrameType = 2

	// FrameRTCP is one RTCP packet, marshalled. A compound datagram arrives as
	// several frames, one per sub-packet.
	FrameRTCP FrameType = 3
)

func (t FrameType) String() string {
	switch t {
	case FrameSDP:
		return "SDP"
	case FrameRTP:
		return "RTP"
	case FrameRTCP:
		return "RTCP"
	default:
		return "INVALID"
	}
}

// Valid reports whether t is a type this bus defines.
func (t FrameType) Valid() bool {
	switch t {
	case FrameSDP, FrameRTP, FrameRTCP:
		return true
	default:
		return false
	}
}

// Frame is one message off the bus.
type Frame struct {
	Type FrameType

	// Track is which media of the session description the payload belongs to,
	// as the zero-based index of its "m=" section.
	//
	// It travels with every packet because the payload alone cannot say: an RTP
	// packet identifies its format by payload type, which only distinguishes
	// medias while each has a different one. A session description describes
	// them all and carries 0.
	Track byte

	// Payload aliases the received message and is only valid until the handler
	// returns; anything kept beyond that has to be copied.
	Payload []byte
}

// EncodeFrame builds the body of a frame.
//
// The hot path does not use it: Pusher.Send writes the same bytes straight into
// a pooled message. It is here for the tests and for anybody encoding a frame
// outside a socket.
func EncodeFrame(t FrameType, track byte, payload []byte) []byte {
	out := make([]byte, 0, frameHeaderLen+len(payload))
	out = append(out, byte(t), track)
	return append(out, payload...)
}

// frameHeaderLen is the type byte plus the track byte.
const frameHeaderLen = 2

// DecodeFrame splits a frame into its fields.
//
// The payload aliases msg, so a caller that keeps it beyond the life of the
// message has to copy it.
func DecodeFrame(msg []byte) (Frame, error) {
	if len(msg) < frameHeaderLen {
		return Frame{}, errors.NotValidf("media frame of %d bytes", len(msg))
	}
	t := FrameType(msg[0])
	if !t.Valid() {
		return Frame{}, errors.NotValidf("media frame type %d", msg[0])
	}
	return Frame{Type: t, Track: msg[1], Payload: msg[frameHeaderLen:]}, nil
}

// IsOverflow reports a frame the upstream could not keep up with, as opposed to
// a broken link. Overflow is countable and survivable; anything else means the
// stream is over.
//
// It is the caller's job to decide that a dropped packet is acceptable. For the
// session description it is not, and the camera treats every failure to send one
// as fatal.
func IsOverflow(err error) bool {
	return errors.Is(err, mangos.ErrSendTimeout)
}
