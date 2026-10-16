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

package transport

import (
	"context"
	"golang.org/x/sync/errgroup"
	"testing"
)

func TestUdpListener_doubleOps(t *testing.T) {
	ul := rawUdpListener{}
	if err := ul.OpenRandom("0.0.0.0"); err != nil {
		t.Fatal(err)
	}

	// double open must fail
	if err := ul.OpenRandom("0.0.0.0"); err == nil {
		t.Fatal("unexpected success")
	}

	ul.Close()
	ul.Close()

	// open must succeed after a close
	if err := ul.OpenRandom("0.0.0.0"); err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	// double open must still fail
	if err := ul.OpenRandom("0.0.0.0"); err == nil {
		t.Fatal("unexpected success")
	}
}

func TestUdpListener_OpenRandom(t *testing.T) {
	ul := rawUdpListener{}
	if err := ul.OpenRandom("0.0.0.0"); err != nil {
		t.Fatal(err)
	}
	defer ul.Close()

	if ul.GetPortMedia() <= 0 {
		t.Fatal("no media port")
	}
	if ul.GetPortControl() <= 0 {
		t.Fatal("no control port")
	}

	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return ul.Run(ctx)
	})
	cancel()

	if err := g.Wait(); err != nil {
		t.Fatal("unexpected error, no callback returned an error")
	}
}
