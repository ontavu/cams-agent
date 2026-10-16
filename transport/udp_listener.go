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

package transport

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/juju/errors"
	"golang.org/x/sync/errgroup"
)

type StreamListener interface {
	Close() error
	GetMediaChannel() <-chan []byte
	GetControlChannel() <-chan []byte
}

type UdpListener interface {
	StreamListener
	GetPortMedia() int
	GetPortControl() int
	OpenRandom(addr string) error
	OpenPair(addr string) error
	Run(ctx context.Context) error
}

func NewRawUdpListener() UdpListener {
	return &rawUdpListener{}
}

type rawUdpListener struct {
	media   net.PacketConn
	control net.PacketConn

	mediaOut   chan []byte
	controlOut chan []byte

	singletonLock sync.Mutex
}

func (ul *rawUdpListener) GetMediaChannel() <-chan []byte {
	return ul.mediaOut
}

func (ul *rawUdpListener) GetControlChannel() <-chan []byte {
	return ul.controlOut
}

func (ul *rawUdpListener) GetPortMedia() int {
	return getPort(ul.media)
}

func (ul *rawUdpListener) GetPortControl() int {
	return getPort(ul.control)
}

func (ul *rawUdpListener) OpenRandom(addr string) error {
	if !ul.isClear() {
		return errors.New("opening an opened listener")
	}
	if !ul.singletonLock.TryLock() {
		return errors.New("opening a running listener")
	}
	defer ul.release()

	ul.init()

	var err error
	ul.media, err = openPort(addr, 0)
	if err != nil {
		return errors.Trace(err)
	}

	ul.control, err = openPort(addr, 0)
	if err != nil {
		ul.media.Close()
		return errors.Trace(err)
	}

	return nil
}

func (ul *rawUdpListener) OpenPair(addr string) error {
	if !ul.isClear() {
		return errors.New("opening an opened listener")
	}
	if !ul.singletonLock.TryLock() {
		return errors.New("opening a running listener")
	}
	defer ul.release()

	ul.init()

	var err error
	for port := 0; ; port++ {
		ul.media, err = openPort(addr, port)
		if err != nil {
			return errors.Trace(err)
		}
		if port == 0 {
			port = getPort(ul.media)
		}

		ul.control, err = openPort(addr, port+1)
		if err == nil {
			return nil
		}

		ul.media.Close()
	}
}

func (ul *rawUdpListener) Close() error {
	if ul == nil {
		return nil
	}

	if !ul.singletonLock.TryLock() {
		panic("listener still in use")
	}
	defer ul.release()

	if ul.media != nil {
		ul.media.Close()
		ul.media = nil
	}
	if ul.control != nil {
		ul.control.Close()
		ul.control = nil
	}

	return nil
}

func (ul *rawUdpListener) Run(ctx context.Context) error {
	if err := ul.acquire(); err != nil {
		return errors.Trace(err)
	}
	defer ul.release()

	g, ctx := errgroup.WithContext(ctx)
	getBuffer := func() []byte { return make([]byte, 8192) }

	runCnx := func(cnx net.PacketConn, out chan []byte) error {
		buf := getBuffer()
		for ctx.Err() == nil {
			_ = cnx.SetDeadline(time.Now().Add(time.Second))
			if n, _, err := cnx.ReadFrom(buf); err != nil {
				if err, ok := err.(net.Error); ok && err.Timeout() {
					continue
				}
				return errors.Trace(err)
			} else {
				out <- buf[:n]
				buf = getBuffer()
			}
		}
		return nil
	}
	g.Go(func() error { return runCnx(ul.media, ul.mediaOut) })
	g.Go(func() error { return runCnx(ul.control, ul.controlOut) })
	return g.Wait()
}

func (ul *rawUdpListener) init() {
	ul.mediaOut = make(chan []byte, 512)
	ul.controlOut = make(chan []byte, 8)
}

func (ul *rawUdpListener) isOk() bool {
	return ul != nil && ul.media != nil && ul.control != nil
}

func (ul *rawUdpListener) isClear() bool {
	return ul == nil || ul.media == nil && ul.control == nil
}

func (ul *rawUdpListener) acquire() error {
	if !ul.isOk() {
		return errors.New("udp listener invalid state")
	}
	if !ul.singletonLock.TryLock() {
		log.Panicln("here", ul, ul.media, ul.control)
		return errors.New("udp listener running")
	}
	return nil
}

func (ul *rawUdpListener) release() {
	if ul != nil {
		ul.singletonLock.Unlock()
	}
}

func openPort(addr string, port int) (net.PacketConn, error) {
	cnx, err := net.ListenPacket("udp", fmt.Sprintf("%s:%d", addr, port))
	return cnx, errors.Trace(err)
}

func getPort(cnx net.PacketConn) int {
	if cnx == nil {
		return -1
	}
	return cnx.LocalAddr().(*net.UDPAddr).Port
}
