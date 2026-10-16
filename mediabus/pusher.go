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

package mediabus

import (
	"time"

	"github.com/juju/errors"
	"go.nanomsg.org/mangos/v3"
	"go.nanomsg.org/mangos/v3/protocol/push"

	// Registers the "inproc" scheme. Without it every Dial fails with
	// mangos.ErrBadTran.
	_ "go.nanomsg.org/mangos/v3/transport/inproc"
)

const (
	// DefaultQueueDepth is how many frames may wait to be picked up by the
	// upstream. It is the bound on the media held in memory, and it is the same
	// depth the channel it replaces used to have.
	//
	// The real slack is slightly larger: the inproc pipe itself is unbuffered,
	// but the puller has a queue of its own on the other side.
	DefaultQueueDepth = 512

	// DefaultSendTimeout is how long a send may wait on a full queue before the
	// frame is reported as an overflow.
	//
	// It has to be short, because a caller is typically an RTSP reading
	// goroutine and stalling one delays the whole session. It cannot be zero:
	// contrary to its documentation, mangos treats a zero or negative send
	// deadline as "no deadline" and blocks forever, so there is no such thing
	// as a non-blocking send here.
	DefaultSendTimeout = 2 * time.Millisecond
)

// Pusher is the active side of the media bus. Its Send is safe for concurrent
// use, which is the whole reason this replaces a channel and a draining
// goroutine: mangos serialises the frames internally, so the RTP and the RTCP
// callbacks of an RTSP client may both push directly.
type Pusher struct {
	name string
	sock mangos.Socket
}

// Option tunes a Pusher at construction.
type Option func(*settings)

type settings struct {
	queueDepth  int
	sendTimeout time.Duration
}

// WithQueueDepth overrides DefaultQueueDepth.
func WithQueueDepth(n int) Option {
	return func(s *settings) { s.queueDepth = n }
}

// WithSendTimeout overrides DefaultSendTimeout.
func WithSendTimeout(d time.Duration) Option {
	return func(s *settings) { s.sendTimeout = d }
}

// DialPush connects to the upstream listening at connectURL.
//
// The upstream must already be bound: the dial is synchronous, and that is
// deliberate. An upstream that has stopped is reported here, immediately, as
// mangos.ErrConnRefused, which is how a camera learns that there is no point
// starting a stream.
//
// name only labels the log lines.
func DialPush(name, connectURL string, opts ...Option) (*Pusher, error) {
	cfg := settings{
		queueDepth:  DefaultQueueDepth,
		sendTimeout: DefaultSendTimeout,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	sock, err := push.NewSocket()
	if err != nil {
		return nil, errors.Annotate(err, "push socket")
	}

	// Every option is set before the dial, and none afterwards. Changing the
	// queue depth of a live socket races with the senders in mangos itself:
	// SendMsg reads the queue channel without the lock while SetOption replaces
	// it under the lock.
	//
	// OptionBestEffort is deliberately absent, and this is not an oversight. It
	// looks like exactly what a media path wants -- drop instead of blocking --
	// but mangos implements it by making the discard branch of a select
	// permanently ready, so it competes with the enqueue branch even when the
	// queue is empty. A select picks uniformly among ready cases, so it discards
	// about half of all frames whatever the load, and reports success for each.
	//
	// Measured against v3.4.2 with a puller that was keeping up: of 2000 frames
	// sent, 999 arrived and 1001 vanished, with not one error returned.
	options := []struct {
		name  string
		value interface{}
	}{
		{mangos.OptionWriteQLen, cfg.queueDepth},
		{mangos.OptionSendDeadline, cfg.sendTimeout},
		// Say so at once when the upstream is gone, rather than filling the
		// queue and then timing out one frame at a time.
		{mangos.OptionFailNoPeers, true},
	}
	for _, opt := range options {
		if err := sock.SetOption(opt.name, opt.value); err != nil {
			_ = sock.Close()
			return nil, errors.Annotatef(err, "set %s", opt.name)
		}
	}

	if err := sock.Dial(connectURL); err != nil {
		_ = sock.Close()
		return nil, errors.Annotatef(err, "dial %s", connectURL)
	}

	return &Pusher{name: name, sock: sock}, nil
}

// Close releases the socket.
//
// Whatever is still queued is discarded: mangos abandons the write queue and
// does not honour a linger. For media that is the right trade -- the tail of a
// stream that is ending is worth less than a prompt teardown -- but it does
// mean the last frames of an attempt may never arrive.
func (p *Pusher) Close() error {
	return errors.Trace(p.sock.Close())
}

// Send hands one frame to the upstream. track names the media the payload
// belongs to; a session description carries 0.
//
// A full queue costs the caller up to the send timeout and then returns an
// error for which IsOverflow is true. Anything else means the link is done.
func (p *Pusher) Send(t FrameType, track byte, payload []byte) error {
	if !t.Valid() {
		return errors.NotValidf("media frame type %d", byte(t))
	}

	// One pooled message, filled in place: the alternative, Send([]byte),
	// allocates and copies once more for nothing.
	msg := mangos.NewMessage(frameHeaderLen + len(payload))
	msg.Body = append(msg.Body, byte(t), track)
	msg.Body = append(msg.Body, payload...)

	if err := p.sock.SendMsg(msg); err != nil {
		// On a failure the message is still ours, so it goes back to the pool.
		// On success it belongs to mangos and must not be touched.
		msg.Free()
		return errors.Annotatef(err, "push %s", t)
	}
	return nil
}
