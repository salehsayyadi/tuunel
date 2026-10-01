# Wire protocol (version 1)

All integers are big-endian. Every carrier delivers whole *messages*: datagram
carriers send one message per datagram, stream carriers (TCP, QUIC streams,
WebSocket binary frames over TCP) prefix each message with a 2-byte length.

## Handshake: Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s

- Prologue: `tuunel/1 Noise_IKpsk2_25519_ChaChaPoly_BLAKE2s`
- The initiator knows the responder's static public key (IK). The PSK slot is
  the configured pre-shared key, or 32 zero bytes when none is configured.
- Message type 1 (`HandshakeInit`): `0x01 | noise message 1`.
  Encrypted payload: `version(1)=1 | flags(1) | sender_index(4) | timestamp_ns(8) | id_len(1) | node_id`.
  Flag bit 0 = probe session (never becomes the data link).
- Message type 2 (`HandshakeResp`): `0x02 | noise message 2`.
  Encrypted payload: `version(1)=1 | 0x00 | sender_index(4)`.
- The responder authorizes the initiator's static key against configured peers
  before replying. It rejects initiations whose timestamp is more than 60 s older
  than the newest accepted one for that key, or that were already seen
  (replayed initiation). Consequence: if an initiator's clock steps back by
  more than 60 s, the responder rejects its initiations until the responder
  restarts. Keep clocks synchronized (NTP).
- Datagram carriers retransmit `HandshakeInit` until a response or
  `handshake_timeout`.

## Data: type 3

```text
0x03 | receiver_index(4) | counter(8) | ChaCha20-Poly1305( inner_type(1) | body ) 
```

- Nonce = counter; the 13-byte header is the AEAD associated data.
- Receivers drop counters outside a 2048-message sliding window or already seen
  (RFC 6479 bitmap; 1984 positions are guaranteed usable behind the highest
  counter).
- Rekey after 2^48 messages or `rekey_interval`; keys are rejected after 2^60.
- Per-packet overhead: 13 + 1 + 16 = **30 bytes** plus carrier overhead
  (tcp 34, udp 8, quic stream 52, quic datagram 36, ws/wss 70, icmp 12, bytes,
  outer IP header excluded).

### Inner types

| Type | Name | Body |
|---|---|---|
| 1 | IP | one IPv4/IPv6 packet; source must be in the sender's allowed IPs |
| 2 | Ping | `id(8) | padding` (8–1200 bytes) |
| 3 | Pong | echo of the Ping body (same size: no amplification) |
| 4 | Close | link is being closed by the sender |

## ICMP carrier (experimental, IPv4)

Echo request/reply with identifier per link; payload starts with magic
`TUNQ` (client→server) or `TUNR` (server→client) followed by the session
message. Max message 1400 bytes. Requires `CAP_NET_RAW` and
`experimental.icmp: true`, and usually `net.ipv4.icmp_echo_ignore_all=1` on the
server so the kernel does not answer the same requests. That sysctl also
stops the host from answering ordinary pings, including pings to its tunnel
address.

## Compatibility

The audit changed no wire format. The dependency upgrades (quic-go v0.63,
x/net v0.59) keep QUIC v1/RFC 9221 DATAGRAM and RFC 6455 WebSocket framing.
Interoperability between builds from before and after the audit was not
tested, so upgrade both nodes together.

## Versioning

Unknown versions are rejected. A future version changes the prologue, so
mixed versions fail the handshake explicitly instead of misinterpreting data.
