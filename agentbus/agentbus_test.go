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

package agentbus_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juju/errors"
	"github.com/ontavu/cams-agent/agentbus"
	"go.nanomsg.org/mangos/v3"
)

// ---------- framing ----------

func TestRequest_RoundTrip(t *testing.T) {
	for _, tc := range []struct{ verb, arg string }{
		{"PLAY", ""},
		{"PLAY", "3fa85f64-5717-4562-b3fc-2c963f66afa6"},
		{"LIST", ""},
		// An argument may itself contain spaces: only the first token is the
		// verb, the remainder is the argument verbatim.
		{"SAY", "hello there world"},
	} {
		t.Run(tc.verb+"/"+tc.arg, func(t *testing.T) {
			verb, arg, err := agentbus.DecodeRequest(agentbus.EncodeRequest(tc.verb, tc.arg))
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if verb != tc.verb || arg != tc.arg {
				t.Fatalf("got (%q, %q), want (%q, %q)", verb, arg, tc.verb, tc.arg)
			}
		})
	}
}

// TestEncodeRequest_BareVerb pins the wire compatibility that let the camera
// move onto this package unchanged: a verb without an argument is just itself.
func TestEncodeRequest_BareVerb(t *testing.T) {
	if got := string(agentbus.EncodeRequest("PLAY", "")); got != "PLAY" {
		t.Fatalf("EncodeRequest(PLAY, \"\") = %q, want PLAY", got)
	}
}

func TestDecodeRequest_Rejected(t *testing.T) {
	for _, payload := range []string{"", "   ", "\n", "\t\r\n"} {
		if verb, _, err := agentbus.DecodeRequest([]byte(payload)); err == nil {
			t.Fatalf("DecodeRequest(%q) = %q, want an error", payload, verb)
		}
	}
}

func TestReply_RoundTrip(t *testing.T) {
	for _, arg := range []string{"", "PLAYING", "a b c"} {
		got, err := agentbus.DecodeReply(agentbus.EncodeOK(arg))
		if err != nil {
			t.Fatalf("DecodeReply(EncodeOK(%q)): %v", arg, err)
		}
		if got != arg {
			t.Fatalf("DecodeReply(EncodeOK(%q)) = %q", arg, got)
		}
	}
}

func TestDecodeReply_Error(t *testing.T) {
	payload := agentbus.EncodeErr(errors.New("camera is on fire"))

	got, err := agentbus.DecodeReply(payload)
	if err == nil {
		t.Fatalf("DecodeReply(%q) = %q, want an error", string(payload), got)
	}
	if !errors.Is(err, agentbus.ErrRemote) {
		t.Fatalf("error %v does not match ErrRemote", err)
	}
	if !strings.Contains(err.Error(), "camera is on fire") {
		t.Fatalf("error %v lost the message reported by the agent", err)
	}
}

// TestEncodeErr_SingleLine matters because a reply is read back as one line: an
// annotation chain from juju/errors contains newlines and would be truncated.
func TestEncodeErr_SingleLine(t *testing.T) {
	err := errors.Annotate(errors.New("inner\nsecond line"), "outer")

	payload := agentbus.EncodeErr(err)
	if strings.ContainsAny(string(payload), "\n\r") {
		t.Fatalf("EncodeErr produced a multi-line reply: %q", string(payload))
	}
	if _, derr := agentbus.DecodeReply(payload); derr == nil {
		t.Fatal("a failure reply decoded as a success")
	}
}

func TestEncodeErr_NilStillFails(t *testing.T) {
	if _, err := agentbus.DecodeReply(agentbus.EncodeErr(nil)); err == nil {
		t.Fatal("EncodeErr(nil) decoded as a success")
	}
}

// TestDecodeReply_Malformed is the important negative: a garbled exchange must
// not pass for a successful one carrying a zero value.
func TestDecodeReply_Malformed(t *testing.T) {
	for _, payload := range []string{"", "   ", "MAYBE", "ok", "OKAY fine", "PLAY"} {
		if got, err := agentbus.DecodeReply([]byte(payload)); err == nil {
			t.Fatalf("DecodeReply(%q) = %q, want an error", payload, got)
		}
	}
}

// ---------- transport ----------

func testURL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("inproc://cams/test/%s-%d", t.Name(), time.Now().UnixNano())
}

// echo answers OK with the argument, and stops on STOP.
func echo(_ context.Context, verb, arg string) ([]byte, bool) {
	switch verb {
	case "ECHO":
		return agentbus.EncodeOK(arg), false
	case "STOP":
		return agentbus.EncodeOK(""), true
	case "FAIL":
		return agentbus.EncodeErr(errors.New("as requested")), false
	default:
		return agentbus.EncodeErr(errors.NotSupportedf("verb %q", verb)), false
	}
}

// serve starts a server and returns a connected client.
func serve(t *testing.T, h agentbus.Handler) *agentbus.Client {
	t.Helper()

	url := testURL(t)
	srv, err := agentbus.Listen("test", url)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		srv.Serve(ctx, h)
	}()

	cli, err := agentbus.Dial("test", url, agentbus.WithTimeout(2*time.Second))
	if err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("Dial: %v", err)
	}

	t.Cleanup(func() {
		if err := cli.Close(); err != nil {
			t.Errorf("client close: %v", err)
		}
		cancel()
		wg.Wait()
	})

	return cli
}

func TestClient_RoundTrip(t *testing.T) {
	cli := serve(t, echo)

	for _, arg := range []string{"", "one", "one two three"} {
		got, err := cli.Do("ECHO", arg)
		if err != nil {
			t.Fatalf("Do(ECHO, %q): %v", arg, err)
		}
		if got != arg {
			t.Fatalf("Do(ECHO, %q) = %q", arg, got)
		}
	}
}

func TestClient_RemoteFailure(t *testing.T) {
	cli := serve(t, echo)

	_, err := cli.Do("FAIL", "")
	if err == nil {
		t.Fatal("a failure reply was reported as a success")
	}
	if !errors.Is(err, agentbus.ErrRemote) {
		t.Fatalf("error %v does not match ErrRemote", err)
	}
}

// TestServer_UnknownVerbAnswered checks that an unrecognised verb gets a reply
// instead of being dropped, and that the exchange survives it.
func TestServer_UnknownVerbAnswered(t *testing.T) {
	cli := serve(t, echo)

	if _, err := cli.Do("PIROUETTE", ""); err == nil {
		t.Fatal("an unknown verb was accepted")
	}
	// The server has to still be serving afterwards.
	if _, err := cli.Do("ECHO", "still here"); err != nil {
		t.Fatalf("the server stopped serving after a bad verb: %v", err)
	}
}

// TestServer_MalformedRequestAnswered goes under the client, which cannot
// produce an empty request through EncodeRequest.
func TestServer_MalformedRequestAnswered(t *testing.T) {
	cli := serve(t, echo)

	if _, err := cli.Do("", ""); err == nil {
		t.Fatal("an empty request was accepted")
	}
	if _, err := cli.Do("ECHO", "still here"); err != nil {
		t.Fatalf("the server stopped serving after a malformed request: %v", err)
	}
}

// TestListen_BeforeServe is the ordering every factory in the tree relies on: a
// client must be able to dial a server that is bound but not yet serving.
func TestListen_BeforeServe(t *testing.T) {
	url := testURL(t)

	srv, err := agentbus.Listen("test", url)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer func() {
		if err := srv.Close(); err != nil {
			t.Errorf("server close: %v", err)
		}
	}()

	// Serve has deliberately not been called.
	cli, err := agentbus.Dial("test", url)
	if err != nil {
		t.Fatalf("a client could not dial a bound but idle server: %v", err)
	}
	if err := cli.Close(); err != nil {
		t.Errorf("client close: %v", err)
	}
}

func TestListen_DuplicateAddress(t *testing.T) {
	url := testURL(t)

	first, err := agentbus.Listen("first", url)
	if err != nil {
		t.Fatalf("first Listen: %v", err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Errorf("server close: %v", err)
		}
	}()

	second, err := agentbus.Listen("second", url)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second server bound the same address")
	}
	if !errors.Is(err, mangos.ErrAddrInUse) {
		t.Fatalf("want ErrAddrInUse, got %v", err)
	}
}

func TestDial_NoListener(t *testing.T) {
	if cli, err := agentbus.Dial("test", testURL(t)); err == nil {
		_ = cli.Close()
		t.Fatal("dialling an address nobody serves succeeded")
	}
}

// TestServe_StopsOnCancel checks that cancellation reaches a blocked receiver
// rather than leaking the goroutine.
func TestServe_StopsOnCancel(t *testing.T) {
	url := testURL(t)
	srv, err := agentbus.Listen("test", url)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		srv.Serve(ctx, echo)
	}()

	cli, err := agentbus.Dial("test", url, agentbus.WithTimeout(2*time.Second))
	if err != nil {
		cancel()
		t.Fatalf("Dial: %v", err)
	}
	if _, err := cli.Do("ECHO", "up"); err != nil {
		cancel()
		t.Fatalf("Do: %v", err)
	}
	if err := cli.Close(); err != nil {
		t.Errorf("client close: %v", err)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

// TestDoTolerateGone covers a command whose effect is that the peer stops. The
// reply usually cannot be collected, and that must not read as a failure.
func TestDoTolerateGone(t *testing.T) {
	url := testURL(t)
	srv, err := agentbus.Listen("test", url)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		srv.Serve(ctx, echo)
	}()

	cli, err := agentbus.Dial("test", url, agentbus.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() {
		if err := cli.Close(); err != nil {
			t.Errorf("client close: %v", err)
		}
	}()

	if _, err := cli.DoTolerateGone("STOP", ""); err != nil {
		t.Fatalf("DoTolerateGone(STOP): %v", err)
	}

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the stopping command")
	}
}

// TestClient_Concurrent is the reason each exchange opens its own mangos
// context. On the shared default context a second Send cancels the request
// already in flight, and one of these callers would fail.
func TestClient_Concurrent(t *testing.T) {
	cli := serve(t, echo)

	const callers = 8
	const rounds = 10

	var failures atomic.Uint32
	start := make(chan struct{})
	wg := sync.WaitGroup{}

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			<-start
			want := fmt.Sprintf("caller-%d", id)
			for r := 0; r < rounds; r++ {
				got, err := cli.Do("ECHO", want)
				if err != nil {
					failures.Add(1)
					t.Errorf("caller %d: %v", id, err)
					return
				}
				// A mismatch here would mean replies crossed over.
				if got != want {
					failures.Add(1)
					t.Errorf("caller %d got %q, want %q", id, got, want)
					return
				}
			}
		}(i)
	}

	close(start)
	wg.Wait()

	if n := failures.Load(); n != 0 {
		t.Fatalf("%d concurrent callers failed", n)
	}
}
