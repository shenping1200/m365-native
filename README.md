# m365-native

M365 ChatHub gateway for **authorized Microsoft 365 Copilot sessions**. It exposes OpenAI-compatible and Anthropic-compatible HTTP APIs for chat, streaming, multimodal input, tool calls, session continuity, and upstream image-event parsing.

> This project is an interoperability gateway, not an authentication bypass. You must use a Microsoft account and tenant you are authorized to use. Upstream model availability, quotas, tools, vision, and image generation depend on the account and Microsoft service.

## 近期更新（2026-07-26 · 本 fork 增量）

> 以下为 `shenping1200/m365-native` 相对上游的本地增量改动，便于在主页快速查看。

### 2026-07-19
- **多账号轮询**：`resolveAccount()` 改为 round-robin。请求未指定账号且无会话绑定时，依次轮流使用全部已登录账号，把请求量分散到多个账号，规避微软 ChatHub 的 per-account 限流。同一段对话（同一 `session_key`）内账号锁定为首次选中的账号，保证上下文连贯。
- **删除账号**：账号池页面每一行新增「删除」按钮，调用 `POST /api/accounts/delete`（body `{"id":"..."}`）按 id 删除账号，操作不可撤销。
- **每账号请求次数统计**：`Server` 新增 `accountStats map[string]int64`，`resolveAccount()` 在「指定账号」与「轮询」两条分支均计数；`/api/accounts` 每个账号返回 `requestCount`；前端账号池表格新增「请求次数」列（蓝色数字）。计数在内存中，服务/容器重启后归零。
- **上下文连贯性说明**：轮询不会导致 AI 回答断片。同一段对话内账号被锁定；且 OpenAI 兼容客户端（`/v1/chat/completions`）每轮都会把完整 `messages` 历史拼进 prompt 发送，即使账号轮换模型也能看到全部历史。

相关提交：`f9df1f7`（删除账号 + 轮询）、`523c482`（请求次数初版）、`db75611`（请求次数计数修复）。

### 2026-07-26
- **并发安全修复（P0 #3）**：`/api/accounts` 读取每账号请求计数 `accountStats` 改为在 `Server` 锁内读取（新增 `accountRequestCount` 方法），消除与 `resolveAccount` 写计数之间的并发 map 读写，避免进程触发 `fatal error: concurrent map read and map write` 后崩溃。
- **删除账号清理会话绑定（P0 #4）**：`deleteAccount` 在删除令牌的同时调用 `sessions.deleteByAccount(id)`，一并清除该账号绑定的所有 `session_key` 映射。修复「删除账号后带该 `session_key` 的会话永久返回 400」以及「删账号后废掉多账号轮询故障转移」两个问题。
- **API Key 有效期**：创建密钥时支持「永久有效」或「自定义天数」（创建框新增单选项，`POST /api/admin/keys` 传 `days`，`days≤0` 为永久）；密钥列表新增「有效期」列（显示「永久有效」/ 过期时间 /「已过期」），并提供「改有效期」按钮（调用 `PATCH /api/admin/keys`，body `{"id":"...","days":N}`，`days≤0` 表示改回永久）。过期的密钥在调用任何 `/v1/*` 接口时返回 `401`。
- **说明**：以上改动全部只在本 fork 维护，不向上游合并；上游设置不理想，所有更新/拉取均走本 fork。

相关提交：本 fork 最新提交（2026-07-26 增量：并发安全 + 删账号清会话 + API Key 有效期）。

## Reference repositories

- **M365-Copilot2API:** <https://github.com/HEXUXIU/M365-Copilot2API>
- **Microsoft 365 Copilot:** <https://www.microsoft.com/microsoft-365/copilot>

## Features

- OpenAI-compatible `/v1/chat/completions`
- OpenAI Responses-compatible `/v1/responses`
- Anthropic-compatible `/v1/messages`
- Streaming responses and multimodal input
- Gateway-level tool/function calling with protocol conversion
- Persistent conversation mapping through `session_key`
- Model catalog with GPT-5.5, GPT-5.5 reasoning, GPT-5.6 reasoning, and Claude Sonnet routes when available upstream
- Upstream image-event/GraphicArt parsing when enabled for the account
- Web console for account, API-key, settings, conversations, and debug management

## Requirements

- Go 1.22+ for source builds, or Docker/Compose
- An authorized Microsoft account and tenant
- OAuth access obtained through the bundled PKCE flow or an existing account cache

## Quick start: source build

```bash
git clone https://github.com/shenping1200/m365-native.git
cd m365-native
cp .env.example .env
# Edit .env. Never commit real passwords or tokens.
set -a; . ./.env; set +a
go test ./...
go vet ./...
go run ./cmd/server
```

The default bind address is `127.0.0.1:4141`. Open <http://127.0.0.1:4141/> and complete administrator setup/login. Keep the service on localhost unless you add TLS and an access-control layer.

Build a standalone binary:

```bash
go build -trimpath -o m365-native ./cmd/server
./m365-native
```

## Docker deployment (recommended)

Docker is the recommended deployment method for a reproducible runtime. The image runs as a non-root user and stores mutable credentials/state under `/data`.

### 1. Prepare directories and admin secret

```bash
mkdir -p data secrets
printf '%s\n' 'replace-with-a-long-random-admin-password' > secrets/m365_admin_password
chmod 600 secrets/m365_admin_password
```

Do not commit `data/` or `secrets/`. The provided `.gitignore` excludes them.

### 2. Build and start

```bash
docker compose build
docker compose up -d

docker compose ps
docker compose logs -f m365-native
```

The default Compose mapping is local-only:

```text
127.0.0.1:4141 -> container:4141
```

For a reverse proxy or LAN deployment, change the `ports` mapping deliberately and put TLS/authentication in front of it.

### 3. Persistent data

The Compose file mounts:

```text
./data/accounts.json       OAuth account cache
./data/token-cache.json    token cache
./data/sessions.json       session_key mapping
./data/api-keys.json       API-key hashes
./secrets/m365_admin_password administrator password secret
```

Back up these files securely. `accounts.json` and token caches are credentials. Never paste them into issues, logs, screenshots, or public repositories.

### 4. First login and API key

Open:

```text
http://127.0.0.1:4141/
```

Log in to the web console, complete the Microsoft authorization flow, and create an API key from the administration interface. Use that key with `/v1`:

```bash
curl http://127.0.0.1:4141/v1/models \
  -H 'Authorization: Bearer YOUR_M365_NATIVE_API_KEY'
```

The gateway accepts either `Authorization: Bearer ...` or `X-API-Key: ...`.

## API examples

OpenAI Chat Completions:

```bash
curl http://127.0.0.1:4141/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_M365_NATIVE_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "gpt-5.6-reasoning",
    "messages": [{"role": "user", "content": "Hello"}],
    "stream": true,
    "session_key": "my-conversation-1"
  }'
```

Keep `session_key` stable for every turn of the same conversation. Use a different key for a different conversation. The gateway stores the corresponding upstream `ConversationID` and `SessionID` in the session cache.

Anthropic-compatible endpoint:

```bash
curl http://127.0.0.1:4141/v1/messages \
  -H 'x-api-key: YOUR_M365_NATIVE_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "claude-sonnet",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

## Model routing

The public model IDs are gateway aliases. The current stable catalog is:

| Public model | Upstream tone |
|---|---|
| `gpt-5.5` | `Gpt_5_5_Chat` |
| `gpt-5.5-reasoning` | `Gpt_5_5_Reasoning` |
| `gpt-5.6-reasoning` | `Gpt_5_6_Reasoning` |
| `claude-sonnet` | `Claude_Sonnet` |
| `claude-sonnet-reasoning` | `Claude_Sonnet_Reasoning` |

Availability and latency remain controlled by Microsoft 365 ChatHub and the account entitlement.

## Configuration

Common environment variables:

| Variable | Default | Purpose |
|---|---|---|
| `M365_LISTEN` | `127.0.0.1:4141` | HTTP bind address |
| `M365_CONFIG` | `~/.config/m365-native/accounts.json` | OAuth account cache |
| `M365_ADMIN_PASSWORD` | bootstrap default only | Admin password; prefer a secret file |
| `M365_ADMIN_PASSWORD_FILE` | unset | File containing admin password |
| `M365_TOKEN_CACHE` | platform default | Token cache path |
| `M365_SESSION_CACHE` | temp directory | Persistent `session_key` mapping |
| `M365_API_KEYS` | `~/.config/m365-native/api-keys.json` | API-key hash store |
| `M365_CHAT_TIMEOUT_SECONDS` | `120` | Chat timeout |
| `M365_IMAGE_TIMEOUT_SECONDS` | `150` | Image request timeout |
| `M365_MAX_TOOL_ROUNDS` | `16` | Maximum tool rounds |
| `M365_MAX_TOOL_CALLS_PER_TURN` | `1` | Tool-call limit per turn |
| `M365_CONTEXT_WINDOW` | `128000` | Advertised context window |
| `M365_MAX_OUTPUT_TOKENS` | `16384` | Advertised output limit |

## Development and verification

```bash
gofmt -w cmd internal
go test ./...
go vet ./...
go build ./...
```

## Security notes

- Bind to localhost by default.
- Change the administrator password immediately.
- Keep OAuth caches, token files, API-key files, and Docker secrets private.
- Use TLS and an additional access-control layer before exposing the service outside localhost.
- Do not log or publish access tokens, cookies, authorization headers, or raw authenticated WebSocket URLs.
- This gateway only supports accounts and services you are authorized to access.

## License

MIT. See [LICENSE](LICENSE).
