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

package lanctrl

import (
	"strings"
	"testing"

	"github.com/juju/errors"
	"github.com/ontavu/cams-agent/agentbus"
)

func TestParseCommand_RoundTrip(t *testing.T) {
	for _, cmd := range []Command{
		CommandList, CommandPlay, CommandPause, CommandPing,
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

func TestParseCommand_Rejected(t *testing.T) {
	// EXIT is deliberately not part of the vocabulary: the LAN agent lives as
	// long as the process.
	for _, verb := range []string{"", "list", "LISTS", "EXIT", "STATE", "OK"} {
		if got, err := ParseCommand(verb); err == nil {
			t.Fatalf("ParseCommand(%q) = %q, want an error", verb, string(got))
		}
	}
}

func TestIDs_RoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
	}{
		{"empty", []string{}},
		{"one", []string{"cam-a"}},
		{"several", []string{"cam-a", "cam-b", "cam-c"}},
		{"uuids", []string{
			"3fa85f64-5717-4562-b3fc-2c963f66afa6",
			"9c858901-8a57-4791-81fe-4c455b099bc9",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeIDs(EncodeIDs(tc.ids))
			if len(got) != len(tc.ids) {
				t.Fatalf("round trip of %v gave %v", tc.ids, got)
			}
			for i := range got {
				if got[i] != tc.ids[i] {
					t.Fatalf("round trip of %v gave %v", tc.ids, got)
				}
			}
		})
	}
}

// TestEncodeIDs_DropsAmbiguous is the guard that keeps a LIST reply parseable.
// An identifier with a space in it would come back as two.
func TestEncodeIDs_DropsAmbiguous(t *testing.T) {
	got := EncodeIDs([]string{"cam-a", "bad id", "", "cam-b", "tab\there", "nl\nhere"})
	if got != "cam-a cam-b" {
		t.Fatalf("EncodeIDs = %q, want \"cam-a cam-b\"", got)
	}
}

// TestDecodeIDs_Padding covers a reply whose spacing is not exactly what we
// would have produced, since the agent may be of another revision.
func TestDecodeIDs_Padding(t *testing.T) {
	for _, arg := range []string{"cam-a cam-b", "  cam-a   cam-b  ", "cam-a\tcam-b", "cam-a  cam-b\n"} {
		got := DecodeIDs(arg)
		if len(got) != 2 || got[0] != "cam-a" || got[1] != "cam-b" {
			t.Fatalf("DecodeIDs(%q) = %v, want [cam-a cam-b]", arg, got)
		}
	}
}

// TestDecodeIDs_Empty pins that an agent with no camera is a normal answer and
// not an error, and that the result is usable without a nil check.
func TestDecodeIDs_Empty(t *testing.T) {
	for _, arg := range []string{"", "   ", "\n"} {
		got := DecodeIDs(arg)
		if got == nil {
			t.Fatalf("DecodeIDs(%q) returned nil, want an empty slice", arg)
		}
		if len(got) != 0 {
			t.Fatalf("DecodeIDs(%q) = %v, want empty", arg, got)
		}
	}
}

// TestListReply_RoundTrip walks the identifiers through the reply framing, the
// exchange List actually performs.
func TestListReply_RoundTrip(t *testing.T) {
	ids := []string{"cam-a", "cam-b"}

	arg, err := agentbus.DecodeReply(agentbus.EncodeOK(EncodeIDs(ids)))
	if err != nil {
		t.Fatalf("DecodeReply: %v", err)
	}
	got := DecodeIDs(arg)
	if len(got) != 2 || got[0] != "cam-a" || got[1] != "cam-b" {
		t.Fatalf("round trip gave %v, want %v", got, ids)
	}
}

func TestListReply_Error(t *testing.T) {
	payload := agentbus.EncodeErr(errors.New("lan is unwell"))

	if _, err := agentbus.DecodeReply(payload); err == nil {
		t.Fatal("a failure reply decoded as a success")
	} else if !strings.Contains(err.Error(), "lan is unwell") {
		t.Fatalf("error %v lost the message reported by the agent", err)
	}
}

// TestPlayRequest_CarriesTheCamera pins that the identifier survives the
// request framing, since PLAY is the only verb here with an argument.
func TestPlayRequest_CarriesTheCamera(t *testing.T) {
	const camID = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

	verb, arg, err := agentbus.DecodeRequest(agentbus.EncodeRequest(string(CommandPlay), camID))
	if err != nil {
		t.Fatalf("DecodeRequest: %v", err)
	}
	if got, err := ParseCommand(verb); err != nil || got != CommandPlay {
		t.Fatalf("verb came back as %q (%v)", verb, err)
	}
	if arg != camID {
		t.Fatalf("argument came back as %q, want %q", arg, camID)
	}
}
