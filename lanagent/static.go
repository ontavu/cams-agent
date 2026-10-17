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
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/ontavu/cams-go-utils/utils"
	"github.com/ontavu/onvif/v2/networking"
)

// The cameras the configuration names are learned by a loop of their own,
// driven by the same rescan tick as the interfaces. Each round contacts the ones
// not known yet, so a camera that was down when the agent started is picked up
// once it answers. One that is known is not contacted again: its camera agent
// owns the link to it from then on, and if that agent stops, the camera is
// forgotten and the next round learns it again.
//
// Discovery leaves their addresses alone. Were it to reach one first, it would
// learn the camera under the identifier WS-Discovery reported and with the
// agent's credentials, and the configured uuid and password would be ignored
// for good -- or not, depending on which loop happened to get there first.

const (
	// onvifDefaultPort is where the device service is reached when the address
	// names no port, since its URL is built as http://<address>/onvif/....
	onvifDefaultPort = "80"
)

// validateStaticCameras refuses the configured cameras that cannot work, and
// returns the set of their addresses in canonicalXaddr's form.
//
// An address is checked the way the ONVIF client will use it, as the host of
// http://<address>/onvif/device_service: anything that does not parse there
// would fail every round, for as long as the agent runs, with a warning each
// time. Two cameras on one address or under one uuid are refused too: the
// second would be taken for the first and never contacted.
func validateStaticCameras(cams []CameraConfig) (map[string]bool, error) {
	addresses := make(map[string]bool, len(cams))
	uuids := make(map[string]bool, len(cams))
	for _, cam := range cams {
		if err := validateStaticCamera(cam); err != nil {
			return nil, err
		}
		xaddr := canonicalXaddr(cam.Address)
		if addresses[xaddr] {
			return nil, fmt.Errorf("two cameras at %s", xaddr)
		}
		addresses[xaddr] = true
		if cam.UUID != "" {
			if uuids[cam.UUID] {
				return nil, fmt.Errorf("two cameras with uuid %q", cam.UUID)
			}
			uuids[cam.UUID] = true
		}
	}
	return addresses, nil
}

func validateStaticCamera(cam CameraConfig) error {
	switch {
	case cam.Address == "":
		return errors.New("configured camera without an address")
	case strings.Contains(cam.Address, "://"), strings.Contains(cam.Address, "/"):
		return fmt.Errorf("camera address %q: want host or host:port, not a URL", cam.Address)
	case strings.ContainsAny(cam.UUID, " \t\r\n"):
		// A LIST reply separates identifiers with spaces.
		return fmt.Errorf("camera uuid %q contains a blank", cam.UUID)
	}

	u, err := url.Parse("http://" + cam.Address + "/onvif/device_service")
	if err != nil || u.Host != cam.Address || u.Hostname() == "" || strings.HasSuffix(cam.Address, ":") {
		return fmt.Errorf("camera address %q: want host or host:port, with an IPv6 address in brackets", cam.Address)
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("camera address %q: port %q is not a port", cam.Address, port)
		}
	}
	return nil
}

// canonicalXaddr is the form two addresses of a device are compared in: the
// port made explicit, the host lower-cased. Discovery reports a device on port
// 80 without its port, and a configuration may name it either way.
func canonicalXaddr(xaddr string) string {
	host, port, err := net.SplitHostPort(xaddr)
	if err != nil {
		// No port: a name, an IPv4 address, or an IPv6 one with or without its
		// brackets.
		host, port = strings.TrimSuffix(strings.TrimPrefix(xaddr, "["), "]"), onvifDefaultPort
	}
	return net.JoinHostPort(strings.ToLower(host), port)
}

// authFor is the credentials a configured camera is reached with: its own where
// it has them, the agent's otherwise, one field at a time.
func (lan *Agent) authFor(cam CameraConfig) networking.ClientAuth {
	auth := networking.ClientAuth{Username: lan.cfg.User, Password: lan.cfg.Password}
	if cam.User != "" {
		auth.Username = cam.User
	}
	if cam.Password != "" {
		auth.Password = cam.Password
	}
	return auth
}

// runStaticLoop learns the configured cameras, one round per rescan, until the
// context ends.
func (lan *Agent) runStaticLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case gen := <-lan.staticTrigger:
			lan.learnStaticCamerasSync(ctx, gen)
		}
	}
}

// triggerStaticAsync asks the static loop for a round, and never waits: a round
// already pending covers every camera this one would.
func (lan *Agent) triggerStaticAsync(ctx context.Context, gen uint32) {
	select {
	case <-ctx.Done():
	case lan.staticTrigger <- gen:
	default:
		utils.Logger.Debug().Uint32("gen", gen).Msg("static rescan avoided")
	}
}

// learnStaticCamerasSync contacts each configured camera that is not known yet.
//
// There is no purge here. A configured camera is never forgotten, and the
// discovered ones are purged by the round that discovers, as before.
func (lan *Agent) learnStaticCamerasSync(ctx context.Context, gen uint32) {
	for _, cam := range lan.devicesStatic {
		if ctx.Err() != nil {
			return
		}
		lan.learnStaticCamera(ctx, gen, cam)
	}
}

func (lan *Agent) learnStaticCamera(ctx context.Context, gen uint32, cam CameraConfig) {
	contactCtx, cancel := context.WithTimeout(ctx, lan.contactTimeout)
	defer cancel()

	info := networking.ClientInfo{Xaddr: cam.Address, Uuid: cam.UUID}
	err := lan.learnSingleCameraSync(contactCtx, gen, info, lan.authFor(cam), true)
	if err == nil || ctx.Err() != nil {
		// A camera interrupted because the agent is stopping is not
		// unreachable, and saying so would be the last thing in the log.
		return
	}
	// Nothing more to do than say so: the next round tries again.
	utils.Logger.Warn().Str("address", cam.Address).Uint32("gen", gen).Err(err).
		Msg("configured camera unreachable")
}
