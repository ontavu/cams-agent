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

package camagent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ontavu/cams-agent/camagent"
	"github.com/ontavu/cams-agent/errkind"
	"github.com/ontavu/onvif/v2/sdk"
)

// withoutProfileS is a device that advertises neither the device nor the
// media service. Nothing but ProfileS is asked of it.
type withoutProfileS struct{ sdk.Appliance }

func (withoutProfileS) ProfileS() (*sdk.ProfileS, bool) { return nil, false }

// A device with no Profile S is reported as such -- not found -- rather than
// as a camera that offers no profile.
func TestFromSDK_NoProfileSIsNotFound(t *testing.T) {
	_, err := camagent.FromSDK(withoutProfileS{}).MediaProfiles(context.Background())
	if !errors.Is(err, errkind.ErrNotFound) {
		t.Fatalf("MediaProfiles: %v, want ErrNotFound", err)
	}
}
