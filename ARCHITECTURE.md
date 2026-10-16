# Architecture

What `cams-agent` is made of. Where it sits in the whole system -- the processes, their
ports, who dials whom -- is in [Cams's ARCHITECTURE.md](https://github.com/ontavu/cams/blob/main/ARCHITECTURE.md):
the agent is deployed on each LAN, close to the cameras. It discovers them, registers
their streams with the hub's control plane (`cams-ctrl`, `:6000`), and uploads a stream
to its data plane (`cams-rtp2hls`, `:6001`) when it is told to. It never accepts a
connection: it dials out, which is what lets it live behind a NAT.

Its roles:
* to carry the credentials of the user;
* to discover the devices, unless a static configuration names them;
* to pilot the local cameras;
* to register the available streams in a hub;
* to trigger and tunnel the desired streams toward the hub.

## What it speaks

On the LAN, to the cameras:
* [WS Discovery], SOAP messages over multicast UDP (239.255.255.250:3702), probed on each
  interface the system reports and the `discover` patterns select -- which is why the
  agent needs the host's network, or a `macvlan`, rather than a bridge;
* [OnVif] to control the devices and discover their media streams: SOAP over HTTP, at
  the address the device advertised in its `ProbeMatch` -- usually
  `http://<host>/onvif/device_service`, on a port of the camera's choosing;
* [RTSP] over TCP to negotiate and control the media streams;
* [RTP] and [RTCP] to consume them: UDP by default, and interleaved on the RTSP
  connection when a camera refuses that. [gortsplib] switches transport on its own,
  replaying `Setup`, and the packet callbacks fire identically either way, which is why
  nothing above `camagent/stream.go` knows which one was used.

Toward the hub, [gRPC] on the contract of [cams-api]'s `hub.proto`, every call dialled by
the agent:

| Call | Served by | What it carries |
|---|---|---|
| `Registrar.Register`, unary | `cams-ctrl` :6000 | the cameras and what they emit, again every `RegisterPeriod` |
| `Controller.Control`, a stream the agent opens | `cams-ctrl` :6000 | `PLAY` and `STOP`, pushed down by the hub |
| `Uploader.MediaUpload`, a client stream | `cams-rtp2hls` :6001 | one call per stream being uploaded: the SDP, then the raw RTP and RTCP packets |

## Which stream of a camera it takes

A camera offers several ONVIF profiles, and `chooseProfile`, in `camagent/profile.go`,
picks the one to pull. It prefers **H.264** whenever one is offered: the hub remuxes and
never transcodes, and H.265 in fMP4 plays in Safari and in a Chrome with hardware decode,
in little else. A camera that offers nothing but H.265 is carried anyway. Among the
profiles of the best encoding on offer, the smallest frame at least `minSubstreamHeight`
tall wins, and the token breaks a tie, so the order is total and a camera does not change
codec between two reconnects.

## Internal agents

`cams-agent` is itself agent-oriented: several internal agents communicate over a
message bus implemented with [Mangos], exchanging plain-text commands.

That choice comes from the variety of long-running tasks that run independently and yet
act on each other. Each agent stays simple, mostly lock-free, and reaches the others
only through messages — which is also what makes each of them testable alone.

The internal agents:
* one for the **upstream** connection to the Hub. It performs the registrations and
  receives the `Play`/`Stop` commands, which it hands to the LAN agent's controller —
  it has no camera of its own, and depends on no other agent;
* one for the **LAN** discovery. It is the camera manager: it knows which cameras exist,
  and it creates the camera agents;
* one per **camera**, created by the LAN agent.

Each agent has a controller structure acting as a client, which owns the wire protocol
of that agent. `main()` owns the upstream and LAN agents; the LAN agent owns the camera
agents. An owner that exits leaves no agent of its own running.

The media takes another path. Each camera agent pushes its frames onto a data bus of its
own, PUSH/PULL rather than REQ/REP, and at the other end a media sink in `bin/cams-agent`
forwards them to `Uploader.MediaUpload`, opening a call when the camera announces a
stream. `main` pairs the two as one camera for the LAN agent, and runs them as an
ensemble: a camera whose sink has stopped has nowhere to push, and a sink whose camera has
exited has nothing to forward. The rules of both buses are in [AGENTS.md](AGENTS.md).

![The agent's goroutines](docs/20221115%20cams-agent%20goroutines.svg)

## Layout

| Path | Contents |
|---|---|
| `bin/cams-agent/` | The agent's entry point: the configuration, the ownership tree, and the media sink that uploads a camera's stream (`upstream_media.go`). |
| `bin/cams-cli/` | `cams-cli`: `hub play` through `cams-ctrl`, and `cam play`, which drives a camera itself, with no hub. |
| `agentbus/` | The control bus: REQ/REP, one line of text per command. Framing, client side, serving loop. No vocabulary of its own. |
| `mediabus/` | The data bus: PUSH/PULL, binary media frames. The camera connects, the upstream binds. |
| `camagent/` | The agent of one camera: ONVIF discovery, the RTSP session, and pushing the media frames onto the data bus. Imported only by `main`. |
| `camctrl/` | The controller of a camera agent, and the vocabulary they exchange. |
| `lanagent/` | The agent managing the LAN: NIC discovery, the camera registry, the camera lifecycles. Imported only by `main`. |
| `lanctrl/` | The controller of the LAN agent, and the vocabulary they exchange. |
| `lan/` | The host's network interfaces, as the system reports them. |
| `upagent/` | The agent holding the link to the hub: registrations out, `Play`/`Stop` in. Imported only by `main`. |
| `upctrl/` | The controller of the upstream agent: `PING` and `STATE`, nothing more. |
| `transport/` | A raw UDP socket pair. Unused since the move to gortsplib v5. |
| `docs/` | The agent's goroutines, as the diagram above. |
| `ci/check.sh` | Everything CI checks; see [AGENTS.md](AGENTS.md). |
| `Dockerfile` | The image of `cams-agent` and `cams-cli`; see [README.md](README.md). |

## References

RTSP is handled by upstream [gortsplib] v5, which is very good and moves fast; the agent
depends on it rather than on a fork of it. A modified copy used to live in Cams's
`go/rtsp1`. Do not reintroduce a fork: the hooks that motivated it (`ListenPacket`,
`DialContext`, `Tunnel`) are all upstream now.

* RTSP [rfc2326], RTP and RTCP [rfc3550]
* RTP payload formats: H.264 [rfc6184], HEVC [rfc7798]
* [OnVif], [WS Discovery]
* [gRPC], [Mangos]

[cams-api]: https://github.com/ontavu/cams-api
[gortsplib]: https://github.com/bluenviron/gortsplib
[rfc2326]: https://datatracker.ietf.org/doc/html/rfc2326
[rfc3550]: https://datatracker.ietf.org/doc/html/rfc3550
[rfc6184]: https://datatracker.ietf.org/doc/html/rfc6184
[rfc7798]: https://datatracker.ietf.org/doc/html/rfc7798
[OnVif]: https://www.onvif.org/
[WS Discovery]: https://en.wikipedia.org/wiki/Web_Services_Discovery
[RTSP]: https://en.wikipedia.org/wiki/Real_Time_Streaming_Protocol
[RTP]: https://en.wikipedia.org/wiki/Real-time_Transport_Protocol
[RTCP]: https://en.wikipedia.org/wiki/RTP_Control_Protocol
[gRPC]: https://grpc.io/
[Mangos]: https://github.com/nanomsg/mangos
