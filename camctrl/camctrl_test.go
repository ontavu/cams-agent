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

package camctrl

import (
	"errors"
	"testing"

	"github.com/ontavu/cams-agent/agentbus"
	"github.com/ontavu/cams-agent/errkind"
)

// An unknown verb is refused as one, so that a dispatcher can tell it from a
// command that failed. An empty one is a malformed request, not an unknown
// verb, and has no kind.
func TestParseCommand_UnknownIsNotSupported(t *testing.T) {
	for _, verb := range []string{"BOGUS", "play", "LIST"} {
		if _, err := ParseCommand(verb); !errors.Is(err, errkind.ErrNotSupported) {
			t.Fatalf("ParseCommand(%q): %v, want ErrNotSupported", verb, err)
		}
	}
}

func TestParseCommand_RoundTrip(t *testing.T) {
	for _, cmd := range []Command{
		CommandPlay, CommandPause, CommandPing, CommandExit, CommandState,
	} {
		t.Run(string(cmd), func(t *testing.T) {
			got, err := ParseCommand(string(cmd))
			if err != nil {
				t.Fatalf("ParseCommand(%q): %v", string(cmd), err)
			}
			if got != cmd {
				t.Fatalf("ParseCommand(%q) = %q", string(cmd), string(got))
			}
		})
	}
}

// TestParseCommand_Rejected covers what reaches the vocabulary once agentbus has
// split the request: a bare token that is not one of ours.
func TestParseCommand_Rejected(t *testing.T) {
	for _, verb := range []string{"", "play", "PLAYY", "OK", "\x00", "STATUS"} {
		if got, err := ParseCommand(verb); err == nil {
			t.Fatalf("ParseCommand(%q) = %q, want an error", verb, string(got))
		}
	}
}

// TestParseCommand_AfterDecodeRequest pins the two halves together: whatever
// framing agentbus strips, the verb it hands over must still be recognised.
func TestParseCommand_AfterDecodeRequest(t *testing.T) {
	for _, payload := range []string{"PLAY", "PLAY\n", " PLAY ", "PLAY\r\n", "\tPLAY\t"} {
		verb, arg, err := agentbus.DecodeRequest([]byte(payload))
		if err != nil {
			t.Fatalf("DecodeRequest(%q): %v", payload, err)
		}
		if arg != "" {
			t.Fatalf("DecodeRequest(%q) produced argument %q", payload, arg)
		}
		got, err := ParseCommand(verb)
		if err != nil {
			t.Fatalf("ParseCommand(%q) after DecodeRequest(%q): %v", verb, payload, err)
		}
		if got != CommandPlay {
			t.Fatalf("payload %q became %q, want PLAY", payload, string(got))
		}
	}
}

func TestParseState_RoundTrip(t *testing.T) {
	for _, st := range []State{
		StateOff, StateIdle, StatePlaying, StatePausing, StateResuming,
	} {
		got, err := ParseState(string(st))
		if err != nil {
			t.Fatalf("ParseState(%q): %v", string(st), err)
		}
		if got != st {
			t.Fatalf("ParseState(%q) = %q", string(st), string(got))
		}
	}
}

func TestParseState_Rejected(t *testing.T) {
	for _, token := range []string{"", "off", "PLAYIN", "UNKNOWN"} {
		if got, err := ParseState(token); err == nil {
			t.Fatalf("ParseState(%q) = %q, want an error", token, string(got))
		}
	}
}

// TestState_SurvivesAReply is the round trip CommandState depends on: each state
// has to come back out of a reply as itself.
func TestState_SurvivesAReply(t *testing.T) {
	for _, st := range []State{
		StateOff, StateIdle, StatePlaying, StatePausing, StateResuming,
	} {
		arg, err := agentbus.DecodeReply(agentbus.EncodeOK(string(st)))
		if err != nil {
			t.Fatalf("state %q: %v", string(st), err)
		}
		got, err := ParseState(arg)
		if err != nil {
			t.Fatalf("state %q: %v", string(st), err)
		}
		if got != st {
			t.Fatalf("state %q came back as %q", string(st), string(got))
		}
	}
}

func TestParseEncoding(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  Encoding
	}{
		{"H264", EncodingH264},
		{"H265", EncodingH265},
		{"JPEG", EncodingJPEG},
		{"UNSPECIFIED", EncodingUnspecified},
		// ONVIF spelling varies between devices.
		{"h264", EncodingH264},
		{" H265 ", EncodingH265},
		// An encoding a newer agent knows about and this revision does not.
		{"AV1", EncodingUnspecified},
		{"", EncodingUnspecified},
	} {
		if got := ParseEncoding(tc.token); got != tc.want {
			t.Fatalf("ParseEncoding(%q) = %q, want %q", tc.token, string(got), string(tc.want))
		}
	}
}

func TestMedia_RoundTrip(t *testing.T) {
	for _, m := range []Media{
		{Encoding: EncodingH264, Width: 640, Height: 480, GopLength: 30},
		{Encoding: EncodingH265, Width: 2560, Height: 1920, GopLength: 0},
		{Encoding: EncodingJPEG},
		// A camera that has never streamed.
		{},
	} {
		got, err := DecodeMedia(EncodeMedia(m))
		if err != nil {
			t.Fatalf("media %+v: %v", m, err)
		}
		want := m
		if want.Encoding == "" {
			// The zero value goes on the wire as UNSPECIFIED and comes back so.
			want.Encoding = EncodingUnspecified
		}
		if got != want {
			t.Fatalf("media %+v came back as %+v", want, got)
		}
	}
}

// The reply crosses agentbus, so it has to survive that framing too.
func TestMedia_SurvivesAReply(t *testing.T) {
	want := Media{Encoding: EncodingH264, Width: 640, Height: 480, GopLength: 25}

	arg, err := agentbus.DecodeReply(agentbus.EncodeOK(EncodeMedia(want)))
	if err != nil {
		t.Fatalf("DecodeReply: %v", err)
	}
	got, err := DecodeMedia(arg)
	if err != nil {
		t.Fatalf("DecodeMedia: %v", err)
	}
	if got != want {
		t.Fatalf("media came back as %+v, want %+v", got, want)
	}
}

func TestDecodeMedia_Rejected(t *testing.T) {
	for _, arg := range []string{
		"",
		"H264",
		"H264 640",
		"H264 640 480",
		"H264 wide 480 30",
		"H264 640 480 -1",
		"H264 -640 480 30",
	} {
		if _, err := DecodeMedia(arg); err == nil {
			t.Fatalf("DecodeMedia(%q) was accepted", arg)
		}
	}
}

// An agent of a newer revision may report more about itself; that must not stop
// this one understanding the part it knows.
func TestDecodeMedia_ToleratesExtraFields(t *testing.T) {
	got, err := DecodeMedia("H264 640 480 30 something-else 7")
	if err != nil {
		t.Fatalf("DecodeMedia: %v", err)
	}
	want := Media{Encoding: EncodingH264, Width: 640, Height: 480, GopLength: 30}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestMedia_Unknown(t *testing.T) {
	for _, tc := range []struct {
		media Media
		want  bool
	}{
		{Media{}, true},
		{Media{Encoding: EncodingUnspecified}, true},
		// Dimensions without an encoding are still nothing to go on.
		{Media{Width: 640, Height: 480}, true},
		{Media{Encoding: EncodingH264}, false},
	} {
		if got := tc.media.Unknown(); got != tc.want {
			t.Fatalf("%+v.Unknown() = %v, want %v", tc.media, got, tc.want)
		}
	}
}
