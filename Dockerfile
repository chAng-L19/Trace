# syntax=docker/dockerfile:1.7
ARG PYTHON_IMAGE=python:3.12.12-slim-bookworm@sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c
FROM ${PYTHON_IMAGE} AS build
ENV PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1
WORKDIR /build
COPY pyproject.toml README.md LICENSE ./
COPY src ./src
RUN python -m pip wheel --wheel-dir /wheels . \
    && python -m venv /opt/venv \
    && /opt/venv/bin/pip install --no-index --find-links=/wheels trace-agent

FROM ${PYTHON_IMAGE} AS runtime
ENV PATH="/opt/venv/bin:${PATH}" \
    PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1 \
    TRACE_HOME=/var/lib/trace TRACE_TOOLS_HOME=/var/cache/trace/tools TRACE_BIN=/opt/venv/bin \
    TRACE_WEB_HOST=0.0.0.0 TRACE_WEB_PORT=8765 \
    XDG_DATA_HOME=/var/lib/trace/data XDG_CONFIG_HOME=/var/lib/trace/config \
    XDG_CACHE_HOME=/var/cache/trace \
    PLAYWRIGHT_BROWSERS_PATH=/var/cache/trace/ms-playwright
COPY --from=build /opt/venv /opt/venv
ARG TRACE_INSTALL_BROWSER=1
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git tini \
    && python -m playwright install-deps chromium \
    && if [ "$TRACE_INSTALL_BROWSER" = 1 ]; then python -m playwright install chromium; fi \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10001 --shell /usr/sbin/nologin trace \
    && mkdir -p /var/lib/trace/artifact-store /var/lib/trace/workspaces /var/cache/trace \
    && chown -R trace:trace /var/lib/trace /var/cache/trace
COPY --chmod=0555 deploy/run-trace.sh /opt/trace/run-trace.sh
COPY deploy/healthcheck.py /opt/trace/healthcheck.py
USER 10001:10001
WORKDIR /var/lib/trace/workspaces
VOLUME ["/var/lib/trace", "/var/lib/trace/artifact-store", "/var/lib/trace/workspaces", "/var/cache/trace"]
EXPOSE 8765
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD ["python", "/opt/trace/healthcheck.py"]
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/trace/run-trace.sh"]
CMD ["web"]
