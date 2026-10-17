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

package errkind_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/ontavu/cams-agent/errkind"
)

// The way the kinds are meant to be used: the message is the description then
// the kind, and errors.Is finds the kind through any further wrapping.
func TestKinds_WrapAndRead(t *testing.T) {
	for _, tc := range []struct {
		kind error
		want string
	}{
		{errkind.ErrNotValid, `outer: argument "x" to LIST not valid`},
		{errkind.ErrNotSupported, `outer: argument "x" to LIST not supported`},
		{errkind.ErrNotFound, `outer: argument "x" to LIST not found`},
	} {
		t.Run(tc.kind.Error(), func(t *testing.T) {
			inner := fmt.Errorf("argument %q to %s %w", "x", "LIST", tc.kind)
			err := fmt.Errorf("outer: %w", inner)
			if got := err.Error(); got != tc.want {
				t.Fatalf("message %q, want %q", got, tc.want)
			}
			if !errors.Is(err, tc.kind) {
				t.Fatalf("errors.Is lost %v through the wrapping", tc.kind)
			}
		})
	}
}

// Three distinct kinds: one must never be mistaken for another.
func TestKinds_AreDistinct(t *testing.T) {
	kinds := []error{errkind.ErrNotValid, errkind.ErrNotSupported, errkind.ErrNotFound}
	for i, a := range kinds {
		for j, b := range kinds {
			if i != j && errors.Is(a, b) {
				t.Fatalf("%v is %v", a, b)
			}
		}
	}
}
