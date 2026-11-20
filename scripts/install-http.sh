#!/bin/sh
# Offline CPython 3.12/Linux amd64 install; caller supplies the frozen wheels.
set -eu
release=${1:?release root required}
wheels=${2:?frozen HTTP wheel directory required}
source_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
python3 -m pip install --no-index --require-hashes --no-compile --upgrade \
    --find-links "$wheels" --target "$release/runtime/site-packages" \
    -r "$source_root/scripts/http-requirements.txt"
mkdir -p "$release/runtime/bin"
install -m 0755 "$source_root/scripts/curl" "$release/runtime/bin/curl"
install -m 0755 "$source_root/scripts/curl" "$release/runtime/bin/curl-cffi"
