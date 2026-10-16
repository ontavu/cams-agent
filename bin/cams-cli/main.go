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

	"github.com/ontavu/cams-go-utils/utils"
	"github.com/spf13/cobra"
)

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
		Short: "Commands targetting the hub",
	}

	cmdHubPlay := &cobra.Command{
		Use:   "play",
		Short: "Play a stream",
		Long:  "Contact the Cams Hub and download a stream given its User/Stream ID",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return hubPlay(ctx, "127.0.0.1:6000", args[0], args[1])
		},
	}

	cmdCam := &cobra.Command{
		Use:   "cam",
		Short: "Commands targeting a camera",
	}

	var camInterface string
	cmdCamPlay := &cobra.Command{
		Use:   "play",
		Short: "play",
		Args:  cobra.ExactArgs(1),
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
