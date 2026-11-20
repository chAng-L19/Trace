#!/bin/sh
# Frozen Ubuntu 22.04 amd64 build input, never a runtime network installer.
set -eu
bundle=${1:?frozen capability bundle required}
lock=${2:?source-controlled sha256 lock required}
cd "$bundle"
sha256sum -c "$lock"
font_dir=/usr/local/share/fonts/trace-noto-cjk
license_dir=/opt/trace/licenses/noto-cjk
mkdir -p "$font_dir" "$license_dir"
chmod 0755 "$font_dir" "$license_dir"
install -m 0644 ./NotoSansCJK-Regular.ttc "$font_dir/NotoSansCJK-Regular.ttc"
install -m 0644 ./NotoSansCJK-LICENSE.txt "$license_dir/LICENSE.txt"
# Fontconfig scans the standard font directory even when its optional CLI is
# absent from the frozen image. No download or browser/persona change occurs.
if command -v fc-cache >/dev/null 2>&1; then fc-cache -f "$font_dir"; fi
dpkg -i ./libcbor0.8_0.8.0-2ubuntu1_amd64.deb ./libfido2-1_1.10.0-1_amd64.deb \
    './openssh-client_1%3a8.9p1-3ubuntu0.18_amd64.deb' ./sshpass_1.09-1_amd64.deb \
    './xxd_2%3a8.2.3995-1ubuntu2.36_amd64.deb'
python3 -m pip install --no-index --no-deps --no-compile --target /opt/trace/runtime/site-packages \
    ./pydash-8.0.5-py3-none-any.whl
python3 -m pip install --no-index --require-hashes --find-links "$bundle" --no-compile --upgrade \
    --target /opt/trace/runtime/site-packages -r "$(dirname "$lock")/http-requirements.txt"
# Install pip's generated official CLI entry under both managed command names.
install -m 0755 /opt/trace/runtime/site-packages/bin/curl-cffi /usr/local/bin/curl
install -m 0755 /opt/trace/runtime/site-packages/bin/curl-cffi /usr/local/bin/curl-cffi
command -v ssh sshpass xxd
python3 -c 'import importlib.metadata,pydash; assert importlib.metadata.version("pydash")=="8.0.5"; d={}; pydash.set_(d,"a.b",7); assert d=={"a":{"b":7}}'
