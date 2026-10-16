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

package lan

import (
	"net"
	"slices"
	"testing"
)

// Every interface the system reports is listed, in its order: nothing is
// filtered here, so a caller that wants the loopback left out decides it.
func TestDiscoverSystemNics_ListsEveryInterface(t *testing.T) {
	itfs, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	var want []string
	for _, itf := range itfs {
		want = append(want, itf.Name)
	}

	got, err := DiscoverSystemNics()
	if err != nil {
		t.Fatalf("DiscoverSystemNics: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("DiscoverSystemNics() = %v, want %v", got, want)
	}
	// Every host this runs on has a loopback, so an empty list is a bug in
	// this function rather than an odd machine.
	if len(got) == 0 {
		t.Fatal("no interface at all, not even the loopback")
	}
}
