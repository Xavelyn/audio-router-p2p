# Security boundaries

`audio-router-p2p` is designed for a trusted LAN. It is not yet an
adversarial-network-safe discovery or key-management protocol.

## Discovery and clock sync

- Peer announcements include `-auth-token` in a JSON UDP payload. The token is
  compared in constant time, but it is not encrypted, integrity-protected, or
  replay-protected. Anyone who can observe an announcement can learn it.
- Discovery uses multicast plus unicast fallback. Scope multicast to a trusted
  broadcast domain or VLAN; do not expose the discovery, sync, or audio UDP
  ports directly to an untrusted network.
- Clock sync now admits datagrams only from a source endpoint currently learned
  through discovery, and attributes offsets to that discovered peer rather than
  to the sender-claimed ID. Sync messages are still not cryptographically
  authenticated, so this is admission control rather than a replacement for
  message authentication.

## DTLS/SRTP

Certificate mode uses self-signed certificates and deliberately skips
certificate-chain and hostname verification. This encrypts audio against passive
eavesdropping, but it does **not** authenticate the remote certificate and is
not safe against an active man-in-the-middle. Do not use certificate mode as a
substitute for an authenticated network boundary.

When `-encrypt` is used with a high-entropy, secret `-psk`, DTLS uses a
pre-shared-key cipher suite instead. A peer with the wrong PSK cannot complete
the handshake. PSK mode authenticates the DTLS peer only while that PSK remains
secret; discovery itself is still protected only by the plaintext token.

## Deployment guidance

- Prefer a trusted wired/wireless LAN, a VPN, or an otherwise authenticated
  network overlay.
- Use a unique, high-entropy authentication token and protect it like a
  password. Rotate it if it may have been observed.
- For encrypted audio across an untrusted network, configure a strong shared
  PSK and treat the discovery channel as untrusted until it is hardened.
- Do not publish the discovery multicast group or UDP ports to the Internet.

## Planned hardening

- Replace the plaintext discovery token with a keyed MAC over canonical
  announcement fields, including a nonce and timestamp, with replay handling.
- Derive and verify a message authentication key for clock-sync requests and
  responses instead of relying only on source-address admission.
- Add optional certificate verification for certificate mode, using a private
  CA or a configured certificate/public-key pin.
