# cams-agent

A LAN agent for ONVIF cameras: what runs on the user's own network, beside the cameras,
and relays their streams to a hub. It is two commands:

* **`cams-agent`** discovers the ONVIF cameras it can see, registers their streams with the
  hub's control endpoint (`Registrar` and `Controller`, `:6000` by default), and uploads a
  stream to its media endpoint (`Uploader`, `:6001`) when it is told to. It only dials
  out, which is what lets it live behind a NAT.
* **`cams-cli`** is the client for one-off work. `cams-cli cam play` finds a camera and
  records its stream into a [capture archive](#the-capture-archive), with no hub at all.
  `cams-cli hub play <user> <camera>` asks the hub to have an agent upload a stream.

How it works inside is [ARCHITECTURE.md](ARCHITECTURE.md); the rules for changing it are
[AGENTS.md](AGENTS.md). It speaks the contract of
[cams-api](https://github.com/ontavu/cams-api), and builds on
[cams-go-utils](https://github.com/ontavu/cams-go-utils) and
[go-swarm](https://github.com/ontavu/go-swarm).

## Install

```shell
go install github.com/ontavu/cams-agent/bin/...@latest     # the last release
go install github.com/ontavu/cams-agent/bin/...@main       # what this page describes
```

`@latest` is the last tag, which can be behind `main`, and so behind this page; `@vX.Y.Z`
pins one. Or, from a clone, `go build -o . ./bin/...`. Go 1.26.7 or newer; no C toolchain and no
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

## Running it

```shell
cams-agent                              # the hub on 127.0.0.1 by default
cams-cli hub play <user> <camera>       # ask the hub for one stream
cams-cli cam play <host[:port]>         # record a camera, with no hub
```

`<user>` is the agent's configured user, `plop` by default. `<camera>` is the identifier
the camera is registered under: the endpoint reference it advertises (`urn:uuid:...`), or
the `uuid` the configuration gave it. `cams-agent` logs it as the `key` of each device it
adds, beside its `endpoint`. `hub play` reaches the hub at `127.0.0.1:6000` unless
`--control` names another. The hub reaches an agent only through the control stream the
agent opened, so `hub play` fails until that stream is up.

`cam play` finds the camera at `<host[:port]>` by WS-Discovery, on every interface or on
the one `--interface` names, and records ten seconds of its stream; Ctrl-C ends it sooner.
The address is compared exactly with the one the camera advertises, which `cams-agent`
logs as the `endpoint`: usually the bare host, with a port only when the camera advertises
one. It needs `CAMS_ONVIF_PASSWORD`, and takes `CAMS_ONVIF_USER`, `admin` by default.

The flags of `cams-agent`: `--config`, `--user`, `--control`, `--media`, `--speed`.
`--speed` shortens the registration loop from five seconds to one, and the check loop --
every camera pinged, and a count logged -- from ten seconds to one. The first discovery
runs at start-up either way, and the rescan interval is unaffected.

### The capture archive

`cams-cli cam play` writes `cams-capture-*.tar` in the temporary directory (`$TMPDIR`,
else `/tmp`) and logs its path. It is a GNU tar with one entry per frame, in arrival
order: `000001.t0.sdp`, `000002.t0.rtp`, `000003.t0.rtcp`... The number orders the
entries, `t<N>` is the media's position in the session description, from 0, and the
extension says what the entry holds: the session description the camera returned, or one
raw RTP or RTCP packet. The description itself is filed under `t0`, since it describes
every media.

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
  "discover": ["!lo", "!docker.*", "!veth.*", "!br-.*", "en.*", "eth.*"],
  "control": { "address": "hub.example:6000" },
  "media":   { "address": "hub.example:6001" },
  "cameras": [ { "address": "192.168.1.64" } ]
}
```

| Field | Default | What it is |
|---|---|---|
| `user` | `plop` | The account the cameras are registered under; `--user` overrides it. |
| `camera_user` | `admin` | The cameras' ONVIF user; `CAMS_ONVIF_USER` overrides it. |
| `camera_password` | none | Their ONVIF password; `CAMS_ONVIF_PASSWORD` overrides it, and is the better place for it. |
| `discover` | `["!lo", "!docker.*", ".*"]` | The interfaces discovery probes; see below. |
| `interfaces` | none | Interfaces probed whatever the patterns say. |
| `cameras` | none | The cameras discovery does not reach; see below. |
| `scan_period` | `60` | Seconds between two rescans: the interfaces, and the configured cameras not reached yet. |
| `check_period` | `10` | Seconds between two checks: every camera pinged, and a count logged. |
| `register_period` | `5` | Seconds between two registrations with the hub. |
| `grace_generations` | `0` | Rescans a discovered camera may miss before it is forgotten; `0` forgets none. |
| `control.address` | `127.0.0.1:6000` | The hub's control endpoint; `--control` overrides it. |
| `control.timeout` | `30` | Seconds one registration may take. |
| `media.address` | `127.0.0.1:6001` | The hub's media endpoint; `--media` overrides it. It has no timeout: an upload lasts as long as the camera plays. |

`discover` selects among the interfaces the system reports. Each pattern is a regular
expression that has to match the whole name, and a leading `!` excludes. The first
pattern that matches decides, and an interface that none matches is not probed: in the
example, `wlp2s0` is left out. A pattern that is not a valid expression stops the agent
at start-up, and so does one written as a glob, such as `eth*`, which as a whole-name
expression would select nothing: the error gives the pattern to write, `eth.*`. The
default probes everything but `lo` and the `docker` bridges, so on a machine that runs
Docker every `veth` and `br-` bridge gets a multicast probe and a warning; the example is
the usual way to narrow it.

`cameras` names devices discovery does not reach. They are reached in addition to the
discovered ones, are tried again at every rescan until they answer, and are never
purged. Discovery leaves their addresses alone, so a camera named here keeps its `uuid`
and its credentials even where discovery sees it too. Two cameras on one address, or
under one `uuid`, stop the agent at start-up. For each:

* `address` is the host, or `host:port` when its ONVIF service is not on port 80. Not a
  URL: the device service is reached at `http://<address>/onvif/device_service`.
* `uuid` is the identifier the camera is registered under, the `<camera>` of
  `hub play`. Left out, the device is asked for its endpoint reference, which is the
  identifier discovery would have reported; a camera that cannot answer that needs it
  set.
* `user` and `password` replace the agent's ONVIF credentials for that camera, each on
  its own.

## Licence

[AGPL-3.0](LICENSE), or any later version. The authors are listed in [AUTHORS](AUTHORS).
