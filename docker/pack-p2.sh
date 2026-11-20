#!/bin/bash
# Server-only: copy an accepted image; install nothing and run no product.
set -euo pipefail
if [[ $# != 2 ]]; then
  printf 'usage: bash docker/pack-p2.sh IMAGE NEW_OUTPUT_DIR\n' >&2
  exit 2
fi
source_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
image=$1
image_id=$(docker image inspect --format '{{.Id}}' "$image")
capability_bundle=${TRACE_CAPABILITY_BUNDLE:?set TRACE_CAPABILITY_BUNDLE to the frozen hash-locked build inputs}
capability_bundle=$(realpath -e -- "$capability_bundle")
(cd "$capability_bundle" && sha256sum -c "$source_root/docker/capabilities.sha256")
output=$(realpath -m -- "$2")
[[ ! -e "$output" ]] || { printf 'output path must be new\n' >&2; exit 2; }
mkdir -- "$output"
root="$output/trace-p2-linux-amd64"
mkdir -p "$root/bin" "$root/.trace" "$root/docker" "$root/examples" "$root/scripts"
container=$(docker create "$image_id")
cleanup() {
  if [[ -n ${container:-} ]]; then docker rm "$container" >/dev/null; fi
}
trap cleanup EXIT
# Never copy /state, /workspace, /home, image env, or the Trace source tree.
docker cp "$container:/usr/local/bin/trace" "$root/bin/trace"
for directory in runtime tools licenses prompts; do
  docker cp "$container:/opt/trace/$directory" "$root/$directory"
done
docker cp "$container:/opt/trace/.trace/skills" "$root/.trace/skills"
docker cp "$container:/opt/trace/examples/tsecbench-task.md" "$root/examples/tsecbench-task.md"
docker cp "$container:/opt/trace/examples/tsecbench_api.py" "$root/examples/tsecbench_api.py"
docker cp "$container:/opt/trace/docker/entrypoint.sh" "$root/docker/entrypoint.sh"
cp -- "$source_root/.dockerignore" "$root/.dockerignore"
cp -- "$source_root/scripts/http-requirements.txt" "$root/scripts/http-requirements.txt"
for file in Dockerfile run-linux.sh hosted.env.example install-capabilities.sh capabilities.sha256 lightpanda.sha256; do
  cp -- "$source_root/docker/$file" "$root/docker/$file"
done
test -x "$root/runtime/lightpanda/lightpanda"
(cd "$root/runtime/lightpanda" && sha256sum -c "$root/docker/lightpanda.sha256")
test -s "$root/.trace/skills/lightpanda/SKILL.md"
mkdir -p "$root/runtime/capabilities"
while read -r digest name; do
  cp -- "$capability_bundle/$name" "$root/runtime/capabilities/$name"
done < "$source_root/docker/capabilities.sha256"
cp -- "$source_root/docker/README.zh-CN.md" "$root/README.zh-CN.md"
python3 - "$root" <<'PY'
import importlib.metadata, json, os, pathlib, sys
root = pathlib.Path(sys.argv[1])
for name in ('pi-agent-core', 'pi-ai'):
    package = root / 'runtime/pi/node_modules/@earendil-works' / name / 'package.json'
    if json.loads(package.read_text())['version'] != '1.4.0':
        raise SystemExit('expected pinned pi 1.4.0: ' + name)
cloak = root / 'runtime/cloakbrowser'
versions = {dist.metadata['Name'].lower(): dist.version
            for dist in importlib.metadata.distributions(path=[str(cloak / 'site-packages')])}
if versions.get('cloakbrowser') != '0.6.4' or 'playwright' not in versions:
    raise SystemExit('expected CloakBrowser 0.6.4 and its Python Playwright protocol dependency')
if not os.access(cloak / 'binary/chrome', os.X_OK):
    raise SystemExit('expected executable frozen CloakBrowser binary/chrome')
js_package = root / 'tools/web-analysis/node_modules/cloakbrowser/package.json'
if json.loads(js_package.read_text())['version'] != '0.6.4':
    raise SystemExit('expected JavaScript CloakBrowser 0.6.4')
for old in ('runtime/chromium', 'runtime/playwright', 'runtime/site-packages/DrissionPage',
            '.trace/skills/drissionpage', '.trace/skills/playwright-cli'):
    if (root / old).exists():
        raise SystemExit('superseded browser runtime remains: ' + old)
if not (root / '.trace/skills/cloakbrowser/SKILL.md').is_file():
    raise SystemExit('expected CloakBrowser skill')
PY
docker rm "$container" >/dev/null
container=
tar -C "$output" -czf "$output/trace-p2-linux-amd64.tar.gz" trace-p2-linux-amd64
docker save "$image_id" | gzip > "$output/trace-p2-docker.tar.gz"
printf '%s\n' "$image_id" > "$output/image-id.txt"
python3 - "$output" <<'PY'
import hashlib, json, pathlib, sys, tarfile
root = pathlib.Path(sys.argv[1])
with tarfile.open(root / 'trace-p2-docker.tar.gz') as archive:
    manifest = json.load(archive.extractfile('manifest.json'))
    if len(manifest) != 1:
        raise SystemExit('expected exactly one exported image')
    config = archive.extractfile(manifest[0]['Config']).read()
    (root / 'image-config-id.txt').write_text('sha256:' + hashlib.sha256(config).hexdigest() + '\n')
PY
printf 'Packaged: %s\n' "$output"

