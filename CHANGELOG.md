# 更新说明 / CHANGELOG

本文件记录本 fork（shenping1200/m365-native）相对上游的本地增量改动。

---

## 2026-07-19 功能增量更新

本次更新包含三项新功能：**删除账号**、**多账号轮询**、**每账号请求次数统计**。
对应提交：`f9df1f7`（删除账号 + 轮询）、`523c482`（请求次数初版）、`db75611`（请求次数计数修复）。

### 1. 多账号轮询（负载分散，规避单账号限流）

- **改动文件**：`internal/web/server.go`
- **机制**：
  - `resolveAccount()` 改造为 round-robin：当请求**未指定账号**且**无会话绑定**时，依次轮流使用全部已登录账号，把请求量分散到多个账号，规避微软 ChatHub 的 per-account 限流。
  - 当请求带有 `sessionKey`（同一段多轮对话）时，账号锁定为该会话首轮选中的账号，保证上下文连贯（见下文「上下文说明」）。
- **提交**：`f9df1f7`

### 2. 删除账号功能

- **改动文件**：`web/index.html`、`internal/web/server.go`（底层 `Store.Delete` 已实现）
- **机制**：账号池页面每一行新增「删除」按钮，点击后调用 `POST /api/accounts/delete`（body: `{"id": "..."}`）按 id 删除账号。操作不可撤销。
- **提交**：`f9df1f7`

### 3. 每账号请求次数统计

- **改动文件**：`internal/web/server.go`、`web/index.html`
- **机制**：
  - `Server` 新增 `accountStats map[string]int64` 字段；`resolveAccount()` 在「指定账号」与「轮询」两条分支选中账号时均执行 `accountStats[id]++`（原子递增）。
  - `/api/accounts` 接口的每个账号对象新增 `requestCount` 字段（int64）。
  - 前端账号池表格在「更新时间」右侧新增「请求次数」列，显示该账号被使用的累计次数（蓝色数字）。
- **已知限制**：计数保存在**内存**中，服务或容器重启后归零（轻量设计，不写磁盘）。如需持久化可后续扩展。
- **提交**：`523c482`（初版，含字段）、`db75611`（修复：`accounts()` 接口此前漏把 `requestCount` 从 map 读出，导致界面始终显示 0；已修正）

---

## 上下文连贯性说明（重要）

关于「轮询会不会让 AI 回答上下文断片」：

- **同一段对话内账号被锁定**，不会在轮询中切换身份，因此一段连续会话的上下文始终落在同一个账号上。
- **OpenAI 兼容客户端**（走 `/v1/chat/completions`）每次请求都会把完整的 `messages` 历史拼进 prompt 发给 ChatHub，即使账号轮换，模型每轮也能看到全部历史，回答连贯。
- 因此正常使用时，轮询**不会导致**上下文不连贯或回答失准。

---

## 部署说明

- **后端改动**（`server.go`）：需重新构建镜像
  ```bash
  docker compose build --no-cache && docker compose up -d
  ```
- **前端改动**（`web/index.html`）：该目录为挂载卷，修改即生效，无需重建。
- **推送目标**：本 fork `shenping1200/m365-native` 的 `main` 分支（上游 `uefi2333/m365-native` 未改动）。

---

## 提交历史（本 fork 增量）

| 提交 | 说明 |
| --- | --- |
| `f9df1f7` | feat: account delete UI + multi-account round-robin |
| `523c482` | feat: per-account request count in WebUI |
| `db75611` | fix: wire requestCount from map in accounts API（两条 resolve 路径均计数） |
