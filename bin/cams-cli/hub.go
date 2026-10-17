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

	"github.com/ontavu/cams-api/pb"
	"github.com/ontavu/cams-api/wire"
	"github.com/ontavu/cams-go-utils/utils"
)

func hubPlay(ctx context.Context, address, userID, streamID string) error {
	cnx, err := utils.DialInsecure(ctx, address, wire.DialOptions()...)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer cnx.Close()

	client := pb.NewViewerClient(cnx)
	_, err = client.Play(ctx, &pb.PlayRequest{
		Id: &pb.StreamId{User: userID, Stream: streamID},
	})
	return err
}
