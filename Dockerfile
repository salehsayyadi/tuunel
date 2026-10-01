# Build + unit tests
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go vet ./... && go test ./... \
 && CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /out/tuunel ./cmd/tuunel \
 && CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /out/tunnelctl ./cmd/tunnelctl

# Lab/runtime image: has the tools the netns lab scripts need.
FROM debian:bookworm-slim AS lab
RUN apt-get update && apt-get install -y --no-install-recommends \
      iproute2 iptables iputils-ping python3 procps kmod ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
COPY tests/lab /lab
RUN chmod +x /lab/*.sh
ENTRYPOINT ["/lab/netns-acceptance.sh", "/usr/local/bin"]
