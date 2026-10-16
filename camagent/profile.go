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
	"sort"
	"strings"

	"github.com/jfsmig/onvif/v2/sdk"
)

// ONVIF spells the video encodings this way in VideoEncoderConfiguration.
// These are the wire contract, not a local convention.
const (
	encodingH264 = "H264"
	encodingH265 = "H265"
)

// minSubstreamHeight is the shortest frame worth preferring for being small.
//
// The browser path wants the substream. A camera's main stream is commonly
// 4 Mbps at 2560x1920 where its substream is a few hundred kbps, the hub keeps
// every fragment it is sent, and a grid of cameras decodes far more easily at
// the smaller size. But cameras also offer very small profiles -- 320x240
// mobile streams -- and preferring the smallest with no floor would land on
// one of those. So the rule is the smallest frame that is still this tall.
//
// A constant rather than configuration for now. CameraConfig is where it would
// go when a camera needs to disagree.
const minSubstreamHeight = 360

// videoProfile is one of a camera's ONVIF media profiles, reduced to what
// choosing between them needs.
type videoProfile struct {
	token    string
	encoding string
	width    int
	height   int

	// gop is the camera's advertised GOP length in frames, zero when it
	// reports none. ONVIF only carries it for H.264.
	gop int

	uri string
}

func (p videoProfile) area() int { return p.width * p.height }

// videoProfilesOf flattens what the SDK hands back.
//
// The shape it arrives in is why any of this exists: FetchMediaProfiles
// returns a map, and the SDK's own FetchStreamURI picks one entry out of it.
// It sorts the tokens today, but it used to answer with whichever key map
// iteration reached first, so the same camera served another codec, another
// resolution and another bitrate on every reconnect -- and reconnects happen a
// second apart. Choosing here is what makes the answer ours and repeatable.
func videoProfilesOf(offered sdk.MediaProfiles) []videoProfile {
	out := make([]videoProfile, 0, len(offered.Profiles))
	for token, p := range offered.Profiles {
		if p == nil {
			continue
		}
		enc := p.Profile.VideoEncoderConfiguration
		out = append(out, videoProfile{
			token:    string(token),
			encoding: strings.ToUpper(strings.TrimSpace(string(enc.Encoding))),
			width:    int(enc.Resolution.Width),
			height:   int(enc.Resolution.Height),
			gop:      int(enc.H264.GovLength),
			uri:      strings.TrimSpace(string(p.Uris.Stream.Uri)),
		})
	}
	return out
}

// encodingRank orders the encodings by what the hub can carry. H.264 remuxes
// into fMP4 and plays everywhere; H.265 remuxes too but only Safari and a
// hardware-decoding Chrome will play it; anything else needs a transcoder that
// is not built.
func encodingRank(encoding string) int {
	switch encoding {
	case encodingH264:
		return 0
	case encodingH265:
		return 1
	default:
		return 2
	}
}

// chooseProfile picks which of a camera's profiles to pull.
//
// The order is total and depends on nothing but the profiles themselves, so two
// calls against one camera agree -- which is the property the SDK's own
// selection lacks, and the reason a camera could change codec between two
// reconnects.
//
//  1. A profile with no stream URI is not a candidate.
//  2. Only the best encoding on offer competes: an H.264 main stream beats an
//     H.265 substream, because a codec the browser cannot play is worth less
//     than the bytes saved.
//  3. Among those, the smallest frame at least minSubstreamHeight tall. If none
//     reaches that, the largest of what is left -- a camera offering only
//     320x240 should still be watchable.
//  4. The token breaks any remaining tie, so the answer never depends on the
//     order the profiles arrived in.
func chooseProfile(offered []videoProfile) (videoProfile, bool) {
	candidates := make([]videoProfile, 0, len(offered))
	best := len(offered) + 1
	for _, p := range offered {
		if p.uri == "" {
			continue
		}
		if r := encodingRank(p.encoding); r < best {
			best = r
		}
		candidates = append(candidates, p)
	}
	if len(candidates) == 0 {
		return videoProfile{}, false
	}

	kept := candidates[:0]
	for _, p := range candidates {
		if encodingRank(p.encoding) == best {
			kept = append(kept, p)
		}
	}

	// Whether anything reaches the floor decides which way size is read: below
	// it, bigger is better.
	tallEnough := false
	for _, p := range kept {
		if p.height >= minSubstreamHeight {
			tallEnough = true
			break
		}
	}

	sort.Slice(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if tallEnough {
			ai := a.height >= minSubstreamHeight
			bi := b.height >= minSubstreamHeight
			if ai != bi {
				return ai
			}
			if a.area() != b.area() {
				return a.area() < b.area()
			}
		} else if a.area() != b.area() {
			return a.area() > b.area()
		}
		return a.token < b.token
	})
	return kept[0], true
}
