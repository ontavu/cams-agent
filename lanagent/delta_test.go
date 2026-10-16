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
	"math"
	"testing"
)

func TestDelta(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hi, lo uint32
		want   uint32
	}{
		{"same generation", 7, 7, 0},
		{"one apart", 7, 6, 1},
		{"ordinary distance", 6, 2, 4},
		{"from zero", 5, 0, 5},

		// The counter is circular, so a recent generation can be numerically
		// smaller than an old one. These are the cases the previous
		// implementation got wrong by one.
		{"across the wrap", 2, math.MaxUint32 - 2, 5},
		{"just across the wrap", 0, math.MaxUint32, 1},
		{"a whole turn back", 0, 1, math.MaxUint32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := delta(tc.hi, tc.lo); got != tc.want {
				t.Fatalf("delta(%d, %d) = %d, want %d", tc.hi, tc.lo, got, tc.want)
			}
		})
	}
}

// TestDelta_NarrowerWidths checks the generic over the other widths the
// constraint admits, since the wraparound is where the arithmetic matters and
// it is much easier to reach on 8 bits.
func TestDelta_NarrowerWidths(t *testing.T) {
	if got := delta[uint8](2, math.MaxUint8-2); got != 5 {
		t.Fatalf("delta[uint8](2, 253) = %d, want 5", got)
	}
	if got := delta[uint16](2, math.MaxUint16-2); got != 5 {
		t.Fatalf("delta[uint16](2, 65533) = %d, want 5", got)
	}
}
