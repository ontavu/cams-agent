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

// Package errkind names the kinds of failure a caller may branch on, as
// sentinels to wrap with %w and to test with errors.Is:
//
//	return fmt.Errorf("argument %q to %s %w", arg, verb, errkind.ErrNotValid)
//
// The message is the description followed by the kind, as it has always read
// in this module: `argument "x" to LIST not valid`.
//
// A kind does not cross a bus. Only the text of an error travels between two
// agents, so on the far side the kind is gone and agentbus.ErrRemote is what
// errors.Is can find.
//
// It imports nothing of this module, so that any package may import it.
package errkind

import "errors"

var (
	// ErrNotValid is an input that cannot be accepted as it is: an argument,
	// a reply, a pattern, an identifier.
	ErrNotValid = errors.New("not valid")

	// ErrNotSupported is a request this side does not know how to honour: an
	// unknown verb, an encoding with no mapping.
	ErrNotSupported = errors.New("not supported")

	// ErrNotFound is something looked for and not there.
	ErrNotFound = errors.New("not found")
)
