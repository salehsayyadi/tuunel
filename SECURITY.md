# Security notes

- Use TLS 1.3 mutual authentication with private CA-issued certificates. Protect private keys as secrets and rotate/revoke them operationally.
- Configure the server certificate with the DNS name used by clients. The client must not disable certificate or hostname verification.
- The frame sequence is strict per direction; TLS additionally provides record-level integrity and replay/reordering defenses over TCP.
- Packet size, IP version, header lengths and frame sequence are validated before writing to TUN. The output queue is bounded.
- TUN configuration, routes and firewall are local administrator responsibilities. Never accept remote shell commands or route changes from a peer.
- Run with the minimum Linux privileges that allow TUN access. Restrict the TCP listener with host/network firewalls. Management/API listeners are not implemented.
- Do not log private key contents or packet payloads.

## Known limitations / not yet production ready

No security audit has been performed. There is no configuration schema, rate limiter, connection admission policy, endpoint allowlist, key hot-rotation, formal protocol negotiation, automated route cleanup, integration test with privileged network namespaces, or fuzzing suite yet. Use only in a controlled test environment until these are implemented and reviewed.
