# Build context is the unpacked Linux release; runtime/ contains frozen dependencies.
ARG BASE_IMAGE=ubuntu:22.04
FROM ${BASE_IMAGE}
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl git jq file binutils gdb nmap netcat-openbsd tini \
    unzip xz-utils procps ripgrep sqlite3 openssl util-linux xvfb xauth \
    libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libatspi2.0-0 \
    libcups2 libdrm2 libdbus-1-3 libxkbcommon0 libxcomposite1 libxdamage1 \
    libxfixes3 libxrandr2 libgbm1 libasound2 libpango-1.0-0 libcairo2 \
    libx11-6 libxcb1 libxext6 libxshmfence1 libglib2.0-0 libgtk-3-0 \
    libexpat1 libfontconfig1 libfreetype6 fonts-liberation \
    && rm -rf /var/lib/apt/lists/*
COPY runtime/ /opt/trace/runtime/
COPY docker/lightpanda.sha256 /opt/trace/runtime/lightpanda/SHA256SUMS
RUN cd /opt/trace/runtime/lightpanda && sha256sum -c SHA256SUMS
ENV PATH="/opt/trace/runtime/python/bin:/opt/trace/runtime/node/bin:${PATH}" \
    PYTHONPATH="/opt/trace/runtime/cloakbrowser/site-packages:/opt/trace/runtime/site-packages:/opt/trace/runtime/ASC" \
    PYTHONIOENCODING=utf-8 PYTHONUTF8=1 PYTHONDONTWRITEBYTECODE=1 \
    TRACE_DATA_DIR=/state TRACE_PROMPT_FILE=/opt/trace/prompts/agent.md \
    TRACE_SKILL_PATHS=/opt/trace/.trace/skills \
    TRACE_WEB_ANALYSIS_DIR=/opt/trace/tools/web-analysis \
    TRACE_PI_RUNTIME=/opt/trace/runtime/pi \
    LIGHTPANDA_DISABLE_TELEMETRY=true \
    LIGHTPANDA_BINARY_PATH=/opt/trace/runtime/lightpanda/lightpanda \
    CLOAKBROWSER_BINARY_PATH=/opt/trace/runtime/cloakbrowser/binary/chrome \
    CLOAKBROWSER_AUTO_UPDATE=false \
    TRACE_BROWSER_NO_SANDBOX=1 \
    REA_BROWSER_NO_SANDBOX=true \
    NO_UPDATE_NOTIFIER=1
RUN cd /opt/trace/runtime/pi \
    && node --input-type=module -e 'import {readFileSync} from "node:fs"; for (const name of ["pi-agent-core","pi-ai"]) { const p=JSON.parse(readFileSync("node_modules/@earendil-works/"+name+"/package.json")); if(p.version!=="1.4.0") throw new Error(name+" must be pinned to 1.4.0"); } await import("@earendil-works/pi-agent-core");' \
    && chmod -R a+rX /opt/trace/runtime/cloakbrowser \
    && test -x "$LIGHTPANDA_BINARY_PATH" \
    && test -x "$CLOAKBROWSER_BINARY_PATH" \
    && test -x /usr/bin/xvfb-run \
    && test ! -e /opt/trace/runtime/chromium \
    && test ! -e /opt/trace/runtime/playwright \
    && python3 -c 'import cloakbrowser, importlib.util, sys; assert sys.version_info[:2] == (3, 12); assert cloakbrowser.__version__ == "0.6.4"; assert importlib.util.find_spec("DrissionPage") is None' \
    && chmod -R a+rX /opt/trace/runtime/ASC \
    && useradd -m -u 1000 trace \
    && mkdir -p /state /workspace \
    && chown trace:trace /state /workspace
# Keep frozen dependencies before product files; copy web-analysis only once.
# Reference: Muteki c60c8fe docker/web/Dockerfile:79-84 (dependency cache order).
COPY tools/web-analysis/ /opt/trace/tools/web-analysis/
COPY .trace/skills/ /opt/trace/.trace/skills/
COPY licenses/ /opt/trace/licenses/
# Build context supplies the tiny, hash-locked offline bundle; never fetch at startup.
COPY runtime/capabilities/ /tmp/trace-capabilities/
COPY docker/install-capabilities.sh docker/capabilities.sha256 scripts/http-requirements.txt /tmp/trace-capability-build/
RUN sh /tmp/trace-capability-build/install-capabilities.sh /tmp/trace-capabilities /tmp/trace-capability-build/capabilities.sha256 \
    && rm -rf /tmp/trace-capabilities /tmp/trace-capability-build
COPY bin/trace /usr/local/bin/trace
COPY prompts/ /opt/trace/prompts/
COPY examples/tsecbench-task.md /opt/trace/examples/tsecbench-task.md
COPY examples/tsecbench_api.py /opt/trace/examples/tsecbench_api.py
COPY docker/entrypoint.sh /opt/trace/docker/entrypoint.sh
RUN chmod 0755 /usr/local/bin/trace /opt/trace/docker/entrypoint.sh \
    && test -s /opt/trace/.trace/skills/cloakbrowser/SKILL.md \
    && test -s /opt/trace/.trace/skills/lightpanda/SKILL.md \
    && test ! -e /opt/trace/.trace/skills/drissionpage \
    && test ! -e /opt/trace/.trace/skills/playwright-cli \
    && cd /opt/trace/tools/web-analysis \
    && node --input-type=module -e 'import {readFileSync} from "node:fs"; if(JSON.parse(readFileSync("node_modules/cloakbrowser/package.json")).version!=="0.6.4") throw new Error("CloakBrowser must be pinned to 0.6.4"); await import("cloakbrowser");'
USER trace
WORKDIR /workspace
STOPSIGNAL SIGINT
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/opt/trace/docker/entrypoint.sh"]
CMD ["benchmark"]
