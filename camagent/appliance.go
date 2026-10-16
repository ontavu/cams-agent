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

package camagent

import (
	"context"

	"github.com/jfsmig/onvif/v2/sdk"
	"github.com/juju/errors"
)

// Appliance is the part of an ONVIF device this package uses: the identity
// that names the camera, and the media profiles to choose a stream from.
//
// Narrower than sdk.Appliance, and declared here rather than taken from the
// SDK, because v2 reaches the media profiles through *sdk.ProfileS -- a
// concrete type over an unexported client, which no test can construct. The
// seam therefore belongs on the consumer side, which is also where the two
// calls the agent actually makes are visible at a glance.
type Appliance interface {
	GetUUID() string
	MediaProfiles(ctx context.Context) (sdk.MediaProfiles, error)
}

// FromSDK adapts an ONVIF appliance to what this package needs.
func FromSDK(dev sdk.Appliance) Appliance { return sdkAppliance{dev: dev} }

type sdkAppliance struct {
	dev sdk.Appliance
}

func (a sdkAppliance) GetUUID() string { return a.dev.GetUUID() }

// MediaProfiles asks the appliance's Profile S client for what it offers.
//
// An appliance advertising neither the device nor the media service has no
// Profile S client at all, and that is reported as an error rather than as an
// empty set: a camera that offers no media service is not a camera offering
// zero profiles, and the retry loop should say which of the two it met.
func (a sdkAppliance) MediaProfiles(ctx context.Context) (sdk.MediaProfiles, error) {
	profileS, ok := a.dev.ProfileS()
	if !ok {
		return sdk.MediaProfiles{}, errors.NotFoundf("ONVIF Profile S on the appliance")
	}
	return profileS.FetchMediaProfiles(ctx), nil
}
