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

// Package agentbus carries the plain text commands that the internal agents of
// cams-agent exchange, as described in ARCHITECTURE.md.
//
// It holds what every agent and controller pair needs and none of what any
// single one of them means: the framing of a request and a reply, the request
// side of the bus, and the serving loop of the reply side. The vocabulary --
// which verbs exist and what they do -- belongs to the controller package of
// each agent.
//
// A request is one line, a verb and an optional argument:
//
//	PLAY
//	PLAY 3fa85f64-5717-4562-b3fc-2c963f66afa6
//
// A reply is one line too, either a success with an optional argument or a
// failure with a message:
//
//	OK
//	OK PLAYING
//	ERR no such camera
package agentbus

import (
	"strings"

	"github.com/juju/errors"
)

// The two leading tokens of a reply.
const (
	replyOK  = "OK"
	replyErr = "ERR"
)

// ErrRemote reports a failure that the peer itself sent back, as opposed to a
// failure of the transport. errors.Is makes the two distinguishable.
//
// Only the message crosses the bus, not the error's identity: a caller that
// needs to branch on a particular failure needs a code in the reply, which no
// agent has needed so far.
var ErrRemote = errors.New("the agent refused the command")

// EncodeRequest builds a request. The argument is optional, and a verb alone
// encodes to just the verb, so a vocabulary that has no arguments produces the
// bare words it always did.
func EncodeRequest(verb, arg string) []byte {
	if arg == "" {
		return []byte(verb)
	}
	return []byte(verb + " " + arg)
}

// DecodeRequest splits a request into its verb and its argument. Recognising
// the verb is left to the caller, which is the only side that knows its own
// vocabulary.
func DecodeRequest(payload []byte) (verb, arg string, err error) {
	fields := strings.SplitN(strings.TrimSpace(string(payload)), " ", 2)

	verb = fields[0]
	if verb == "" {
		return "", "", errors.New("empty request")
	}
	if len(fields) > 1 {
		arg = strings.TrimSpace(fields[1])
	}
	return verb, arg, nil
}

// EncodeOK builds a success reply, with an optional argument.
func EncodeOK(arg string) []byte {
	if arg == "" {
		return []byte(replyOK)
	}
	return []byte(replyOK + " " + arg)
}

// EncodeErr builds a failure reply out of an error. A nil error would produce a
// reply claiming a failure without naming one, so it names one anyway.
func EncodeErr(err error) []byte {
	if err == nil {
		return []byte(replyErr + " unspecified error")
	}
	// The message travels on one line: an annotation chain from juju/errors
	// contains newlines, and would be read back as a truncated reply.
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if msg == "" {
		msg = "unspecified error"
	}
	return []byte(replyErr + " " + msg)
}

// DecodeReply returns the argument of a success, or an error carrying what the
// peer reported. A reply that parses as neither a success nor a failure is an
// error too: returning a zero value there would let a garbled exchange pass for
// a successful one.
func DecodeReply(payload []byte) (string, error) {
	fields := strings.SplitN(strings.TrimSpace(string(payload)), " ", 2)

	var arg string
	if len(fields) > 1 {
		arg = strings.TrimSpace(fields[1])
	}

	switch fields[0] {
	case replyOK:
		return arg, nil
	case replyErr:
		if arg == "" {
			arg = "unspecified error"
		}
		return "", errors.Annotate(ErrRemote, arg)
	default:
		return "", errors.NotValidf("reply %q", string(payload))
	}
}
