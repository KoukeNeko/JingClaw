# JingClaw as an image.
#
# The image is the program. A volume mounted at JINGCLAW_HOME is the
# deployment: settings, the event log, the workspace, and any credentials
# kept as files. Nothing about a deployment is baked in here — a settings
# file shipped inside becomes the operator's by accident, and then quietly
# stops being read the day they write their own.

FROM golang:1.26-bookworm AS build

WORKDIR /src

# Dependencies first, so editing the code does not re-download them.
COPY core/go.mod core/go.sum ./core/
RUN cd core && go mod download

COPY core ./core
COPY proto ./proto

# Static, because the sqlite driver is pure Go (modernc.org/sqlite) and
# nothing else here needs libc. What that buys is a binary that does not care
# which libc the runtime image has.
#
# The build is stripped of paths and symbols it does not need: -trimpath so
# the binary does not carry the builder's directory layout, and -s -w because
# a stack trace from a released binary is read from the log rather than from
# symbol names.
RUN cd core && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
	-o /out/jingclaw ./cmd/jingclaw

FROM debian:bookworm-slim

# Not scratch, and not distroless. The binary would run in either, and the
# agent would be able to do nothing: exec_command is most of what it is for,
# and an image with no shell has nothing to run. This is a larger image on
# purpose.
#
# git is the one command the code invokes by name. ca-certificates is what
# every provider call needs — without it they fail at TLS, which reads like a
# wrong key rather than a missing package.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		ca-certificates \
		git \
	&& rm -rf /var/lib/apt/lists/*

# A user of its own. The agent runs commands somebody approved, which is a
# different thing from commands somebody wrote, and root in a container is one
# careless flag away from root on the host.
#
# A fixed uid so a bind mount from the host can be given to it: `chown 10001`
# on the directory being mounted is the documented way in, and a uid that
# moved between builds would make that advice wrong later.
RUN useradd --system --uid 10001 --create-home --home-dir /home/jingclaw \
		--shell /bin/bash jingclaw \
	&& mkdir -p /var/lib/jingclaw \
	&& chown jingclaw:jingclaw /var/lib/jingclaw

COPY --from=build /out/jingclaw /usr/local/bin/jingclaw
COPY docs/config.example.toml /usr/share/jingclaw/config.example.toml

# Where the deployment lives, and the one path to mount.
ENV JINGCLAW_HOME=/var/lib/jingclaw
VOLUME ["/var/lib/jingclaw"]

USER jingclaw
WORKDIR /var/lib/jingclaw

# The supervisor, which starts the daemon and the gateway and already knows
# what to do with no terminal to draw on. One container rather than two: the
# parts find each other through a discovery file whose whole purpose is to be
# local, and sharing it between containers would make a local thing a network
# one.
ENTRYPOINT ["jingclaw"]
