# MCP-Server 模块研究纪要

> 整理自 2026-09-13 的特性问答与方案讨论（含 SDD spec `mydocs/specs/2026-09-13_22-17_MCP网关多用户Key透传.md` 的研究结论）。
> 所 有 file:line 均以当时代码为准；`mcp-server/` = Python 网关，`internal/mcp/` = Go 侧 MCP 客户端，两者方向相反勿混淆。

---

## 1. 模块定位：协议翻译网关

`mcp-server/` 是一个**单文件、无状态**的 MCP 协议适配层，把 MCP「工具调用」翻译成 WeKnora REST API 调用：

```
Claude Desktop / Cursor 等AI客户端
   │  stdio（本地进程） 或 http/sse + Bearer（网络传输）
   ▼
mcp-server（Python 容器 WeKnora-mcp，宿主机 8082 → 容器 8000）
   │  X-API-Key: <WEKNORA_API_KEY>
   ▼
WeKnora Go 后端（http://app:8080/api/v1）
```

与另外两条 MCP 线的区别：
- **方向①（Go 后端 internal/mcp/）**：WeKnora 作为 MCP **客户端**，智能体接外部 MCP 服务器的工具（SSE/HTTP Streamable，stdio 因命令注入风险禁用）。带五层安全设计：SSRF 校验、密钥 AES-256-GCM 加密、OAuth 按人隔离（`mcp_oauth_tokens` 唯一键 tenant+user+service）、工具人工审批门（issue #1173）、提示注入缓解（工具输出加 untrusted 前缀）。
- **方向②（mcp-server/，本文）**：WeKnora 被暴露为 MCP **服务器**。
- **方向③（cli/cmd/mcp/）**：`weknora mcp serve` 把 CLI 暴露为 stdio MCP 服务器（精选 10 只读+会话工具）。

易混淆：`internal/modelcontext/` 与 MCP 协议**无关**，是模型上下文句柄注册表（UUID → dN/res://NNNN 临时句柄）。

## 2. 代码结构走读（weknora_mcp_server.py，1115 行）

| 层 | 位置 | 要点 |
|---|---|---|
| 配置与网络鉴权 | L30-125 | `WEKNORA_BASE_URL` / `WEKNORA_API_KEY` / `WEKNORA_CHAT_TIMEOUT`(300s)；`MCPAuthMiddleware` 用 `secrets.compare_digest` 校验共享令牌（Bearer 或 X-MCP-Auth-Token） |
| WeKnoraClient | L127-609 | `requests.Session` 薄封装。线程本地 Session（mcp 2.x 同步工具跑线程池，Session 非线程安全）；`resolve_kb_id`/`resolve_agent_id` 名字→UUID 解析（LLM 常传名字）；`_consume_sse_stream`（L453-520）把 SSE 流聚合成一次性结果（answer 累加/references/error 抛异常/complete 结束）——**流式到非流式的翻译** |
| 30 个 @mcp.tool() | L612-1012 | docstring 即工具描述（直接进 LLM 提示词，含使用指引）；`agent_chat` 有 KB 预检（kb_selection_mode=none 且未传 knowledge_base_ids 时主动失败并列出可用 KB）；chat/agent_chat 用 `run_in_executor` 把阻塞 SSE 读取扔线程池防卡事件循环 |
| 传输选择 | L1015-1115 | `--transport` 参数 > `MCP_TRANSPORT` env > 默认 stdio；容器默认 `--transport http`（Dockerfile:17）；sse/http 强制要求共享令牌否则拒启 |

配套文件：`upload_paths.py`（上传路径安全：stdio 不限 / 网络传输默认仅工作目录 / `MCP_ALLOWED_UPLOAD_DIRS` 白名单，realpath+commonpath 防穿越）；`main.py`（完整入口）/`run_server.py`（固定 stdio）/`run.py`（最简）。

## 3. 两把钥匙的区别（高频疑问）

| | `MCP_SERVER_AUTH_TOKEN` | `WEKNORA_API_KEY` |
|---|---|---|
| 保护谁 | mcp-server 自己（谁能用这台 MCP 服务器） | WeKnora 后端资源（能碰哪些库、做什么） |
| 谁校验 | Python 中间件（恒定时间比较） | Go 中间件查 `tenant_api_keys` 表 |
| 谁生成 | 自己 `openssl rand -hex 32`，系统不登记 | 后端生成 `sk-`+43位 base64url（tenant_api_key.go:208-213），库存 SHA-256 哈希+AES 加密，**明文只显示一次** |
| 何时必需 | 仅 sse/http（改造后移除，见 §7） | 始终必需 |

头的关系：`Authorization: Bearer` 与 `X-MCP-Auth-Token` 是共享令牌的两个等价载体；`X-API-Key` 是后端 API Key 的唯一载体。

## 4. 传输方式与工具面

- **SSE（老协议 2024-11，已废弃）**：`GET /sse` 长连接 + `POST /sse/messages/`，服务器需维护会话状态。仅老客户端退回用。
- **Streamable HTTP（MCP 2025-03-26）**：单端点 `/mcp`，`stateless_http=True` 每请求自包含，可水平扩展。**默认/推荐**。
- **stdio**：本地子进程，无网络暴露，无需共享令牌；stdout 是 JSON-RPC 通道（诊断走 stderr）。

30 个工具分六组：租户 2（create/list_tenants）、知识库 6（CRUD+`hybrid_search`）、文档 6（from_file/url/text + list/get/delete）、模型 3、会话+问答 4（`chat`/`agent_chat`/`list_agents`/`get_agent`）、分块 2、Wiki 只读 3。**没有任何 update 工具**（刻意收缩写面）。

`chat` vs `agent_chat`：
- `chat` → `POST /knowledge-chat/:id`：固定 RAG 流水线（改写→检索→带引用总结），不可跳步，检索不到走 fallback_response。
- `agent_chat` → `POST /agent-chat/:id`：ReAct 智能体循环，LLM 自主决定调工具（knowledge_search/web_search/SQL/MCP 工具…），必须传 agent_id，多步推理、更慢更贵。

## 5. API Key 权限模型与后端校验链路

Key 三层授权（`internal/types/tenant_api_key.go:18-38`）：
1. **能力位 Capabilities**：retrieve/chat/ingest/read_agents/manage_kbs/manage_agents/message_history/manage_models/manage_mcp_services/manage_datasources/manage_channels/manage_vector_stores/manage_storage_backends/manage_web_search/run_evaluations/manage_members/manage_spaces/manage_tenant_settings + 平台级 system_*
2. **知识库白名单 KnowledgeBaseIDs**：空=全空间；非空则逐请求硬校验
3. **FullAccess**：全权限开关

**每个请求的三层关卡**（逐请求执行，无缓存）：
```
① 身份层：X-API-Key → SHA-256 哈希 → 查 tenant_api_keys.key_hash（唯一索引）
          吊销(RevokedAt)/过期(ExpiresAt)/不存在 → 401（统一报 invalid，不泄露存在性）
          命中 → 异步更新 last_used_at（限频）
② 路由层：APIKeyRouteAuthorizer 按路由表查策略（能力 OR full_access）
          未声明路由对 API key 默认拒绝（default-deny）
③ 白名单层：KB 级路由校验 key 的允许列表（AllowsKnowledgeBase 等）
```

**用户身份模式**（`tenants.api_principal_config`，中间件 auth.go:600-662 resolveAPIPrincipal）：
- `tenant`（仅空间）：Principal = api_tenant:<tenantID>
- `direct_header`：`X-External-User-ID` 头 → api_external_user:<tenantID>:<uid>（低保证，仅可信服务间）
- `signed_token`：`X-External-User-Token` HS256 JWT（aud=weknora、exp≤24h、tenant 匹配）→ 同上

身份消费方：会话归属（SessionOwnerIDFromContext，session.go:352 按其过滤——**不同 key 的会话互相不可见**）、长期记忆 subject、MCP OAuth 令牌隔离。
**自带 mcp-server 现状不透传终端用户身份**：所有调用落在进程级单一身份上（改造见 §7）。

## 6. 能力位映射表（MCP 工具 → 后端路由 → 所需能力）

| 能力位（OR full_access） | MCP 工具 | 路由证据 |
|---|---|---|
| retrieve | hybrid_search、list/get_knowledge_base、list_knowledge_bases、list_knowledge、get_knowledge、list_chunks、wiki_search/read_page/index_view | routes_knowledge.go:198(kb组)/73(kbRead)/103(kRead)/33(chunkRead)/325(wikiRead) |
| chat | chat、agent_chat、create/get/list/delete_session | routes_chat.go:106/112/51 |
| read_agents ∨ chat ∨ manage_agents | list_agents、get_agent | routes_agent.go:26 |
| ingest | create_knowledge_from_file/url/text、delete_knowledge、delete_chunk | routes_knowledge.go:70-78/31-45 |
| manage_kbs | create/delete_knowledge_base | routes_knowledge.go:199-226 |
| manage_models（读也要求） | create/list/get_model | routes_infra.go:21 |
| manage_spaces | list_shared_knowledge_bases | routes_agent.go:209 |
| manage_tenant_settings | list_tenants | routes_auth_tenant.go:73 |
| platform[SystemTenantsManage] | create_tenant（租户 key 永不可用） | routes_auth_tenant.go:71 |

反直觉点：`list_models` 读也要 manage_models；`list_shared_knowledge_bases` 要 manage_spaces；`create_tenant` 平台专属。
**发 key 实操**：开发者 `retrieve + chat`（问答/检索/会话/列智能体全可用）；要上传加 `ingest`；`manage_*` 只给管理员。

## 7. 多用户共享部署改造（进行中，待批准执行）

**场景**：一个空间、管理者给小A/小B 各发一把 key，两人各自 Claude 接入**同一个** mcp-server。

**现状缺陷**（旧 spec `2026-09-02_18-48` 实证）：API Key 是进程级（env 读一次、写入 Session 默认头），MCP 协议无客户端凭证通道 → 共享部署下所有人折叠为同一身份：权限无差别、会话互通、记忆共享、审计不可区分。

**方案对比**：
- 方案一：每人本机 stdio 跑自己的 mcp-server 实例，各配各的 key（零改造，服务端零部署）
- **方案二（选定）**：改造共享网关透传每用户 key
- 方案三：共享一把 key（会话互通，不推荐）

**方案二设计要点**（详见 spec `2026-09-13_22-17_MCP网关多用户Key透传.md`）：
1. **入站双通道**：个人 key 走 `Authorization: Bearer <sk-...>`（兼容只有 token 字段的客户端）或 `X-WeKnora-Key` 自定义头
2. **`MCP_SERVER_AUTH_TOKEN` 移除**（用户确认无存量配置）：网关变纯透传验真，后端查库即校验；启动硬性要求改 warning
3. **ContextVar + 按请求头覆盖**：中间件把 key 存 `contextvars.ContextVar`；`WeKnoraClient` 出站 `headers={"X-API-Key": ...}` 按请求覆盖——**绝不改写线程本地 Session 默认头**（多请求复用，改写会串号）
4. **线程池传播**：同步工具经 anyio 自动传播 contextvars；`chat`/`agent_chat` 的 `run_in_executor` 不传播，需显式 `contextvars.copy_context().run` 包装
5. **严格模式**：`MCP_REQUIRE_USER_KEY=1` 拒绝无 key 裸连；默认无 key 回落 env（stdio/单用户兼容）
6. **SSE 老传输不承诺**（消息可能在独立任务处理，ContextVar 未必可达）；容器默认 http 传输无此问题
7. 出站覆盖三处：`_request()`、`_consume_sse_stream()`、`create_knowledge_from_file()`（复制头直发那个特例）

**key 透传后的端到端效果**：小A 的 key 白名单只有「案例设计知识库」→ 他 MCP 里 `list_knowledge_bases` 只见此库；调越权工具被路由层 403；会话/审计/记忆按 key 隔离；删 key 即时生效（下次 401）。

**部署配置卫生**：`MCP_SERVER_AUTH_TOKEN` 移除后，`.env` 里 `WEKNORA_API_KEY` 可留空或仅作回落；各人 key 只存各自客户端，不进服务器 env。

**后续增强（Out-of-Scope 备忘）**：用户身份模式透传（X-External-User-ID）→ 记忆/MCP OAuth 也按人隔离；SSE 传输的 ContextVar 传播验证。

## 8. 快速排障速查

| 症状 | 根因方向 |
|---|---|
| MCP 客户端连上网关 401 | 无 key 裸连且开了严格模式 / key 已吊销过期 /（改造前）Bearer ≠ 共享令牌 |
| 工具调用 403 "API key scope..." | key 能力位不足（对照 §6 映射表）或 KB 白名单不含目标库 |
| 空列表/看不到某知识库 | key 的 knowledge_base_ids 白名单过滤 |
| 会话里看到别人的记录 | 用了共享 key（同 key 同归属桶）——改造后按人发 key 解决 |
| `no search targets available` | agent 的 kb_selection_mode=none 且未传 knowledge_base_ids（mcp-server 已有预检会提前报清晰错误） |
| stdio 客户端报协议错误 | 有 print 污染了 stdout（诊断必须走 stderr） |
