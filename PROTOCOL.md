# MVP wire protocol

## Transport

One TCP connection protected by TLS 1.3. Both peers present certificates issued by the configured CA; the client additionally verifies the configured server name. TLS provides authenticated encryption, record integrity, and its own record sequence/nonce handling. No custom cryptographic primitive or shared machine identifier is used.

## Packet frame

Each direction has an independent counter starting at 1. A frame is:

| Field | Size | Encoding |
|---|---:|---|
| Sequence | 8 bytes | unsigned big-endian, exact next value required |
| Packet length | 2 bytes | unsigned big-endian, 1..configured MTU |
| IP packet | declared length | IPv4 or IPv6 packet |

Any missing, repeated, skipped, reordered, oversized, truncated or malformed frame terminates that session. Sequence wrap requires establishing a fresh TLS connection. Frames are carried only inside the mutually authenticated TLS stream; plaintext tunnel payload is never sent by the engine.

## Recovery

A broken connection ends the current session. The client reconnects with exponential backoff capped at 30 seconds; the server returns to accept. The TUN interface and IP assignments remain local and stable. This MVP does not promise delivery of packets in flight during reconnect or seamless migration.

## Compatibility

This is an initial private protocol, version 0, not a stable interoperability commitment. Future evolution must add explicit negotiation/versioning before incompatible wire changes.
