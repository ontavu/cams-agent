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

package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jfsmig/onvif/v2/networking"
	"github.com/jfsmig/onvif/v2/sdk"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/lanagent"
	"github.com/ontavu/cams-agent/mediabus"
)

// testAppliance is a device as far as building a camera looks at one: its
// identifier. Nothing here plays, so nothing asks it for its profiles.
type testAppliance struct {
	sdk.Appliance
	uuid string
}

func (a testAppliance) GetUUID() string { return a.uuid }

// closedAddress is a loopback address nothing listens on, so that an upload
// fails the way it does when the hub is down: refused, at once.
func closedAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return addr
}

func closeCamera(t *testing.T, ctl camctrl.CameraController, agent lanagent.CameraAgent) {
	t.Helper()
	if err := ctl.Close(); err != nil {
		t.Errorf("controller close: %v", err)
	}
	if err := agent.Close(); err != nil {
		t.Errorf("bundle close: %v", err)
	}
}

// When a bundle's Run returns, both of its inproc endpoints are free: the
// camera can be built again under the same identifier straight away. The LAN
// agent relies on it, since it forgets a camera as soon as Run returns and the
// next round rebuilds it -- a rebuild that would otherwise fail on an address
// still in use, every round, for good.
func TestCameraBundle_ReleasesBothEndpointsWhenItStops(t *testing.T) {
	for i, tc := range []struct {
		name string
		stop func(t *testing.T, camID string, ctl camctrl.CameraController, cancel context.CancelFunc) (cleanup func())
	}{
		{
			// The case that motivated forget: the media sink returns, and the
			// ensemble stops the camera agent with it.
			name: "the upload broke",
			stop: func(t *testing.T, camID string, _ camctrl.CameraController, _ context.CancelFunc) func() {
				pusher, err := mediabus.DialPush("test", camMediaURL(camID))
				if err != nil {
					t.Fatalf("dial the media bus: %v", err)
				}
				if err := pusher.Send(mediabus.FrameSDP, 0, []byte("v=0\r\n")); err != nil {
					t.Fatalf("send the banner: %v", err)
				}
				// Closed only once the bundle has returned: a mangos close
				// discards what is still queued.
				return func() {
					if err := pusher.Close(); err != nil {
						t.Errorf("pusher close: %v", err)
					}
				}
			},
		},
		{
			// The camera agent returns, and the ensemble stops the sink with it.
			name: "the camera agent exited",
			stop: func(t *testing.T, _ string, ctl camctrl.CameraController, _ context.CancelFunc) func() {
				if err := ctl.Exit(); err != nil {
					t.Fatalf("exit: %v", err)
				}
				return func() {}
			},
		},
		{
			name: "the agent is stopping",
			stop: func(_ *testing.T, _ string, _ camctrl.CameraController, cancel context.CancelFunc) func() {
				cancel()
				return func() {}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			camID := fmt.Sprintf("urn:uuid:bundle-%d-%d", i, time.Now().UnixNano())
			cfg := DefaultConfig()
			cfg.UpstreamMedia.Address = closedAddress(t)
			build := newCameraFactory(cfg)

			ctl, agent, err := build(testAppliance{uuid: camID}, networking.ClientAuth{})
			if err != nil {
				t.Fatalf("build: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				agent.Run(ctx)
			}()

			cleanup := tc.stop(t, camID, ctl, cancel)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the bundle did not return")
			}
			cleanup()
			if err := ctl.Close(); err != nil {
				t.Errorf("controller close: %v", err)
			}

			ctl, agent, err = build(testAppliance{uuid: camID}, networking.ClientAuth{})
			if err != nil {
				t.Fatalf("the camera cannot be built again once its bundle returned: %v", err)
			}
			closeCamera(t, ctl, agent)
		})
	}
}
