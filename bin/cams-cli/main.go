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
	"os/signal"
	"syscall"

	"github.com/ontavu/cams-go-utils/utils"
	"github.com/spf13/cobra"
)

// defaultControlAddr is the hub's control endpoint when --control is not
// given: the same default as cams-agent's, a hub on this machine.
const defaultControlAddr = "127.0.0.1:6000"

func main() {
	ctx, cancel := signal.NotifyContext(
		context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	cmd := &cobra.Command{
		Use:   "cams-cli",
		Short: "Cams command Line Interface",
		Long:  "CLI Client for Cams services",
	}

	cmdHub := &cobra.Command{
		Use:   "hub",
		Short: "Commands targeting the hub",
	}

	var hubControl string
	cmdHubPlay := &cobra.Command{
		Use:   "play <user> <camera>",
		Short: "Ask the hub to have an agent upload a camera's stream",
		Long: "Ask the hub, through its Viewer service, to have the agent of <user> upload the " +
			"stream of <camera>: the identifier the camera is registered under, which " +
			"cams-agent logs as the key of each device it adds. Nothing is downloaded here.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return hubPlay(ctx, hubControl, args[0], args[1])
		},
	}
	cmdHubPlay.Flags().StringVar(&hubControl, "control", defaultControlAddr,
		"the hub's control endpoint, host:port")

	cmdCam := &cobra.Command{
		Use:   "cam",
		Short: "Commands targeting a camera",
	}

	var camInterface string
	cmdCamPlay := &cobra.Command{
		Use:   "play <host[:port]>",
		Short: fmt.Sprintf("Record %v of a camera into a capture archive, with no hub", captureDuration),
		Long: fmt.Sprintf("Find the camera at <host[:port]> by WS-Discovery and record %v of its "+
			"stream into a capture archive in the temporary directory, whose path is logged. "+
			"The address is the one the camera advertises, exactly: cams-agent logs it as the "+
			"endpoint of each device it adds. The ONVIF credentials come from %s, admin by "+
			"default, and %s, which is required.",
			captureDuration, envCamUser, envCamPassword),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return camPlay(ctx, args[0], camInterface)
		},
	}
	cmdCamPlay.Flags().StringVar(&camInterface, "interface", "",
		"which network interface to send the discovery probe on; empty probes them all")

	cmdCam.AddCommand(cmdCamPlay)
	cmdHub.AddCommand(cmdHubPlay)
	cmd.AddCommand(cmdHub, cmdCam)

	if err := cmd.Execute(); err != nil {
		utils.Logger.Fatal().Err(err).Msg("Aborting")
	} else {
		utils.Logger.Info().Msg("Exiting")
	}
}
