# Copilot Proxy + New API deployment

把定制的 [New API](https://github.com/QuantumNous/new-api) 与
[copilot-api-proxy](https://github.com/messense/copilot-api-proxy) 放进一个可重建的部署仓库。
完整源码在 `src/`，不依赖原服务器、本地私有基础镜像或 Git submodule。

```text
客户端 → SSH 隧道 → New API（本机 3000）
                       ├─ default 分组 → Copilot 主账号
                       ├─ copilot-secondary 分组 → Copilot 第二账号
                       └─ 两组可共享独立配置的 DeepSeek channel
```

**仓库不包含任何可用凭据或用户数据。** 每次新部署都需要自行授权 GitHub、
初始化管理员，并在 New API 后台填入自己的上游 Key。

## 1. 准备环境

推荐 Linux amd64/arm64、Docker Engine + Compose v2 或更新版本、Git、Bash、OpenSSL。
首次源码构建建议至少 4 CPU / 8 GiB RAM / 20 GiB 可用磁盘；小机器设置
`BUILD_JOBS=1`。构建需要访问软件镜像、Go/Bun/Cargo 依赖源。
Windows/macOS 可使用 Docker Desktop，部署命令在 WSL/Linux shell 或 Bash 中运行。
这是服务端部署仓库，不会安装或修改任何客户端。

```bash
git clone https://github.com/BoringZheng/copilot-proxy.git
cd copilot-proxy
bash scripts/stack.sh init
bash scripts/stack.sh build
```

`init` 生成本机 `.env` 和两个独立随机 secret，重复执行不会轮换已有值。
`.env` 只放非敏感配置，例如监听端口、时区、构建并行数。
不要把 GitHub Token、DeepSeek Key 或管理员密码填进 Compose/Dockerfile。

## 2. 授权 Copilot 账号

```bash
bash scripts/stack.sh auth primary
```

按终端提示打开 GitHub 设备授权页面，确认登录的是预期账号。
授权凭据只保存在该实例的 Docker volume，不在仓库中，也不会进入镜像。
账号必须拥有相应 Copilot 权限；模型可用性受账号与上游政策限制。

只使用一个账号：

```bash
bash scripts/stack.sh up
```

使用两个独立账号：

```bash
bash scripts/stack.sh auth secondary
bash scripts/stack.sh up --secondary
```

两个反代各有独立 volume，第二账号未启用时无需授权。
反代端口不发布到宿主机；不要为它们增加公网端口，因为通用转发接口自身不做客户端鉴权。
Docker 网络内具有访问权的其他容器仍可调用反代，因此不要接入不可信容器。

## 3. 初始化 New API 和路由

默认仅监听 `127.0.0.1:3000`。在自己的电脑建立 SSH 隧道：

```bash
ssh -N -L 13000:127.0.0.1:3000 <你的SSH用户名>@<服务器地址>
```

打开 `http://127.0.0.1:13000`，完成 New API 初始化向导并设置强管理员密码。
这个入口可同时提供后台和 `http://127.0.0.1:13000/v1` API。
SSH 不由本仓库配置；如果要公开访问，请单独设置 HTTPS、访问控制、防火墙和备份策略。

在后台按 `config/channels.example.json` 创建 GPT channel：

| 用途 | Base URL（不要加 `/v1`） | 分组 |
| --- | --- | --- |
| 主账号 | `http://copilot-primary:9876` | `default` |
| 第二账号 | `http://copilot-secondary:9876` | `copilot-secondary` |
| 共享 DeepSeek | 你实际使用的 DeepSeek 上游地址 | `default,copilot-secondary` |

反代 channel 使用 OpenAI 兼容类型（type 1）；其中 Key 只是后台必填项的非敏感占位，
不是保护反代的密码。DeepSeek channel 必须在后台填入真正的上游 Key。
示例模型名不是账号权限保证；先检查实例 `/v1/models`，再按自己的上游调整模型映射。

GPT 示例包含 `gpt-6.1-terra` 和 `gpt-5.6-sol-fast`。Sol Fast 是独立模型 ID，
客户端应直接选择该模型；不需要设置 `service_tier=priority`。
Terra 支持 `low/medium/high/xhigh/max`，Sol Fast 还支持 `none`，以实际账号返回为准。
Codex 客户端使用本地模型目录时，也需要刷新目录才能显示新增模型。

内部计费由各部署自行设置。若沿用本实例的 6.1 Sol 零额度策略，可在 New API 的
阶梯表达式计费设置中，将这两个模型的 `billing_mode` 设置为 `tiered_expr`，
`billing_expr` 设置为 `tier("base", p * 0 + c * 0)`。先验证调用日志扣额为 0；
这只控制 New API 内部额度，不代表上游 Copilot 不计费。示例 channel 不会自动修改价格。

两个账号需要隔离时：

1. 建立 `copilot-secondary` 分组并设置倍率。
2. 第二账号用户及其 API Token 使用该分组（Token 留空则继承用户分组）。
3. 禁止第二分组用户切换到 `default` / `auto`，关闭跨组重试，避免借用主账号。
4. GPT 通道不要同时加入两个分组；只有 DeepSeek 通道共享。
5. 未授权或不使用的 channel 保持禁用；不要自动扩大用户额度。

仓库默认不自动创建用户、API Token、管理员密码、余额或已有机器的 Key。
这使得部署不依赖原数据库，也不会把原账号信息复制到新环境。

可选 Argon：已有移除内置搜索、图像预算的后端策略，但需要另建兼容通道。
使用 New API 的 Gemini/OpenAI 协议转换，将入站 Responses 转成反代支持的
Chat Completions；不要给 GPT 的透传通道直接添加 Argon。第二账号默认只用 GPT。

## 4. 状态、停机与换号

```bash
bash scripts/stack.sh status
docker compose logs --tail 50 new-api
docker compose --profile secondary logs --tail 50 copilot-secondary
```

日志可能包含运行标识或错误信息；分享前自行脱敏，不能提交进 Git。
停止服务但保留数据：

```bash
bash scripts/stack.sh down
```

**不要运行 `docker compose down -v`**，它会删除数据库和登录凭据。
更换第二账号必须安排维护窗口，并先备份它的 volume：

```bash
docker compose --profile secondary stop copilot-secondary
bash scripts/stack.sh auth secondary
docker compose --profile secondary up -d copilot-secondary
```

这会中断第二账号的活动请求，不影响主账号。换号后旧会话可能因历史条目或加密状态
属于原账号而返回 `input item does not belong to this connection`；不能保证跨账号续聊，
新建空白会话验证。登录失败时不要删除旧 volume，先保留凭据并检查网络。

## 5. 凭据、迁移和备份

- `secrets/session_secret`、`secrets/crypto_secret`：宿主机本地文件，目录 0700 / 文件 0600，
  通过 Compose secrets 挂载，只在 New API 进程启动时读取；不是云端 Secret Manager。
- `<project>_copilot-primary-home`、`<project>_copilot-secondary-home`：各账号的凭据和设备标识。
- `<project>_new-api-data`：SQLite 数据库（含用户、后台保存的 channel Key 和 Token）。
- 这三类都是敏感数据。备份应加密、限制访问，保存在仓库外；GitHub 私有仓库也不能当密钥库。

迁移需要一致性备份：安排停机、停止该 Compose 项目后备份三个 volume，以及本地 `.env`
和两个 secret 文件；在新机器恢复到相同项目名或明确映射的 volume，再启动。
不要直接套用原腾讯云的数据库路径或覆盖正在运行的数据库。
恢复已有数据库时必须同时恢复原 `crypto_secret`，不要重新生成后替换。

现有 systemd 部署不自动迁移。本仓库与原服务是独立部署；先使用新的项目名和端口，
例如 `COMPOSE_PROJECT_NAME=copilot-test NEW_API_PORT=23000`，验证后再安排切流。
不要在没有备份和明确维护窗口时停用旧服务。

## 验证与已知边界

```bash
python3 scripts/check_repository.py
python3 -m unittest discover -s tests -v
bash -n scripts/stack.sh
sh -n deploy/new-api-entrypoint.sh
docker compose --profile secondary config --quiet
```

CI 检查发布文件的高置信密钥模式、禁止路径、导入源码哈希、初始化幂等性和 Compose 配置。
模式扫描不能保证识别所有凭据，因此发布前仍需人工审核；不要强制添加被忽略的文件。
导入来源和改动概览见 `THIRD_PARTY.md` 与 `source-provenance.json`。

该快照保留了 32 MiB 请求限制、Responses ID 兼容修复和 SSE 空闲超时修复。
它不声称已经修复所有 Copilot 上游 408，也不保证上游对第三方客户端或模型的支持。
请遵守 GitHub/Copilot 及其他提供商的使用条款，不要把账号权限或隔离配置作为绕过上游政策的手段。

New API 保留原作者与项目标识，遵循 AGPL-3.0；反代遵循 MIT。
向他人提供修改版 New API 的网络服务时，应按许可证提供对应源码，详见 `THIRD_PARTY.md`。
