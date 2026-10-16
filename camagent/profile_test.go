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

import "testing"

func h264(token string, w, h int) videoProfile {
	return videoProfile{token: token, encoding: "H264", width: w, height: h,
		uri: "rtsp://cam/" + token}
}

func h265(token string, w, h int) videoProfile {
	return videoProfile{token: token, encoding: "H265", width: w, height: h,
		uri: "rtsp://cam/" + token}
}

func TestChooseProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offered []videoProfile
		want    string
	}{{
		// The point of the exercise: the substream, not the main stream.
		name:    "the smaller H.264 profile wins",
		offered: []videoProfile{h264("main", 2560, 1920), h264("sub", 640, 480)},
		want:    "sub",
	}, {
		name:    "order of arrival does not matter",
		offered: []videoProfile{h264("sub", 640, 480), h264("main", 2560, 1920)},
		want:    "sub",
	}, {
		// A codec the browser cannot play is worth less than the bytes saved.
		name:    "H.264 beats a smaller H.265",
		offered: []videoProfile{h265("sub", 640, 480), h264("main", 2560, 1920)},
		want:    "main",
	}, {
		name:    "H.265 is taken when it is all there is",
		offered: []videoProfile{h265("main", 2560, 1920), h265("sub", 640, 480)},
		want:    "sub",
	}, {
		name: "an unplayable encoding loses to anything",
		offered: []videoProfile{
			{token: "mjpeg", encoding: "JPEG", width: 640, height: 480, uri: "rtsp://cam/mjpeg"},
			h265("hevc", 2560, 1920),
		},
		want: "hevc",
	}, {
		// Preferring the smallest with no floor would pick the mobile stream.
		name: "a profile below the floor is not preferred for being small",
		offered: []videoProfile{
			h264("mobile", 320, 240),
			h264("sub", 640, 480),
			h264("main", 2560, 1920),
		},
		want: "sub",
	}, {
		// But a camera offering nothing better must still be watchable.
		name:    "below the floor, the largest wins",
		offered: []videoProfile{h264("tiny", 176, 144), h264("mobile", 320, 240)},
		want:    "mobile",
	}, {
		name:    "a profile with no stream URI is not a candidate",
		offered: []videoProfile{{token: "sub", encoding: "H264", width: 640, height: 480}, h264("main", 2560, 1920)},
		want:    "main",
	}, {
		// Equal in every way that matters, so the token decides and the answer
		// is still the same on the next call.
		name:    "the token breaks a tie",
		offered: []videoProfile{h264("zulu", 640, 480), h264("alpha", 640, 480)},
		want:    "alpha",
	}, {
		// Some cameras do not report a resolution at all.
		name:    "an unreported resolution is still usable",
		offered: []videoProfile{h264("only", 0, 0)},
		want:    "only",
	}, {
		name:    "a reported resolution beats an unreported one",
		offered: []videoProfile{h264("unknown", 0, 0), h264("sub", 640, 480)},
		want:    "sub",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := chooseProfile(tc.offered)
			if !ok {
				t.Fatal("no profile chosen")
			}
			if got.token != tc.want {
				t.Fatalf("chose %q, want %q", got.token, tc.want)
			}
		})
	}
}

func TestChooseProfileFindsNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offered []videoProfile
	}{
		{"no profiles at all", nil},
		{"every profile lacks a stream URI", []videoProfile{
			{token: "a", encoding: "H264", width: 640, height: 480},
			{token: "b", encoding: "H265", width: 640, height: 480},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := chooseProfile(tc.offered); ok {
				t.Fatal("a profile was chosen where none is usable")
			}
		})
	}
}

// The selection has to be a function of the profiles alone: the SDK hands them
// over in a map, and the bug being fixed is that the answer used to depend on
// the iteration order.
func TestChooseProfileIsStable(t *testing.T) {
	offered := []videoProfile{
		h264("main", 2560, 1920),
		h264("sub", 640, 480),
		h265("hevc-sub", 640, 480),
		h264("mobile", 320, 240),
	}

	first, ok := chooseProfile(offered)
	if !ok {
		t.Fatal("no profile chosen")
	}
	// Every rotation of the same set has to give the same answer.
	for i := range offered {
		rotated := append(append([]videoProfile{}, offered[i:]...), offered[:i]...)
		got, ok := chooseProfile(rotated)
		if !ok {
			t.Fatalf("rotation %d chose nothing", i)
		}
		if got.token != first.token {
			t.Fatalf("rotation %d chose %q, want %q", i, got.token, first.token)
		}
	}
	if first.token != "sub" {
		t.Fatalf("chose %q, want sub", first.token)
	}
}
