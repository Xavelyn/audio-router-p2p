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
