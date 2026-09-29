# Linux 与 Docker 部署

支持 Linux 上的 Python 3.11、3.12、3.13；当前便携工具清单和部署门禁覆盖 x86_64。
Docker 默认将 `python:3.12.12-slim-bookworm` 固定到多架构 index digest
`sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c`，
于 2026-09-23 从 Docker Registry 核验；通过 `PYTHON_IMAGE` 可显式更新。
发布时保存构建后的镜像 digest，部署与回滚使用同一 digest。
依赖版本约束由 `pyproject.toml` 管理；apt 仓库、构建依赖与 pip 传递依赖仍可能更新，
源码重建不保证逐字节相同，应归档 wheel、依赖 wheelhouse 和镜像 digest。

## Docker Compose

需要 Docker Engine 与 Compose v2（支持 `up --wait`）。在仓库根目录运行：

```sh
cp deploy/trace.env.example .env
chmod 600 .env
${EDITOR:-vi} .env  # 设置非空 TRACE_ADMIN_PASSWORD；需要模型时填写 Provider 三项
docker compose up --build -d --wait
curl --fail http://127.0.0.1:8765/api/auth/status
docker compose logs --tail=30 trace
```

浏览器打开 `http://127.0.0.1:8765`，使用 `TRACE_ADMIN_PASSWORD` 登录。模型配置为
`TRACE_MODEL`、`TRACE_API_BASE_URL`、`OPENAI_API_KEY`；也可先留空，在 Web 中配置。
可选 `TRACE_*` Provider 项留空时使用 Web 持久配置，再回落到应用默认值；显式填写
宿主环境或 `.env` 的值会在重启时优先于 Web 配置。Web 录入的 API Key 仅驻留当前
进程，重启后需重新输入，或在 Web 中填写凭据环境变量名并通过容器环境注入该变量。
示例直接传入 `OPENAI_API_KEY`；使用其他名称时，在自定义 Compose 中一并传入对应变量。
`.env` 必须留在本机，排除在构建上下文之外；镜像构建不接收密钥。容器运行环境对有
Docker 管理权的用户可见；密钥也支持 `TRACE_ADMIN_PASSWORD_FILE`、`OPENAI_API_KEY_FILE`
指向只读挂载的文件，使用时在自定义 Compose 中去掉对应的明文环境变量。

容器以 UID/GID `10001:10001` 运行、根文件系统只读、丢弃全部 capabilities。
默认仅发布宿主机 `127.0.0.1`；容器内部监听 `0.0.0.0` 并强制登录。
`TRACE_ALLOW_INSECURE_HTTP=1` 仅对应这一回环端口部署。远程访问应由本机反向代理提供
HTTPS，或挂载证书并设置 `TRACE_TLS_CERT`、`TRACE_TLS_KEY`。直接 TLS 时另设
`TRACE_HEALTH_URL=https://.../api/auth/status` 并为检查进程配置可信 CA。

| 卷 | 容器路径 | 内容 |
| --- | --- | --- |
| `trace-state` | `/var/lib/trace` | SQLite/WAL、配置、凭据加密密钥、原始证据 |
| `trace-artifacts` | `/var/lib/trace/artifact-store` | CAS 工件 |
| `trace-workspaces` | `/var/lib/trace/workspaces` | 每次运行的工作区 |
| `trace-cache` | `/var/cache/trace` | 工具下载与 Playwright 浏览器缓存 |

首次创建 named volume 会继承镜像目录所有权。使用宿主机 bind mount 时，先将对应
目录授予 UID/GID 10001 写权限；单个待分析项目可只读挂载到工作区的子目录。
不要挂载宿主机 Docker socket。健康检查验证 `/api/auth/status` 的 HTTP 状态、
Trace schema 和响应字段，不调用 Provider，也不代表外部工具或模型已经联网可用。

```sh
docker compose stop                 # SIGTERM，最多等待 45 秒；保留全部卷
docker compose up -d --wait          # 继续使用原有持久状态
docker compose down                 # 移除容器，保留卷；不要加 --volumes
docker compose run --rm --no-deps trace cli self-test
docker compose run --rm --no-deps -T trace mcp  # stdio MCP；客户端使用 stdin/stdout
```

MCP/CLI 是短命令或 stdio 服务，不使用 Web 健康检查；长期运行 MCP 容器时设置
`healthcheck: { disable: true }`、`stdin_open: true`。容器的 Web 启动脚本通过 `exec`
交接进程，tini 转发信号并回收子进程；Web 对 SIGTERM 和 Ctrl+C 执行同一关闭路径。

镜像默认预置 Chromium 和 Linux 动态库，不需要第一次浏览时再下载浏览器。
`TRACE_INSTALL_BROWSER=0 docker compose build` 可跳过浏览器文件以减少镜像；动态库
仍在镜像中，运行时工具 setup 可以在可写缓存卷中安装浏览器。浏览器缓存卷跨版本
升级后如缺少新 revision，可运行 `trace setup chromium` 补齐；`trace doctor`/`trace setup`
负责工具准备，镜像不在启动时执行 apt 或修改系统包。内置 Python Playwright
不需要 Node.js；外接 `config.toml.example` 的 `npx @playwright/mcp` 需要另外安装
Node.js，该可选 MCP 不属于基础镜像默认入口。

容器升级先停止服务，归档旧镜像和四个卷。下面的归档经 stdout 写入宿主机，
不需要让容器写宿主机目录：

```sh
umask 077
backup_dir=$(mktemp -d "$HOME/trace-backup-XXXXXXXX")
docker image tag "$(docker compose images -q trace)" trace-agent:previous
docker compose stop
docker compose run --rm --no-deps -T trace tar -C / -czf - \
  var/lib/trace var/cache/trace > "$backup_dir/trace-data.tar.gz"
cp .env "$backup_dir/trace.env"
docker compose build
docker compose up -d --wait
```

回滚恢复该次升级前的完整快照，包含 SQLite/WAL、CAS 和 `trace-secrets.key`。
使用新 Compose project 创建独立卷，保留升级后的数据供排查；不假定旧版本理解新
数据库结构。`backup_dir` 必须指向刚才备份的位置：

```sh
docker compose stop
export TRACE_IMAGE=trace-agent:previous
docker compose --env-file "$backup_dir/trace.env" -p trace-rollback \
  run --rm --no-deps -T trace tar -C / -xzf - < "$backup_dir/trace-data.tar.gz"
docker compose --env-file "$backup_dir/trace.env" -p trace-rollback up --no-build -d --wait
```

`trace-rollback` project 名需保持独立；每次新的恢复使用一个尚未存在的 project 名。
跨主机恢复还需预先 `docker image save`/`load` 对应镜像，或从登记的 digest 拉取。

## Linux 用户级安装

先准备 Python 3.11–3.13 和 `venv`（Debian/Ubuntu 按所用版本安装 `python3-venv`），
从可信构建环境取得 wheel。源码构建时在临时副本中运行，避免污染工作目录：

```sh
build_dir=$(mktemp -d)
git archive HEAD | tar -x -C "$build_dir"
python3 -m pip wheel "$build_dir" --wheel-dir "$build_dir/wheels"
sh deploy/install-linux.sh "$build_dir"/wheels/trace_agent-*.whl
```

脚本只接受本地 wheel 的绝对路径，创建独立 venv，执行 `pip check` 和真实 runtime
自检成功后才切换 `current`，旧环境保留为 `previous`。安装目录默认
`${XDG_DATA_HOME:-$HOME/.local/share}/trace`，可用 `TRACE_INSTALL_PREFIX` 覆盖；
`TRACE_PYTHON=python3.13` 可指定解释器。离线安装预先归档全部依赖 wheels，设置
`PIP_NO_INDEX=1 PIP_FIND_LINKS=/absolute/wheelhouse` 后运行相同脚本。

```sh
prefix="${XDG_DATA_HOME:-$HOME/.local/share}/trace"
export TRACE_HOME="${XDG_STATE_HOME:-$HOME/.local/state}/trace"
"$prefix/trace-service" cli self-test
"$prefix/trace-service" web
# 前台 Ctrl+C 停止；另一个终端验证：
curl --fail http://127.0.0.1:8765/api/auth/status
```

状态/工件/工作区统一位于 `TRACE_HOME`，工具和浏览器使用 XDG 缓存；安装目录与持久
状态分离。启动脚本把 `TRACE_TOOLS_HOME` 默认设置为 `$XDG_CACHE_HOME/trace/tools`，
可显式覆盖。首次使用浏览器前，Linux 动态库由管理员准备一次：

```sh
sudo "$prefix/current/bin/python" -m playwright install-deps chromium
"$prefix/current/bin/python" -m playwright install chromium
```

运行时下载不提升权限。系统库不齐全时先补上系统依赖，再重试工具 setup。

## 可选 systemd 用户服务

以下使用默认安装路径；自定义 XDG/安装目录时修改 unit 的 `ExecStart` 为实际绝对
路径。systemd 不继承交互 shell 的环境，持久配置写入 `trace.env`。

```sh
mkdir -p "$HOME/.config/systemd/user" "$HOME/.config/trace"
cp deploy/trace-web.service "$HOME/.config/systemd/user/"
cp deploy/trace.env.example "$HOME/.config/trace/trace.env"
chmod 600 "$HOME/.config/trace/trace.env"
${EDITOR:-vi} "$HOME/.config/trace/trace.env"
systemctl --user daemon-reload
systemctl --user enable --now trace-web
systemctl --user status trace-web
journalctl --user -u trace-web -n 30
systemctl --user stop trace-web
```

需要登出后继续运行时，由系统管理员执行 `loginctl enable-linger USER`。

## Linux 升级与回滚

停服务后对完整状态做一致性备份，再安装新 wheel。备份不会随安装升级被覆盖：

```sh
prefix="${XDG_DATA_HOME:-$HOME/.local/share}/trace"
state="${TRACE_HOME:-${XDG_STATE_HOME:-$HOME/.local/state}/trace}"
systemctl --user stop trace-web
backup="$state.backup-$(date -u +%Y%m%dT%H%M%SZ)"
cp -a "$state" "$backup"
sh deploy/install-linux.sh /absolute/path/trace_agent-NEW_VERSION-py3-none-any.whl
systemctl --user start trace-web
```

需要回滚时保留升级后的状态副本，并恢复刚才的备份。下面的 `backup` 必须指向该次
升级前的备份，`prefix`、`state` 与上文一致：

```sh
systemctl --user stop trace-web
test -x "$prefix/previous/bin/trace"
test -d "$backup"
mv "$state" "$state.before-rollback-$(date -u +%Y%m%dT%H%M%SZ)"
cp -a "$backup" "$state"
ln -s "$(readlink "$prefix/previous")" "$prefix/.rollback-link"
mv -Tf "$prefix/.rollback-link" "$prefix/current"
systemctl --user start trace-web
curl --fail http://127.0.0.1:8765/api/auth/status
```

CI 在 Ubuntu/Python 3.11–3.13 逐版本运行 wheel 安装、Web HTTP/静态资源、SIGTERM、
MCP 和 Linux 安装/升级 smoke；另外真实构建镜像，验证非 root、只读根文件系统、
Chromium、四个持久卷、容器健康和停止退出码。
