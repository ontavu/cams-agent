# Architecture

What `cams-agent` is made of. Where it sits in the whole system -- the processes, their
ports, who dials whom -- is in [Cams's ARCHITECTURE.md](https://github.com/ontavu/cams/blob/main/ARCHITECTURE.md):
the agent is deployed on each LAN, close to the cameras. It discovers them, registers
their streams with the hub's control plane (`cams-ctrl`, `:6000`), and uploads a stream
to its data plane (`cams-rtp2hls`, `:6001`) when it is told to. It never accepts a
connection: it dials out, which is what lets it live behind a NAT.

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

![The agent's goroutines](docs/20221115%20cams-agent%20goroutines.svg)

## References

RTSP is handled by [gortsplib], which is very good and moves fast; the agent depends on
it rather than on a fork of it.

* RTSP [rfc2326], RTP and RTCP [rfc3550]
* RTP payload formats: H.264 [rfc6184], HEVC [rfc7798]
* [OnVif], [WS Discovery]
* [gRPC], [Mangos]

[gortsplib]: https://github.com/bluenviron/gortsplib
[rfc2326]: https://datatracker.ietf.org/doc/html/rfc2326
[rfc3550]: https://datatracker.ietf.org/doc/html/rfc3550
[rfc6184]: https://datatracker.ietf.org/doc/html/rfc6184
[rfc7798]: https://datatracker.ietf.org/doc/html/rfc7798
[OnVif]: https://www.onvif.org/
[WS Discovery]: https://en.wikipedia.org/wiki/Web_Services_Discovery
[gRPC]: https://grpc.io/
[Mangos]: https://github.com/nanomsg/mangos
