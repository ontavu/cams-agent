# cams-agent

Read [README.md](README.md) first: what the agent and `cams-cli` are, how they are built,
run and configured. [ARCHITECTURE.md](ARCHITECTURE.md) is what the agent is made of. This
file holds the rules for changing it.

## Commands

```sh
ci/check.sh     # everything CI checks
```

It runs gofmt, `go vet`, the build of both commands, the import rule of the agent
packages (below), `go test -race` and the `go mod tidy` check, and refuses a run that
changed the working tree. `.github/workflows/ci.yml` runs it, then builds the image. Add a
check to the script, never to the workflow alone.

`go.mod` declares Go **1.26.7** as the language floor, and CI runs the release that Cams's
CI image pins. Keep the floor at or below that, and do not raise it past the locally
installed toolchain without intending to: with `GOTOOLCHAIN=auto`, Go then downloads a
toolchain silently on the next build.

## What may be imported, and by whom

This module requires [cams-api](https://github.com/ontavu/cams-api), the gRPC contract,
[cams-go-utils](https://github.com/ontavu/cams-go-utils) and
[go-swarm](https://github.com/ontavu/go-swarm), all three under the MPL-2.0 and all three
**at a tagged version, never through a `replace`**. A change to any of them is a release
there, and then `go get <module>@vX.Y.Z` and `go mod tidy` here.

**Nothing outside this repository may import it, and the Cams hub above all.** This module
is under the AGPL-3.0, and the hub is proprietary: linking this code would put the hub
under the AGPL. What both sides need belongs in cams-go-utils, or in cams-api if it is
part of the contract.

`cams-go-utils` knows nothing of the contract, so its gRPC helpers apply no keepalive
unless told to. **Every `utils.DialInsecure` passes `wire.DialOptions()...`.** A dial that
forgets them still connects. It just has no keepalive, so behind a NAT the agent becomes
the zombie that cams-api's `wire/keepalive.go` describes.

## The agents talk over the bus, not through each other

`ARCHITECTURE.md` describes `cams-agent` as a set of internal agents exchanging plain
text commands over a [Mangos](https://github.com/nanomsg/mangos) bus. All three are
converted -- camera, LAN, upstream. The pattern:

1. **An agent package is imported by `main` and nothing else.** Everything else holds a
   controller. `main` builds each pair and injects a factory, which is why
   `lanagent.New` takes a `CameraFactory` rather than knowing how a camera is made. An
   agent may name a *controller* package -- `lanagent` names `camctrl`, `upagent` names
   `lanctrl` -- because a controller drags nothing behind it. Naming another *agent*
   package is what is forbidden, and the check is mechanical: `ci/check.sh` lists, for
   each of `camagent`, `lanagent` and `upagent`, every package other than a `main` that
   imports it, and fails on the first one.
2. **The controller package owns the vocabulary; `agentbus` owns the framing.** The
   vocabulary cannot live in the agent, or every holder of a controller would pull the
   agent in transitively. The framing is shared so that the awkward parts of rule 7 exist
   in exactly one place.
3. **Ownership is a tree, and an owner joins what it started.** `main` owns the upstream
   and LAN agents; the LAN agent owns the camera agents. Every path out of a `Run` has to
   leave nothing of its own behind, which means the teardown belongs in a `defer` rather
   than at the end of the happy path.
4. **An agent depends on no other agent, and on no channel it did not create.** Where it
   needs a neighbour it depends on the interface the neighbour's controller package
   declares -- `camctrl.CameraController`, `lanctrl.LanController` -- and `main` injects
   something that satisfies it. One declaration per contract, beside the vocabulary it
   belongs to, so consumers cannot drift apart. A channel handed in from outside would
   tie two agents' lifetimes together behind the bus's back; there are none, and there is
   no reason to add one.

   **A controller's `New` returns the concrete `*Client`, not the interface.** Returning
   the interface looks tidier and costs more than it looks: the contract is narrower than
   the client on purpose, so returning it puts the remaining methods out of reach of
   every caller including the tests. That is how `Ping`, `State`, `ID` and one `Close`
   went missing once already. Each package carries a
   `var _ XController = (*Client)(nil)` so the two cannot drift.

   **The other half of the rule, learned the harder way: a contract narrower than what
   the agent has to do is a bug, not tidiness.** `camctrl.Client.Ping` existed and
   `CameraController` did not declare it, so the LAN agent -- which owns the cameras and
   holds only the interface -- could not send the one verb that completes a `Play` after
   a `Pause`. Every camera asked to restart while it was still stopping parked in
   `StateResuming` for good. Nothing failed, nothing logged, and the compile-time proof
   above cannot catch it: it proves the client satisfies the contract, never that the
   contract covers the job.
5. **An agent binds its endpoint when it is built, not when it is run.** A mangos `Dial`
   is synchronous, so a controller created before the agent's `Run` was scheduled would
   be refused. It also means an agent that is built and then abandoned must be `Close`d,
   or its endpoint leaks.
6. **The media does not travel on the control bus.** `agentbus` is REQ/REP and
   synchronous; media is PUSH/PULL through `mediabus`, where the camera is the active
   side and the upstream binds. The queue that bounds the media in flight is the PUSH
   socket's own, and the camera's send is where an overflow is counted.
7. **Requests are at most once.** A `req` socket resends after a minute by default, which
   would replay a command that is not idempotent. Retry is disabled and a receive
   deadline is set instead. The corollary is that a command whose peer disappears while
   answering reports `ErrCanceled`; only a stopping command, where the peer going away
   *is* the result, may treat that as success -- that is what
   `agentbus.Client.DoTolerateGone` is for. A controller fronting an agent that itself
   calls another one wants the longer timeout, so the inner hop expires first and the
   failure is reported where it happened.

## Go engineering rules

These are requirements, not preferences. They follow from what the code is: a
long-running relay, on unattended hardware, fed by devices whose output cannot be
trusted.

### The code has to be unit tested

New or modified logic ships with its tests, in the same change.

* Every package but `bin/cams-cli` has tests. Prefer
  adding a test over matching the surrounding absence, and prefer a narrow interface on
  the consumer side over a fixture that needs a server: `upagent` tests its whole gRPC
  logic against `pb.RegistrarClient` and a two-line `controlStream`.
* Anything touching goroutines or channels must also pass `go test -race ./...`.
* Test through the exported API where possible; these packages are small enough that it
  is rarely a real constraint.
* Prefer a table test over a hand-rolled loop, and `t.Fatal` over logging and carrying
  on with a broken fixture.
* When a test's comment and its assertion disagree, the comment usually records the
  intent. Read the implementation before deciding which of the two is the bug.

### Errors are never silently ignored

* No `_ = f()`, no empty `if err != nil {}`, no naked `return` that drops an error. When
  an error truly cannot be acted upon, log it and leave a comment saying why.
* Add context on the way up with `errors.Annotate(err, "describe")` from
  `github.com/juju/errors`, the convention throughout the Go modules.
* An error raised inside a goroutine reaches its owner through the group that supervises
  it, not through a log line alone.
* `go vet ./...` must stay clean; it catches the cheapest half of this class.

### No goroutine may leak

Every goroutine has an end that is reachable from the outside.

* A goroutine either takes a `context.Context` and returns when it is done, or belongs
  to an `errgroup.Group` or a `swarm.Swarm` that somebody waits on.
* `swarm.New` and `swarm.NewEnsemble`, from
  [go-swarm](https://github.com/ontavu/go-swarm), are the local idiom for a supervised
  group: they hold the cancel and the `WaitGroup`, and count live members.
* Whoever cancels also waits. A `Cancel()` with no matching `Wait()` lets the process
  tear down while work is still running.
* A library that knows nothing about contexts needs an explicit bridge. See
  `runStreamOnce` in `camagent/stream.go`, where a watcher goroutine closes the RTSP
  client in order to release `Client.Wait()`.
* Watch for the deadlock shape where every member of a group waits on a cancellation
  that only that group's own `Wait()` could ever produce.

### A service stops when it is told to

`cams-agent` is meant to run under something that stops it with `SIGTERM` -- a container
runtime, systemd, a supervisor. That signal is the only notice it gets.

* **Trap `syscall.SIGTERM`, and never `os.Kill`.** `os.Kill` is `SIGKILL`, which cannot
  be caught; Go accepts it in `signal.Notify` and ignores it, so a binary listing it and
  `os.Interrupt` handles `SIGINT` alone. `SIGTERM` then keeps its default disposition and
  the process dies where it stands. Measured on one of Cams's viewer processes before
  this was fixed there: `SIGTERM` exited 143 with no drain, `SIGINT` exited 0 drained --
  identical from a terminal, opposite everywhere else. Both binaries here take
  `syscall.SIGTERM, syscall.SIGINT`.

### No buffer may grow indefinitely

A camera is an adversarial producer by default: it outruns the uplink, or emits
malformed frames forever.

* Every channel crossing a goroutine boundary is created with an explicit capacity, and
  that capacity is a named constant near the type, not a literal at the call site.
* Every queue has a deliberate policy for being full: drop, or apply back-pressure.
  There is no third choice, and "block forever" is not a policy.
* On a media path, dropping beats blocking. Holding a reading goroutine stalls the whole
  RTSP session and trips its timeout detection, which costs far more than one packet.
  Count the drops and expose the counter, the way `mediaSink.push` does in
  `camagent/stream.go`.
* Never accumulate per-packet or per-frame data in a slice or a map that nothing prunes.

### Review the local code as a senior Go developer

When reviewing what is already here, apply the judgement of somebody who has operated Go
services, not a linter's.

* Say plainly what is wrong and why it will fail, naming the input or the interleaving
  that breaks it, rather than expressing a general unease.
* Look at concurrency first: races, missed cancellations, unbounded growth, lock
  ordering, a stream that tolerates a single writer being fed from two goroutines.
* Separate what must change from what you would merely prefer, and let the second
  category go.
* Verify a claim against the code before making it. Read the dependency's own source
  when its contract is not obvious: whether a callback fires on every transport is
  answered by the library, not by intuition.
* Report a pre-existing defect found along the way even when it falls outside the task,
  and say that it is pre-existing.

## Mangos traps, all verified against v3.4.2

The library's documentation disagrees with its code in three places that cost real time.
Each is worked around in `agentbus` or `mediabus`, with a comment; do not undo them.

* **`OptionBestEffort` on PUSH discards about half of everything**, at any load, and
  reports success. It arms the discard branch of a `select` with an already-closed
  channel, so it competes with the enqueue branch even on an empty queue. Measured: of
  2000 frames sent to a puller that was keeping up, 999 arrived.
* **A negative send or receive deadline does not mean "non-blocking"**, contrary to the
  documented behaviour. `xpush` and `xpull` both guard on `> 0`, so a negative value is
  the same as none and blocks forever. There is no non-blocking send.
* **`OptionLinger` is dead code.** The constant is declared and re-exported, with
  upstream's own `// Remove?`, and read by nothing. `Close` never waits, and anything
  still queued is discarded.

Also worth knowing: options must be set before `Dial`/`Listen`, because
`xpush.SetOption` replaces the send queue while `SendMsg` reads it without the lock.

## Licence header on every `.go` file

```go
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
```

Keep a blank line between the notice and `package`, or Go takes the licence for the
package's doc comment. A file that comes here from an MPL module takes this header, and
leaves its old one behind: `lan/discover.go`, which came from cams-go-utils, is the
example.

## Attribution: `Assisted-By`, never `Co-Authored-By`

A model is a tool, not an author. Work an assistant helped with ends its commit message,
and its pull request description, with one line: `Assisted-By: Claude Code`,
`Assisted-By: Codex`, and so on.

- **The tool, not the model.** Model names change every few months; the tool is what was
  actually run.
- **No address**, and no `Co-Authored-By`. Git and GitHub read that trailer as a person.
  In a copyleft project the authors are the copyright holders, and they are listed in
  `AUTHORS`.
- **Nothing in the sources.** No `// generated by <model>`, and no model name anywhere.

## Releases

A release is a tag, `vX.Y.Z`, below v1 while the commands and their flags still move. CI
runs on the tag as on any other push. Cams's `docker/compose.yaml` builds the image from
this repository at a tag, under its `lan` profile, so a release reaches the Cams sandbox
when that tag is moved there.

**Never move or recreate a tag.** The Go checksum database remembers the first content it
saw for a version, so a moved tag fails `go install` for everyone who already fetched it.
A mistake is fixed by the next version.
