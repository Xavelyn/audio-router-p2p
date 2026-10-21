# Headless VM development

No sound server, audio device, or PortAudio installation is needed for backend
development. The `noaudio` build tag provides a synthetic sine-wave input,
an in-memory output, and emulated device listings. It is intended for tests
and development, not for validating real-device audio quality.

From the repository root, install the local Go toolchain and run the backend
tests:

```bash
bash tools/bootstrap.sh
source tools/env.sh
go test -tags noaudio ./...
```

`tools/bootstrap.sh` installs Go in `.tools/`, caches modules in `.gopath/`,
and generates `tools/env.sh`. These paths are local to this checkout and
ignored by Git. Source `tools/env.sh` in each new shell, or run the bootstrap
script again (it reuses an existing installation).

To start the daemon with emulated audio, use a unique secret token and keep
its generated state outside the repository:

```bash
source tools/env.sh
go run -tags noaudio ./cmd/client \
  -use-emulator \
  -auth-token 'replace-with-a-long-random-token' \
  -routes /tmp/opencode/audio-router-routes.json \
  -cert /tmp/opencode/audio-router-cert.pem \
  -key /tmp/opencode/audio-router-key.pem
```

The API listens on `https://localhost:8443` with a self-signed certificate
unless configured otherwise. See `SECURITY.md` before exposing any ports.
The VM has Node.js, npm, and a C compiler installed. The frontend dependencies
come from `web/package-lock.json`; install and build from the `web/` directory:

```bash
cd web
npm ci
npm run build
```

For a local frontend development server, run `npm run dev` in `web/`. The
default Vite proxy targets `http://127.0.0.1:8000`, while the daemon above
uses HTTPS on port 8443; set `VITE_BACKEND_URL` to a matching backend URL when
using the dev server, or adjust the daemon's `-listen` and `-no-tls` settings.

To run all headless backend tests under Go's race detector:

```bash
source tools/env.sh
CGO_ENABLED=1 go test -tags noaudio -race ./...
```

For two-daemon audio, configure the same route ID and `-auth-token` on both
nodes. A sending `StreamRoute` uses `direction: "send"` (or omits the field)
and lists the receiver's stream UDP address under `destinations`. On the
receiver, use `direction: "receive"` with the same ID and a local
`destDevice` or `destDeviceUrn`. Each daemon needs its own routes file and
distinct listen, discovery, sync, and stream ports when running on one VM.

Daemon PCM16 routes automatically choose the largest whole-millisecond packet
duration (up to 20 ms) that keeps the audio datagram below 1,150 bytes,
including the wire header and room for FEC. For example, 48 kHz stereo uses
5 ms packets rather than fragmented 20 ms packets. Shorter packets cost more
headers and CPU; this is not a bandwidth-saving codec. Both route endpoints
must use matching sample rate and channel count. Unsupported high-bandwidth
formats fail rather than silently sending oversized datagrams.

Matrix-route coefficients and mute are applied to live PCM with sample-slewed
changes. Sending routes process audio before network transmission; receive-only
routes process audio before local playback. Live matrix routes currently require
equal input/output channel counts; a route requiring channel-count conversion
returns an error rather than silently ignoring its matrix.

Compatible routes aimed at the same local output now share one device clock
and are mixed into one saturated PCM block per tick. Each route keeps at most
one pending block, replacing stale audio rather than letting output latency
grow without bound. Routes sharing an output must use the same sample rate and
channel count; an incompatible route is rejected without closing the active
output. This is a bounded real-time mixer, not sample-accurate synchronization
between independent network sources.
