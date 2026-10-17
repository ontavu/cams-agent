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

package upctrl

import (
	"errors"
	"strings"
	"testing"

	"github.com/ontavu/cams-agent/agentbus"
)

func TestParseCommand_RoundTrip(t *testing.T) {
	for _, cmd := range []Command{CommandPing, CommandState} {
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

// TestParseCommand_Rejected names what this agent deliberately does not accept.
// PLAY and PAUSE reach it from the hub, not from the bus; EXIT does not exist
// because the agent lives as long as the process.
func TestParseCommand_Rejected(t *testing.T) {
	for _, verb := range []string{"", "ping", "PINGS", "PLAY", "PAUSE", "EXIT", "LIST", "OK"} {
		if got, err := ParseCommand(verb); err == nil {
			t.Fatalf("ParseCommand(%q) = %q, want an error", verb, string(got))
		}
	}
}

func TestParseState_RoundTrip(t *testing.T) {
	for _, st := range []State{StateDown, StateUp} {
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
	for _, token := range []string{"", "up", "down", "CONNECTED", "IDLE"} {
		if got, err := ParseState(token); err == nil {
			t.Fatalf("ParseState(%q) = %q, want an error", token, string(got))
		}
	}
}

// TestState_SurvivesAReply is the round trip CommandState depends on: each
// state has to come back out of a reply as itself.
func TestState_SurvivesAReply(t *testing.T) {
	for _, st := range []State{StateDown, StateUp} {
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

// TestParseCommand_AfterDecodeRequest pins the two halves together: whatever
// framing agentbus strips, the verb it hands over must still be recognised.
func TestParseCommand_AfterDecodeRequest(t *testing.T) {
	for _, payload := range []string{"STATE", "STATE\n", " STATE ", "\tSTATE\t"} {
		verb, arg, err := agentbus.DecodeRequest([]byte(payload))
		if err != nil {
			t.Fatalf("DecodeRequest(%q): %v", payload, err)
		}
		if arg != "" {
			t.Fatalf("DecodeRequest(%q) produced argument %q", payload, arg)
		}
		if got, err := ParseCommand(verb); err != nil || got != CommandState {
			t.Fatalf("payload %q became %q (%v)", payload, string(got), err)
		}
	}
}

func TestReply_Error(t *testing.T) {
	payload := agentbus.EncodeErr(errors.New("upstream is unwell"))

	if _, err := agentbus.DecodeReply(payload); err == nil {
		t.Fatal("a failure reply decoded as a success")
	} else if !strings.Contains(err.Error(), "upstream is unwell") {
		t.Fatalf("error %v lost the message reported by the agent", err)
	}
}

// TestNew_NoAgent is why the dial is synchronous: a controller for an agent
// that is not there has to say so at once.
func TestNew_NoAgent(t *testing.T) {
	if cli, err := New("inproc://cams/test/up-absent"); err == nil {
		_ = cli.Close()
		t.Fatal("dialling an address nobody serves succeeded")
	}
}
