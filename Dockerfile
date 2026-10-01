# syntax=docker/dockerfile:1
# tuunel container images.
#   runtime (default): minimal non-root daemon image
#       docker build -t tuunel .
#   lab: netns test lab (privileged)   docker build --target lab -t tuunel-lab .
ARG GO_VERSION=1.27
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN go vet ./... && go test ./... \
 && for c in tuunel tunnelctl; do \
      CGO_ENABLED=0 go build -trimpath -buildvcs=false \
        -ldflags "-s -w -buildid= -X github.com/salehsayyadi/tuunel/internal/daemon.Version=${VERSION} -X github.com/salehsayyadi/tuunel/internal/daemon.Commit=${COMMIT}" \
        -o /out/$c ./cmd/$c; done

# ---------------------------------------------------------------- runtime
# Non-root (uid 10001). The binary carries file capabilities
# (NET_ADMIN for TUN/netlink, NET_RAW for the experimental ICMP carrier,
# NET_BIND_SERVICE for ports < 1024); the container must be started with
# --cap-add NET_ADMIN and --device /dev/net/tun so that the capabilities are
# inside the bounding set. nft (for the ICMP reply filter) gets NET_ADMIN too.
FROM debian:bookworm-slim AS runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates nftables libcap2-bin \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/tuunel /out/tunnelctl /usr/local/bin/
RUN setcap 'cap_net_admin,cap_net_raw,cap_net_bind_service+ep' /usr/local/bin/tuunel \
 && setcap 'cap_net_admin+ep' /usr/sbin/nft \
 && apt-get purge -y libcap2-bin && apt-get autoremove -y \
 && groupadd --system --gid 10001 tuunel \
 && useradd --system --uid 10001 --gid 10001 --home-dir /var/lib/tuunel --shell /usr/sbin/nologin tuunel \
 && install -d -m 0750 -o tuunel -g tuunel /run/tuunel /var/lib/tuunel \
 && install -d -m 0750 -o root -g tuunel /etc/tuunel
USER 10001:10001
VOLUME ["/etc/tuunel"]
# The daemon answers on its management socket once started; "status" fails
# (non-zero) when the daemon is not responsive. Tunnel/peer state is reported
# by "tunnelctl status" / metrics, not by container health.
HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/usr/local/bin/tunnelctl", "-socket", "/run/tuunel/tuunel.sock", "status"]
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/tuunel"]
CMD ["run", "-config", "/etc/tuunel/config.yaml"]

# ---------------------------------------------------------------- lab
FROM debian:bookworm-slim AS lab
RUN apt-get update && apt-get install -y --no-install-recommends \
      iproute2 iptables nftables iputils-ping iperf3 python3 procps kmod ethtool ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
COPY tests/lab /lab
RUN chmod +x /lab/*.sh
ENTRYPOINT ["/lab/netns-acceptance.sh", "/usr/local/bin"]
