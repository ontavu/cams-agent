# The image of the LAN side: cams-agent and cams-cli, and nothing else.
#
#   docker build -t cams-agent:local .
#   docker run --rm --network host -e CAMS_ONVIF_PASSWORD=... cams-agent:local \
#       cams-agent --user <user> --control <hub>:6000 --media <hub>:6001
#
# It needs the host's network namespace to work at all. Discovery is
# WS-Discovery, sent per interface over multicast, and the agent enumerates the
# interfaces it can see: inside a bridge namespace that is one veth, and the
# probe never reaches the LAN. macvlan is the other answer, and the better one
# for a NAS or a router.

# The toolchain, the release .github/workflows/ci.yml pins too: the two move
# together. Then the distribution the runtime stage runs on.
ARG GO_VERSION=1.27.1
ARG UBUNTU_VERSION=26.04

#----------------------------------------------------------------------------#

FROM golang:${GO_VERSION} AS build

WORKDIR /src

# The module graph first, so that a change to the sources does not download it
# again.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 because nothing here imports "C": the binaries come out static,
# so the runtime stage needs no Go runtime, no libc matching and no toolchain.
# -buildvcs=false because .dockerignore keeps .git out of the context, and
# stamping a revision that is not there is a build failure rather than a
# missing label.
RUN CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -o /out/ ./bin/...

#----------------------------------------------------------------------------#

FROM ubuntu:${UBUNTU_VERSION}

ENV DEBIAN_FRONTEND=noninteractive

# A fixed, unprivileged uid, so that a rule or a volume on the host can name it.
ARG CAMS_UID=10001

RUN set -ex \
 && apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd --gid ${CAMS_UID} cams \
 && useradd --uid ${CAMS_UID} --gid ${CAMS_UID} --no-create-home --shell /usr/sbin/nologin cams

COPY --from=build /out/cams-agent /out/cams-cli /usr/local/bin/

# Checked here rather than discovered by a deployment.
RUN cams-agent --help > /dev/null && cams-cli --help > /dev/null

USER cams
CMD ["cams-agent"]
