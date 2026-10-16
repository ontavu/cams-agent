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
	"os/signal"
	"syscall"

	"github.com/jfsmig/onvif/v2/sdk"
	"github.com/juju/errors"
	"github.com/ontavu/cams-agent/camagent"
	"github.com/ontavu/cams-agent/camctrl"
	"github.com/ontavu/cams-agent/lanagent"
	"github.com/ontavu/cams-agent/lanctrl"
	"github.com/ontavu/cams-agent/upagent"
	"github.com/ontavu/cams-go-utils/utils"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
)

func main() {
	var flagSpeed bool
	var flagConfig, flagUser, flagControl, flagMedia string

	cmd := &cobra.Command{
		Use:   "cams-agent",
		Short: "Cams Agent",
		Long:  "LAN agent for OnVif cameras",
		//Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			zerolog.SetGlobalLevel(zerolog.TraceLevel)

			cfg, err := loadConfig(flagConfig, flagUser, flagControl, flagMedia)
			if err != nil {
				return err
			}
			if flagSpeed {
				cfg.RegisterPeriod = 1
				cfg.CheckPeriod = 1
			}
			ctx, cancel := signal.NotifyContext(
				context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()

			return runAgent(ctx, cfg)
		},
	}

	// Not the LAN rescan: this used to set ScanPeriod to zero as well, which
	// reads as "as fast as possible" and means the opposite -- lanagent treats
	// a period of zero as unset and puts its own default back. The first scan
	// runs at start-up either way, so what makes a camera appear quickly is the
	// one-second registration and not the rescan.
	cmd.Flags().BoolVarP(&flagSpeed, "speed", "s", false,
		"TEST: one-second registration and check loops; the LAN rescan is unaffected")

	// The four settings a deployment cannot avoid varying. Everything else the
	// agent has -- the interface patterns, the statically configured cameras,
	// the three periods -- is a line in the configuration file, because a flag
	// per field is a command line nobody can read and this process has fifteen.
	cmd.Flags().StringVarP(&flagConfig, "config", "f", "",
		"JSON configuration file; every field it omits keeps its default")
	cmd.Flags().StringVarP(&flagUser, "user", "u", "",
		"the account these cameras are registered under")
	cmd.Flags().StringVar(&flagControl, "control", "",
		"cams-ctrl, which carries the registrations and Play/Pause")
	cmd.Flags().StringVar(&flagMedia, "media", "",
		"cams-rtp2hls, which the media is uploaded to")

	if err := cmd.Execute(); err != nil {
		utils.Logger.Fatal().Err(err).Str("action", "aborting").Msg("agent")
	} else {
		utils.Logger.Info().Str("action", "Exiting").Msg("agent")
	}
}

// loadConfig resolves the configuration from the four places it can come from,
// in the order a deployment expects: the compiled-in defaults, then the file,
// then the environment, then the flags. Later wins, and the two that carry a
// credential -- the environment and the file -- are the two a command line does
// not expose.
//
// This closes the FIXME that stood here for as long as the agent existed.
// AgentConfig could already parse a file and nothing called it, so every one of
// its fields was reachable only by editing DefaultConfig() and rebuilding: the
// agent ran as user "plop" against a hub on its own loopback, which is precisely
// the arrangement a LAN agent is not in.
func loadConfig(path, user, control, media string) (AgentConfig, error) {
	cfg := DefaultConfig()

	if path != "" {
		if err := cfg.LoadFile(path); err != nil {
			return cfg, errors.Annotatef(err, "config %q", path)
		}
	}

	cfg.LoadEnv()

	if user != "" {
		cfg.User = user
	}
	if control != "" {
		cfg.UpstreamControl.Address = control
	}
	if media != "" {
		cfg.UpstreamMedia.Address = media
	}

	// The two upstreams are two processes on two ports and are not
	// interchangeable -- cams-ctrl answers MediaUpload with Unimplemented on
	// purpose -- so an agent pointed at one address for both uploads nothing
	// and says nothing about why.
	if cfg.UpstreamControl.Address == cfg.UpstreamMedia.Address {
		return cfg, errors.Errorf(
			"control and media are the same address (%s); they are two processes",
			cfg.UpstreamControl.Address)
	}

	return cfg, nil
}

// camBusURL and camMediaURL are the bus endpoints of one camera: the first
// carries its commands, the second its media. The inproc registry is
// process-global and matched on exact equality, so the names are kept under a
// prefix of ours.
func camBusURL(camID string) string   { return "inproc://cams/control/" + camID }
func camMediaURL(camID string) string { return "inproc://cams/media/" + camID }

// cameraBundle is one camera as the LAN agent sees it: the agent that captures
// the media and the upstream that forwards it, started and stopped together.
//
// The two halves are paired here because the LAN agent owns cameras, not media
// upstreams, and neither half is useful without the other.
type cameraBundle struct {
	agent *camagent.Agent
	sink  *grpcMediaSink
}

// Run serves both halves and returns when either is done. An ensemble rather
// than a swarm: a camera whose upstream has stopped has nowhere to push, and an
// upstream whose camera has exited has nothing to forward.
func (b *cameraBundle) Run(ctx context.Context) {
	utils.EnsembleRun(ctx, b.sink.Run, b.agent.Run)
}

// Close releases both halves. It is what the LAN agent calls for a camera it
// built and then decided not to start.
func (b *cameraBundle) Close() error {
	// Both are attempted whatever the first one says, since both hold an
	// endpoint that would otherwise leak.
	agentErr := b.agent.Close()
	sinkErr := b.sink.Close()
	if agentErr != nil {
		return errors.Annotate(agentErr, "camera agent")
	}
	return errors.Annotate(sinkErr, "media sink")
}

// newCameraFactory is the only place in the process that knows how a camera is
// assembled. Handing this closure to the LAN agent is what keeps camagent out
// of every other package.
func newCameraFactory(cfg AgentConfig) lanagent.CameraFactory {
	return func(appliance sdk.Appliance) (camctrl.CameraController, lanagent.CameraAgent, error) {
		camID := appliance.GetUUID()
		busURL := camBusURL(camID)
		mediaURL := camMediaURL(camID)

		// The media upstream binds first: it is the passive side, and the
		// camera dials it once it has a stream to push.
		sink, err := NewGrpcMediaSink(cfg.User, camID, cfg.UpstreamMedia.Address, mediaURL)
		if err != nil {
			return nil, nil, errors.Annotate(err, "media sink")
		}

		// The agent binds too, so the controller below can dial right away.
		agent, err := camagent.New(camagent.FromSDK(appliance), busURL, mediaURL,
			camagent.WithCredentials(cfg.CameraUser, cfg.CameraPassword))
		if err != nil {
			if cerr := sink.Close(); cerr != nil {
				utils.Logger.Warn().Err(cerr).Str("cam", camID).Msg("media sink close")
			}
			return nil, nil, errors.Annotate(err, "camera agent")
		}

		bundle := &cameraBundle{agent: agent, sink: sink}

		ctl, err := camctrl.New(camID, busURL)
		if err != nil {
			if cerr := bundle.Close(); cerr != nil {
				utils.Logger.Warn().Err(cerr).Str("cam", camID).Msg("camera close")
			}
			return nil, nil, errors.Annotate(err, "camera controller")
		}

		return ctl, bundle, nil
	}
}

// lanBusURL is the bus endpoint of the LAN agent. The inproc registry is
// process-global and matched on exact equality, so the names are kept under a
// prefix of ours.
const (
	lanBusURL = "inproc://cams/lan"
	upBusURL  = "inproc://cams/upstream"
)

func runAgent(ctx context.Context, cfg AgentConfig) error {
	// The LAN agent binds its endpoint here, so the controller below can dial
	// it before anything is scheduled.
	lan, err := lanagent.New(cfg.lan(), newCameraFactory(cfg), lanBusURL)
	if err != nil {
		return errors.Annotate(err, "lan agent")
	}

	lanCli, err := lanctrl.New(lanBusURL)
	if err != nil {
		if cerr := lan.Close(); cerr != nil {
			utils.Logger.Warn().Err(cerr).Msg("lan agent close")
		}
		return errors.Annotate(err, "lan controller")
	}
	defer func() {
		if cerr := lanCli.Close(); cerr != nil {
			utils.Logger.Warn().Err(cerr).Msg("lan controller close")
		}
	}()

	// The upstream agent reaches the LAN through its controller and knows
	// nothing else about it.
	upstream, err := upagent.New(cfg.upstream(), lanCli, upBusURL)
	if err != nil {
		return errors.Annotate(err, "upstream agent")
	}

	utils.Logger.Info().Str("action", "starting").Msg("agent")

	utils.EnsembleRun(ctx, upstream.Run, lan.Run)

	return nil
}
