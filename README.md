# cams-agent

The LAN side of [Cams](https://github.com/ontavu/cams): what runs on the user's own
network, beside the cameras. It is two commands:

* **`cams-agent`** discovers the ONVIF cameras it can see, registers their streams with the
  hub's control plane (`cams-ctrl`, `:6000`), and uploads a stream to its data plane
  (`cams-rtp2hls`, `:6001`) when it is told to. It only dials out, which is what lets it
  live behind a NAT.
* **`cams-cli`** is the client for one-off work. `cams-cli cam play` finds a camera and
  records its stream into a capture archive, with no hub at all: the format Cams's
  replay harness reads. `cams-cli hub play <user> <camera>` asks
  the hub to have an agent upload a stream.

How it works inside is [ARCHITECTURE.md](ARCHITECTURE.md); the rules for changing it are
[AGENTS.md](AGENTS.md). It speaks the contract of
[cams-api](https://github.com/ontavu/cams-api), and builds on
[cams-go-utils](https://github.com/ontavu/cams-go-utils).

## Install

```shell
go install github.com/ontavu/cams-agent/bin/...@latest     # or @vX.Y.Z
```

or, from a clone, `go build -o . ./bin/...`. Go 1.26.7 or newer; no C toolchain and no
protoc, because the gRPC bindings come from cams-api.

### The image

```shell
docker build -t cams-agent:local .
docker run --rm --network host -e CAMS_ONVIF_PASSWORD=... cams-agent:local \
    cams-agent --user <user> --control <hub>:6000 --media <hub>:6001
```

**It takes the host's network namespace**, and that is not a setting to relax. Discovery
is WS-Discovery: a multicast probe sent per interface, over the interfaces the agent can
enumerate. Inside a bridge namespace that is one veth, the probe never leaves the host and
no camera is ever found -- and no port mapping fixes it, because there is no port, only a
multicast group the container is not on. On a machine that runs Docker the agent will
also probe every `veth` and bridge it can see and log a failure for each; they are
harmless, and the `discover` patterns of the configuration file are how to narrow them.
For a NAS or a router, `macvlan` is the better answer: a real MAC address on the LAN and
real multicast, at the cost of naming the parent interface and losing host-to-container
traffic.

Cams's `docker/compose.yaml` runs this image, built from a tag of this repository, under
its `lan` profile: the whole system on one machine.

## Running it

```shell
cams-agent                              # the hub on 127.0.0.1 by default
cams-cli hub play <user> <camera>       # ask for one stream
```

`<user>` is the agent's configured user, `plop` by default. `<camera>` is the camera's
ONVIF UUID: take it from `cams-agent`'s log, which names each device as it discovers it.
`Viewer.Play` answers `NOT_FOUND: agents not found` until the agent's control stream is up.

The flags: `--config`, `--user`, `--control`, `--media`, `--speed`. `--speed` shortens
the registration loop from five seconds to one; the first discovery runs at start-up
either way, and the rescan interval is unaffected.

## Configuration

Four places, and the later one wins: the compiled-in defaults, then `--config`,
then the environment, then the flags. Only the two ONVIF credentials come from
the environment — `CAMS_ONVIF_USER` and `CAMS_ONVIF_PASSWORD`, the same two names
`cams-cli` takes — because a password on a command line is visible in `ps` to
every user of the machine and lands in the shell history.

The file is JSON, and every field it omits keeps its default:

```json
{
  "user": "sandbox",
  "discover": ["enp*", "eth*", "!lo", "!docker.*", "!veth*", "!br-*"],
  "control": { "address": "hub.example:6000" },
  "media":   { "address": "hub.example:6001" },
  "cameras": [ { "address": "192.168.1.64" } ]
}
```

`discover` selects among the interfaces the system reports, and is worth setting
on a machine that runs Docker: the default matches everything but `lo` and
`docker*`, so every `veth` and bridge gets a multicast probe and a warning.
`cameras` names devices statically, for the ones discovery does not reach.

## Licence

[AGPL-3.0](LICENSE), or any later version. The authors are listed in [AUTHORS](AUTHORS).
