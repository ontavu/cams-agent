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

package lanagent

import (
	"context"

	"github.com/jfsmig/go-wsd/wsd"
	"github.com/jfsmig/onvif/v2/networking"
	"github.com/ontavu/cams-go-utils/utils"
	"github.com/rs/zerolog"
)

// rescanBacklog is how many rescans may wait for a loop that is busy with one,
// an interface's or the configured cameras'. One: a second request while one
// waits would look at the same devices, so it is dropped rather than queued.
const rescanBacklog = 1

type Nic struct {
	ItfName string
	trigger chan uint32
}

func NewNIC(name string) *Nic {
	return &Nic{
		ItfName: name,
		trigger: make(chan uint32, rescanBacklog),
	}
}

func (ls *Nic) PK() string { return ls.ItfName }

func (ls *Nic) debug() *zerolog.Event { return utils.Logger.Debug().Str("itf", ls.PK()) }

func (ls *Nic) warn(err error) *zerolog.Event {
	return utils.Logger.Warn().Err(err).Str("itf", ls.PK())
}

type RegistrationFunc func(ctx context.Context, gen uint32, discovered []networking.ClientInfo)

func (ls *Nic) RunRescanLoop(ctx context.Context, register RegistrationFunc) {
	ls.debug().Msg("nic starting")
	for {
		select {

		case <-ctx.Done():
			ls.debug().Msg("nic stopping")
			// The channel is deliberately left open. TriggerRescanAsync may
			// still be called from the timer loop, which shares this ensemble
			// and has no ordering against us, and a send on a closed channel
			// panics. Nothing else holds the channel, so it is collected with
			// the Nic.
			return

		case generation := <-ls.trigger:
			// The zero ProbeOptions is the dialect ONVIF mandates, and the
			// context bounds the collection window, so a rescan in flight
			// ends with the agent rather than outliving it.
			devices, err := wsd.Discover(ctx, ls.ItfName, wsd.ProbeOptions{})
			if err != nil {
				ls.warn(err).Uint32("gen", generation).Msg("nic rescan failure")
				continue
			}
			if len(devices) > 0 {
				ls.debug().Uint32("gen", generation).Int("found", len(devices)).Msg("nic rescan success")
			}
			register(ctx, generation, clientsOf(devices))
		}
	}
}

// clientsOf narrows what discovery reports to what the ONVIF client needs.
// wsd.Device also carries the address the datagram came from and the service
// URL as advertised; neither is used here, and both are claims made by an
// unauthenticated datagram.
func clientsOf(devices []wsd.Device) []networking.ClientInfo {
	clients := make([]networking.ClientInfo, 0, len(devices))
	for _, dev := range devices {
		clients = append(clients, networking.ClientInfo{Xaddr: dev.Xaddr, Uuid: dev.UUID})
	}
	return clients
}

func (ls *Nic) TriggerRescanAsync(ctx context.Context, generation uint32) {
	select {
	case <-ctx.Done():
		// generate no trigger if waiting for an exit
	case ls.trigger <- generation:
		// try to write a trigger token but ...
	default:
		ls.debug().Uint32("gen", generation).Msg("nic rescan avoided")
		// ... never wait if there are already pending trigger tokens
	}
}
