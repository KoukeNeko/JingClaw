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

# A Python base rather than debian-slim plus an apt python. python:3-slim IS
# debian-slim with a CPython built into /usr/local, so the base does not
# change and neither does the size — what it changes is that pip installs
# system-wide with no externally-managed lock, so there is no virtual
# environment and no wrapper to make python3 on PATH resolve to it. The
# earlier venv did both, and a symlink into it resolved through to the system
# python and lost its own packages. Off that path entirely by not having one.
FROM python:3.13-slim-bookworm

# Not scratch, and not distroless. The binary would run in either, and the
# agent would be able to do nothing: exec_command is most of what it is for,
# and an image with no shell has nothing to run. This is a larger image on
# purpose.
#
# git is the one command the code invokes by name. ca-certificates is what
# every provider call needs — without it they fail at TLS, which reads like a
# wrong key rather than a missing package.
#
# tini is what makes PID 1 an init. Measured rather than assumed: with the
# supervisor as PID 1, a grandchild orphaned by its parent exiting is
# re-parented to it and stays a zombie, because Go reaps the children os/exec
# started and nothing else. This agent runs shells, build tools and whatever
# somebody approved, all of which fork, so those accumulate until the process
# table is full and nothing can start.
#
# In the image rather than left to `docker run --init`, because the failure
# is silent and the flag is easy to forget. Passing --init as well is
# harmless: it puts docker-init in front of this one.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		ca-certificates \
		git \
		tini \
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

# What reading a page needs, and the largest thing in this image by far.
#
# web.enabled is off by default and this changes nothing about that. What it
# changes is what happens when somebody turns it on: the daemon refuses to
# start without an interpreter that can import the package, which in a
# container is a wall rather than a missing package — an image carries what it
# carries, and the operator reading "cannot import cloakbrowser" has nowhere
# to install it.
#
# System-wide, because this base's python is under /usr/local and carries no
# externally-managed marker, so pip installs into the python that is already
# python3 on PATH. Nothing to activate and nothing to point at.
#
# playwright install-deps rather than a hand-written package list: cloakbrowser
# drives a Chromium, the shared libraries one needs are its own business, and a
# list written here would be right until the day it was not.
RUN pip install --no-cache-dir cloakbrowser \
	&& playwright install-deps chromium \
	&& python3 -c 'import cloakbrowser' \
	&& rm -rf /var/lib/apt/lists/*

COPY --from=build /out/jingclaw /usr/local/bin/jingclaw
COPY docs/config.example.toml /usr/share/jingclaw/config.example.toml

# The browser cloakbrowser drives is NOT in this image, and must not be.
#
# It is a compiled Chromium under its own licence — free to use, and not to
# redistribute, which is what putting it in a published image would be. The
# wrapper above is MIT and ships here; the binary it fetches is fetched by the
# deployment that runs it, under that deployment's own terms.
#
# Into the volume rather than the container's writable layer, so the ~200MB
# download happens on the first page read and not again every time the
# container is replaced.
ENV CLOAKBROWSER_CACHE_DIR=/var/lib/jingclaw/browser

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
# tini forwards the signal to its immediate child and reaps whatever the
# kernel hands it. What it does not do is signal the whole tree — so the
# supervisor still terminates its own parts, which it does, and must: docker
# stop signals PID 1 alone, and the moment PID 1 exits the kernel SIGKILLs
# everything else in the namespace.
ENTRYPOINT ["/usr/bin/tini", "--", "jingclaw"]
