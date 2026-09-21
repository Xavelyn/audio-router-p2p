#!/usr/bin/env bash
# Idempotent bootstrap: place a Go toolchain + module cache inside the repo
# volume so every container replica sees the same tooling. Sources the
# resulting environment for the current shell.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ ! -x .tools/go/bin/go ]; then
	url="https://go.dev/dl/go1.26.0.linux-amd64.tar.gz"
	echo "downloading $url"
	curl -sSLo /tmp/go.tgz "$url"
	mkdir -p .tools
	tar -C .tools -xzf /tmp/go.tgz
	rm -f /tmp/go.tgz
fi

. tools/env.sh
