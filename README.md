# tuunel

Independent Linux-first, Go-based Layer-3 tunnel MVP. The initial carrier is TCP protected by TLS 1.3 mutual authentication, including explicit client-certificate identity checking via `-peer-name`. This first release is a development baseline, not production-ready software.

## Requirements

- Linux with `/dev/net/tun`, Go 1.23+, `iproute2`, and OpenSSL for test certificates.
- Root or capabilities sufficient to create/configure TUN and routes.
- A CA plus separate node certificates whose EKUs permit server and client authentication. Keep private keys secret.

## Build and test

```sh
go test ./...
go build -o tuunel ./cmd/tuunel
```

## Test certificates

Use a private CA; do not use these commands as a production PKI process. Issue the server certificate with a DNS SAN matching the client's `-server-name`; issue client certificates from the same CA with client-auth EKU. Example with an existing PKI:

```text
server cert: DNS:node-b.example, serverAuth
client cert: DNS:node-a.example, clientAuth
```

Do not disable certificate validation to make a connection work.

## Create and configure TUN

On each host, configure the TUN interface and point-to-point addresses/routes before starting the engine. Example for a controlled two-host test (choose a non-overlapping private subnet):

```sh
sudo ip tuntap add dev tun0 mode tun
sudo ip addr add 10.200.0.1/30 dev tun0   # Node A; use .2 on Node B
sudo ip link set dev tun0 mtu 1300 up
```

The interface is created on first open if it does not already exist, but the address/MTU/route are intentionally configured locally by the administrator. Do not add the same subnet to unrelated interfaces.

## Run

Node B (listener; its certificate DNS SAN must include node-b.example and the client cert SAN must match node-a.example):

```sh
sudo ./tuunel server -listen :9443 -peer-name node-a.example -tun tun0 -mtu 1300 \
  -cert /etc/tuunel/node-b.crt -key /etc/tuunel/node-b.key -ca /etc/tuunel/ca.crt
```

Node A (dialer):

```sh
sudo ./tuunel client -endpoint node-b.example:9443 -server-name node-b.example \
  -tun tun0 -mtu 1300 -cert /etc/tuunel/node-a.crt \
  -key /etc/tuunel/node-a.key -ca /etc/tuunel/ca.crt
```

Permit TCP/9443 only from the intended peer addresses. Test the point-to-point IP addresses with `ping` after configuring each side's tunnel address. Route only explicitly selected networks through TUN; incorrect broad routes can disrupt SSH or system connectivity.

## Design docs

- [Architecture and roadmap](ARCHITECTURE.md)
- [MVP wire protocol](PROTOCOL.md)
- [Security notes and limitations](SECURITY.md)

## Important TCP limitation

The initial carrier is TCP because it is an explicit requirement. Carrying inner TCP over outer TCP can cause head-of-line blocking and TCP-over-TCP collapse when the path loses packets. The carrier API and UDP/QUIC are not implemented yet. Do not claim adaptive carrier failover, reverse forwarding, automatic MTU detection, installer, or production readiness until each is implemented and reproducibly tested.
