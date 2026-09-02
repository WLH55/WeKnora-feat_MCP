# WeKnora MCP API Key 权限与空间角色详解

> 本文档整理自对 WeKnora 代码的逐项核实,覆盖空间角色体系、MCP API Key 权限模型、各能力的实际语义,以及两者之间的关系。
> 所有结论均标注了对应代码位置,便于查证。

---

## 目录

1. [空间角色体系](#一空间角色体系)
2. [MCP API Key 权限模型](#二mcp-api-key-权限模型)
3. [访问类型(总开关)](#三访问类型总开关)
4. [各能力详解](#四各能力详解)
5. [知识库范围(数据边界)](#五知识库范围数据边界)
6. [角色与 API Key 的关系](#六角色与-api-key-的关系)
7. [安全建议](#七安全建议)
8. [代码索引](#八代码索引)

---

## 一、空间角色体系

### 1.1 角色定义

空间内成员共四级角色,权限按数值递进包含:

| 角色 | 代码值 | 前端显示 | 等级 |
|---|---|---|---|
| 所有者 | `owner` | 所有者 | 40 |
| 管理者 | `admin` | 管理者 | 30 |
| 编辑 | `contributor` | 编辑 | 20 |
| 访客 | `viewer` | 访客 | 10 |

> 注意:空间内第四级角色的代码值是 `contributor`,前端中文显示为"编辑"。
> 真正名为 `editor` 的角色只存在于**组织跨空间共享**体系(`OrgMemberRole`: admin / editor / viewer),用于跨空间共享知识库/智能体时标记对方的访问级别,与空间内角色正交。

定义位置:`internal/types/tenant_member.go:16-33`(TenantRole 枚举)、`internal/types/tenant_member.go:38-61`(角色等级与 HasPermission)、`internal/types/organization.go:10-19`(OrgMemberRole)。

### 1.2 角色权限对照表

权限比较核心是 `tenantRoleLevel` 数值映射 + `HasPermission`("不小于"比较),高等级自动包含低等级的所有权限。

| 能力 | 所有者 | 管理者 | 编辑 | 访客 |
|---|:---:|:---:|:---:|:---:|
| 查看空间内容(只读) | ✅ | ✅ | ✅ | ✅ |
| 创建会话、发起对话 | ✅ | ✅ | ✅ | ✅ |
| 创建知识库/智能体 | ✅ | ✅ | ✅ | ❌ |
| 编辑**自己创建**的资源(KB/智能体/文档) | ✅ | ✅ | ✅ | ❌ |
| 编辑**别人创建**的资源(KB/文档/分块) | ✅ | ✅ | ❌ | ❌ |
| 管理模型/向量库/IM/MCP服务/数据源等基础设施 | ✅ | ✅ | ❌ | ❌ |
| 管理成员、邀请、改他人角色 | ✅ | ❌ | ❌ | ❌ |
| 管理 API Key | ✅ | ❌ | ❌ | ❌ |
| 修改空间配置、删除空间 | ✅ | ❌ | ❌ | ❌ |

**实测确认(重要修正):管理者(admin)不能管理成员。**

成员相关的**所有变更操作**在路由层挂的都是 `g.Owner()`(所有者),不是 `g.Admin()`:
- 查看成员列表:`g.Viewer()`(任何成员可看)
- 添加成员 / 修改成员角色 / 移除成员:`g.Owner()`
- 创建/撤销邀请、创建邀请链接:`g.Owner()`

位置:`internal/router/routes_auth_tenant.go:113-136`。

**而删除知识库/文档,admin 拥有与 owner 相同的权力。** 删除类操作使用 `g.OwnedKBOrAdmin()` / `g.OwnedKnowledgeKBOrAdmin()` 守卫,即"创建者 **或 Admin+**"均可操作:
- 删除知识库:`DELETE /knowledge-bases/:id` → `OwnedKBOrAdmin`
- 删除文档:`DELETE /knowledge/:id` → `OwnedKnowledgeKBOrAdmin`
- 删除文档全部分块:`DELETE /chunks/:knowledge_id` → `OwnedChunkKBOrAdmin`
- 编辑/重新解析文档、删除分块、改 FAQ 同此模式

位置:`internal/router/routes_knowledge.go:106-111`。

**实测确认(修正二):访客(viewer)可以创建会话、发起对话。**

会话与对话类路由的角色门槛是 `g.Viewer()`(任何成员可及),并非 Contributor+:
- 创建/管理自己的会话:`POST /sessions` 等 → `g.Viewer()`(`internal/router/routes_chat.go:51`)
- 发起知识库问答、智能体对话:`/knowledge-chat`、`/agent-chat` → `g.Viewer()`(`internal/router/routes_chat.go:91,97`)

**但"创建知识库"和"创建智能体"这两项访客不行**,门槛是 `g.Contributor()`(编辑及以上):
- 创建知识库:`POST /knowledge-bases`(`internal/router/routes_knowledge.go:202`)
- 创建智能体:`POST /agents`(`internal/router/routes_agent.go:36`)

> 准确的说法是:访客对**知识库内容**是纯只读,但**聊天会话是访客可用的功能**——能建会话、能对话、能管理自己的会话(改名/置顶/删除)。另一条只读红线:下载文档原文需 Contributor+(`internal/router/routes_knowledge.go:118`),访客只能在线预览。

### 1.3 角色总结

- **管理者 vs 所有者**:admin 的权力集中在"资源"上——能删改空间内任何人创建的知识库和内容、管理基础设施;但"组织权力"——成员、角色、邀请、API Key、空间配置、删除空间——全部是 owner 专属。**admin 是"资源管理员",owner 才是"空间主人"。**
- **编辑 vs 管理者**:contributor 只能编辑**自己创建**的资源,管理类操作全部没有;admin 可以编辑任何人创建的资源并管基础设施。
- **访客**:能创建会话、发起对话并管理自己的会话(会话路由门槛是 Viewer+),但创建知识库/智能体需要编辑及以上;对知识库内容纯只读(下载原文也不行,只能在线预览)。
- **多个所有者**:允许存在多个 owner,唯一限制是**必须保留至少一个活跃 owner**——把最后一个 owner 降级或移除会被拒绝(`ErrLastOwner` 保护,`internal/application/service/tenant_member.go:63-68`)。

---

## 二、MCP API Key 权限模型

### 2.1 核心结论

**API Key 的权限与空间角色完全独立,不继承任何角色。** 代码明确注释:"API keys do not reuse tenant-member roles: a key is either full-access, or it carries explicit capabilities."

机制要点:

1. **认证时合成角色仅为兼容**:API Key 认证时会临时合成一个角色塞进上下文——普通 key 合成 `viewer`,开了"空间完全访问"的 key 合成 `owner`。但这**只为兼容旧代码判断,不产生实际授权**。所有 `RequireRole` / `RequireOwnershipOrRole` 守卫对 API Key 直接放行,授权完全交给 `APIKeyGate`。
   - 位置:`internal/middleware/auth.go:520-591`(注入 scope)、`internal/middleware/rbac.go:74-77`(对 API Key 短路)。

2. **实际权限由三样东西决定**:
   - **能力列表(Capabilities)**:18 个空间级 + 7 个平台级独立授权位,与角色表无关。
   - **知识库范围(KB 白名单)**:限定钥匙能碰哪些知识库。
   - **空间完全访问(FullAccess)**:跳过能力检查,直接等价于空间内全权;但仍不等于 owner 角色(碰不到平台级、不能被授予 owner 等)。

3. **路由检查 fail-closed**:每个 API 路由声明它允许哪些能力,没声明的路由一律拒绝。

### 2.2 数据模型

- 表:`tenant_api_keys`(迁移 `000065_tenant_api_keys.up.sql`、`000071_platform_api_keys.up.sql`)
- 模型:`internal/types/tenant_api_key.go:18-38`
- 关键字段:
  - `ScopeType`:`tenant`(空间级) / `platform`(平台级,要求 tenant_id 为 NULL 且 full_access=FALSE)
  - `FullAccess bool`(空间完全访问)
  - `KnowledgeBaseIDs StringArray`(JSONB,知识库范围 allow-list)
  - `Capabilities StringArray`(JSONB,能力授权列表)
  - `KeyHash`(SHA-256)、`APIKey`(AES 加密存储)、`ExpiresAt`、`RevokedAt`
- 创建校验(`internal/application/service/tenant_api_key.go:34-84`):platform key 禁止 FullAccess、必须至少一个 capability;tenant key 若 FullAccess 则清空 KnowledgeBaseIDs 和 Capabilities。

### 2.3 能力列表

能力定义:`internal/types/tenant_api_key.go:73-165`;前端分组:`frontend/src/config/apiKeyCapabilities.ts`。

| 能力常量 | 值 | 前端文案 | 分组 |
|---|---|---|---|
| `APIKeyCapabilityRetrieve` | `retrieve` | 检索知识库 | 知识库数据 |
| `APIKeyCapabilityChat` | `chat` | 对话能力 | 知识库数据 |
| `APIKeyCapabilityIngest` | `ingest` | 写入知识库内容 | 知识库数据 |
| `APIKeyCapabilityManageKnowledgeBases` | `manage_kbs` | 管理知识库 | 知识库数据 |
| `APIKeyCapabilityMessageHistory` | `message_history` | 消息历史 | 知识库数据 |
| `APIKeyCapabilityReadAgents` | `read_agents` | 读取智能体 | 智能体与集成 |
| `APIKeyCapabilityManageAgents` | `manage_agents` | 管理智能体 | 智能体与集成 |
| `APIKeyCapabilityManageMCPServices` | `manage_mcp_services` | 管理 MCP 服务 | 智能体与集成 |
| `APIKeyCapabilityManageDataSources` | `manage_datasources` | 管理数据源 | 智能体与集成 |
| `APIKeyCapabilityManageMembers` | `manage_members` | 管理成员 | 成员与空间 |
| `APIKeyCapabilityManageSpaces` | `manage_spaces` | 管理空间 | 成员与空间 |
| `APIKeyCapabilityManageTenantSettings` | `manage_tenant_settings` | 管理空间设置 | 空间配置 |
| `APIKeyCapabilityManageModels` | `manage_models` | 管理模型 | 空间配置 |
| `APIKeyCapabilityManageChannels` | `manage_channels` | 管理渠道 | 空间配置 |
| `APIKeyCapabilityManageVectorStores` | `manage_vector_stores` | 管理检索基础设施 | 空间配置 |
| `APIKeyCapabilityManageStorageBackends` | `manage_storage_backends` | 管理存储后端 | 空间配置 |
| `APIKeyCapabilityManageWebSearch` | `manage_web_search` | 管理联网搜索 | 空间配置 |
| `APIKeyCapabilityRunEvaluations` | `run_evaluations` | 运行评测 | 空间配置 |

平台级另有 `system_tenants_read` 等 7 个能力,用于平台级 API Key。

---

## 三、访问类型(总开关)

创建 API Key 时,"能力授权"与"空间完全访问"二选一(前端 `ApiIntegrationSettings.vue:485-502`):

- **能力授权**:钥匙只在勾选的能力范围内操作,是默认、最安全的模式。
- **空间完全访问**:在能力授权基础上,额外开放模型、数据源等全部空间级接口。等于"给能力,同时把所有空间配置级接口一并放开"。只建议极信任的自用钥匙选择。

> 一句话:能力授权决定"范围",空间完全访问决定"是否连空间级配置一起放开"。

---

## 四、各能力详解

### 4.1 检索知识库(`retrieve`)

- 允许读取、查询、检索所选知识库范围里的数据,是**只读**的——不创建会话、不修改内容。
- 这是 MCP 只读访问的核心,也是最常用的能力。
- 对应路由:KB 读取类(列表/详情/搜索)、chunk 读取、wiki 读取、知识检索(`/knowledge-search`)等。

### 4.2 对话能力(`chat`)

允许发起对话并管理**自己这把钥匙名下**的会话。**不修改知识库内容**。

**会话归属机制**:

每个会话(session)有一列 `user_id` 作为归属标识,代码对调用方做了区分(`internal/types/principal.go:178-193` `SessionOwnerIDFromContext`):

| 调用方 | 会话 user_id 存什么 | 举例 |
|---|---|---|
| 普通 Web 用户 | 用户的 UUID | `3f2a...-ab12` |
| **API Key 调用** | `api_tenant_key:<空间ID>:<钥匙ID>` | `api_tenant_key:5:12` |
| IM / 嵌入访客 | 各自的前缀标识 | `im_user:...`、`embed_session:...` |

**常见疑问解答**:

- **能读到空间其他成员的对话吗?** 不能。会话列表/读取的过滤规则是 `(user_id = ? OR user_id IS NULL OR user_id = '')`,`?` 是当前身份(`internal/application/repository/session.go:20-26`)。API Key 的身份是 `api_tenant_key:5:12`,与成员的用户 UUID 不匹配,看不到。唯一例外是**遗留数据**(user_id 为空的行),任何钥匙都能看到——这是历史兼容设计。
- **同一把 key 的多次调用是同一个对话吗?** 不是。每次创建的 session 都是独立一行,有自己的 session_id 和消息记录。但同一把 key 能看到**自己名下创建的所有会话**(它们 user_id 相同),所以同钥匙的多客户端共享一个"会话池"。要续接同一段对话,必须显式传入同一个 session_id。
- **多把 key 呢?** 完全隔离。Key A(`api_tenant_key:5:12`)和 Key B(`api_tenant_key:5:34`)的 user_id 互不匹配,互相列不出、读不到。
- **空间成员(含管理员)默认也看不到 API Key 的对话。** user_id 以 `api_tenant_key:` 开头的会话属于"渠道流量",普通用户打开会 404;只有 **Admin+** 在控制台通过 `source=api` 过滤可查看全部 API Key 会话——这对应"消息历史"能力背后的管理视图(`internal/types/session.go:137-170`)。

**隔离建议**:若要让不同客户端/业务之间数据隔离,应**每客户端一把 key**,而不是共用一把。

对应路由:`internal/router/routes_chat.go`(`/sessions`、`/knowledge-chat`、`/agent-chat`)、`routes_agent.go` 的部分。

### 4.3 写入知识库内容(`ingest`)

- 允许向授权范围内的知识库写入内容:上传文档(`/knowledge-bases/:id/knowledge/file`)、编辑分块、FAQ、标签、Wiki。
- **不能**新建知识库、不能新建智能体、不能清空知识库。
- 仅限选中的知识库范围。
- 清空知识库内容(`DELETE /knowledge-bases/:id/knowledge`)只允许 **full-access** key(`internal/router/routes_knowledge.go:81`)。

### 4.4 管理知识库(`manage_kbs`)

允许知识库全生命周期:新建、复制、修改、删除,以及调整初始化/配置信息。

**复制知识库是什么语义?** 代码里有两种操作,容易混淆:

- **`POST /knowledge-bases/copy`(复制)**:**异步整库克隆**。把整个知识库完整复制成一份新的——配置、文档内容、分块、索引全部搬过去。服务端校验源/目标知识库必须在钥匙的知识库范围(allow-list)内,且嵌入模型、向量库、存储后端必须一致才能克隆;副本归调用者所有。
  - 位置:`internal/router/routes_knowledge.go:226-239`、`internal/application/service/knowledgebase.go:1109-1198`
- **`POST /:id/duplicate`(创建副本)**:只复制**设置**,不复制内容/索引/分享,相当于建一个"空壳配置副本"(`internal/application/service/knowledgebase.go:1200-1233`)。

> 所以"复制"是**整库克隆**,不是复制某个文档。

**范围限制**:
- 已勾选知识库上的操作(复制/更新/删除)仍受所选范围(allow-list)限制。
- **新建不受限**——因为新知识库还不属于任何范围;限定 allow-list 的 key 建出的新 KB 落在其 allow-list 之外(同租户、无越权,只是建完自己管不到)。
- 空 allow-list 的 key 则是全租户 KB 管理,新建天然在范围内。

### 4.5 消息历史(`message_history`)

允许查询空间聊天历史、读取聊天历史统计;不授予空间配置权限。定位是审计/回溯。

**能否查整个空间所有成员和 AI 的聊天记录?** 能。消息搜索的仓库层过滤条件只有 `sessions.tenant_id = ?`——**只按空间过滤,不按用户过滤**(`internal/application/repository/message.go:169`)。路由注释也写明这是 "tenant-wide and not attributable to a KB"(空间全局、不归属某个知识库)。

具体能力:
- `POST /messages/search`:关键字/向量/混合搜索**整空间所有会话**的消息——包括空间成员网页对话、其他 API Key 对话、IM 渠道对话。
- `GET /messages/chat-history-stats`:聊天历史知识库统计。

限制:
- 是**只读**的,不能删改(只有 `chat` 能力能在自己会话内删消息)。
- 向量搜索走知识库,仍受检索能力约束。
- 路由注册:`internal/router/routes_chat.go:16-33`(`apiKeyMessageHistory`)。

> ⚠️ **安全提示**:给 MCP Key 勾 `message_history` 等于让这把钥匙读空间里所有人的聊天内容,是敏感能力,默认不勾是合理的。

### 4.6 读取智能体(`read_agents`)与 管理智能体(`manage_agents`)

- **读取智能体**:列出智能体、查看详情、读取预设和建议问题。只读,不发起对话、不修改智能体。
- **管理智能体**:创建、修改、删除、复制智能体。因为智能体可能挂模型/MCP 等敏感绑定,默认关闭。

### 4.7 管理 MCP 服务(`manage_mcp_services`)

允许管理 MCP 服务、凭据(credential)、工具审批策略,以及该主体的 OAuth 授权状态。权限很大,是"用这把钥匙再去管别的 MCP"。

### 4.8 管理数据源(`manage_datasources`)

**数据源功能在哪里?** 这组接口挂在 `/api/v1/datasource` 下(`internal/router/routes_infra.go:242-290`):

| 操作 | 路由 | 角色门槛 |
|---|---|---|
| 获取连接器类型 | `GET /datasource/types` | Viewer+ |
| 校验凭据(测试连接) | `POST /datasource/validate-credentials` | Admin+ |
| 创建/更新/删除数据源 | `POST/PUT/DELETE /datasource(/:id)` | Admin+ |
| 凭据管理 | `PUT /datasource/:id/credentials` 等 | Admin+ |
| 连接测试、资源选择 | `POST /datasource/:id/validate` 等 | Admin+ |
| 手动同步/暂停/恢复 | `POST /datasource/:id/sync` 等 | Admin+ |
| 同步日志(只读) | `GET /datasource/:id/logs` | Viewer+ |

连接的外部系统包括飞书(Feishu)、Notion、语雀(Yuque)等。**管理的是"知识库的外部文档来源"**——把这些系统连进来并同步文档进知识库。绑定知识库时仍受知识库范围限制。

在界面没看到的原因:数据源前端管理页在"空间设置 → 集成/数据源"这类二级菜单,与 API Key 配置页不在同一处;且这个能力主要给程序(如 MCP 客户端)调 API 用。

### 4.9 管理成员(`manage_members`)

允许查看和管理空间成员、角色、邀请和邀请链接。

**注意**:代码明确排除——此能力**不包含** API Key 管理、空间删除或所有权转移(`internal/types/tenant_api_key.go:143-152`),防止钥匙自我提权。且 API Key 走成员管理接口时,**不能被授予 owner 角色**(`ErrAPIKeyCannotAssignOwner`,`internal/application/service/tenant_member.go:127-135`)。

### 4.10 管理空间(`manage_spaces`)

对应 `/organizations` 组织空间协作接口(`internal/router/routes_agent.go:83-205`):
- 创建/加入/退出组织、邀请成员、审批加入申请、改成员角色、查看组织的共享知识库/智能体列表。
- 共享空间**可见性**可被此能力管理(查看共享列表)。

**关键边界——分享管理不受此能力授予**:主动发起/取消分享(`/knowledge-bases/:id/shares`、`/agents/:id/shares`)代码注释明确:"分享管理不通过 capability 授予(manage_spaces 也不含);仅 full-access key 可管理分享,scoped key 保持 default-deny。"(`internal/router/routes_agent.go:167-195`)

所以勾了"管理空间"的钥匙能看共享列表、管组织成员,但**不能把知识库/智能体分享给别人**。

### 4.11 管理空间设置(`manage_tenant_settings`)

对应两类东西:

**a) API 集成设置页(创建 API Key 的页面里的其他配置)**
- **用户身份模式(principal mode)**:配置 API 请求如何识别"终端用户"。两种模式:
  - 直接信任请求头里的用户 ID(仅建议可信服务端到服务端调用;任何持有 key 的调用方可冒充他人)。
  - 后端签发的 JWT/HMAC 签名 Token(安全,面向终端用户;JWT 须含 sub、tenant_id、aud=weknora、exp≤24h)。
  - 该身份用于区分不同用户的对话 Session 与 MCP 工具授权。
- **请求头配置**:用户 ID 请求头、Token 请求头的名字;是否强制每个请求必须带用户 ID 头。
- 前端文案:`frontend/src/i18n/locales/zh-CN.ts:450-482`。

**b) 空间级 KV 配置**(`GET/PUT /tenants/kv/:key`,`internal/handler/tenant.go:1246-1314`),共 6 个键:
- `web-search-config`(联网搜索配置)、`prompt-templates`(提示词模板)、`parser-engine-config`(解析引擎)、`storage-engine-config`(存储引擎)、`chat-history-config`(聊天历史库)、`retrieval-config`(检索配置)

边界:
- 有该能力的 key 能读/更新这些空间级配置,但**管不了** API Key 本身的管理、成员管理、空间删除。
- 其中联网搜索、解析引擎、存储引擎三个配置含敏感凭据,查看额外要求 Admin+ 或此能力(`CanViewIntegrationSecrets`,`internal/handler/dto/role.go:14-42`)。

### 4.12 其他空间配置能力(运维级)

| 能力 | 含义 |
|---|---|
| 管理模型(`manage_models`) | 模型配置、模型凭据、连通性测试、WeKnoraCloud 凭据 |
| 管理检索基础设施(`manage_vector_stores`) | 向量库配置,以及解析器/文档读取器/存储引擎连通性检查 |
| 管理存储后端(`manage_storage_backends`) | 对象/文件存储后端实例(S3 兼容或本地)增删改查、连通性测试、默认存储设置 |
| 管理联网搜索(`manage_web_search`) | 联网搜索供应商配置、凭据、连接测试 |
| 管理渠道(`manage_channels`) | 智能体嵌入渠道、IM 渠道、微信扫码绑定流程 |
| 运行评测(`run_evaluations`) | 运行评测任务、读取评测结果 |

---

## 五、知识库范围(数据边界)

创建 API Key 时选择的知识库范围(allow-list):

- **留空**:允许访问**全部**知识库,最宽范围。
- **选择具体知识库**:把上面所有"读/写"操作都限定在这些知识库内。这是数据隔离的关键——即使勾了"写入知识库内容",也只能写选中的这些。

实现:服务层 `AuthorizeTenantAPIKeyKnowledgeBases` / `AllowsKnowledgeBase`(`internal/types/tenant_api_key.go:320-444`)在 KB 中间件(`kb_access.go:244`)和各 service 层强制生效。

---

## 六、角色与 API Key 的关系

两套体系**互不继承、各管各的**,但有三个交叉点:

1. **谁能创建/管理 API Key**:只有空间的 **owner** 能进创建 API Key 的页面和接口(`g.Owner()`,`internal/router/routes_auth_tenant.go:97-102`)。角色决定"谁能发钥匙",钥匙决定"钥匙能做什么"。
2. **API Key 不能被授予 owner 角色**:用 API Key 调用成员管理接口,想给自己或别人分配 owner 会被直接拒绝(`ErrAPIKeyCannotAssignOwner`),防提权设计。
3. **平台级 API Key**(scope=platform)可触达系统管理员的平台级路由(`RequireSystemAdmin` 对 platform scope 放行),这是另一层,跟空间内角色无关。

> **一句话总结**:空间角色是**给人用**的阶梯体系(owner > admin > contributor > viewer,权限递进包含);MCP API Key 是**给机器用**的扁平能力体系(18 个能力位 + 知识库白名单 + 全访问开关),两者互不继承。唯一连接点:只有 owner 能发钥匙,且钥匙永远拿不到 owner 身份。

---

## 七、安全建议

配置 MCP API Key 时:

1. **最小权限原则**:只勾需要的能力。MCP 场景下通常 `检索知识库` + `对话能力` 就够,其余保持关闭。
2. **必选知识库范围,勿留空**:明确指定能访问哪些知识库,避免一把钥匙访问全部数据。
3. **谨慎勾选高权限能力**:
   - `管理知识库`、`管理智能体`、`管理 MCP 服务`、`管理数据源` 默认关闭,确认必要再开。
   - `消息历史` 等于能读整空间所有人的聊天内容,极其敏感。
   - `管理成员`、`管理空间设置` 接近运维级,一般不需要。
4. **空间完全访问需慎重**:会把空间配置级接口全部放开,只用于自己的运维/管理用途。
5. **会话隔离**:不同客户端/业务用不同 key;同一把 key 的调用方共享会话池。
6. **密钥安全**:Key 仅创建时明文展示一次(数据库里是 AES 加密存储,认证用 SHA-256 哈希比对)。

---

## 八、代码索引

| 内容 | 路径:行号 |
|---|---|
| TenantRole 枚举 / 等级 / HasPermission | `internal/types/tenant_member.go:16-61` |
| OrgMemberRole 枚举 | `internal/types/organization.go:10-19` |
| tenant_members 迁移(含唯一索引) | `migrations/versioned/000043_tenant_rbac.up.sql:26-46` |
| 成员管理路由(Owner 专属) | `internal/router/routes_auth_tenant.go:113-136` |
| API Key 管理路由(Owner 专属) | `internal/router/routes_auth_tenant.go:97-102` |
| RBAC 角色守卫 | `internal/middleware/rbac.go:67,122,147,212` |
| 角色守卫封装 g.Owner() 等 | `internal/router/rbac.go:195-213` |
| 最后 owner 保护 | `internal/application/service/tenant_member.go:63-68`、`internal/application/repository/tenant_member.go:199-269` |
| API Key 不能分配 owner | `internal/application/service/tenant_member.go:127-135` |
| TenantAPIKey 模型 + 能力常量 | `internal/types/tenant_api_key.go:18-38,73-165` |
| Scope 判断 HasCapability/AllowsKnowledgeBase | `internal/types/tenant_api_key.go:307-355` |
| tenant_api_keys 迁移 | `migrations/versioned/000065_tenant_api_keys.up.sql`、`000071_platform_api_keys.up.sql` |
| API Key 创建/认证服务 | `internal/application/service/tenant_api_key.go:34-103` |
| APIKeyGate 中间件 | `internal/middleware/api_key_gate.go:111-153` |
| API Key 路由策略构造器 | `internal/router/rbac.go:226-333` |
| 会话归属(api_tenant_key 前缀) | `internal/types/principal.go:24-40,178-193` |
| 会话/对话路由(访客可用,Viewer+) | `internal/router/routes_chat.go:51,91-103` |
| 创建知识库/智能体路由(编辑及以上) | `internal/router/routes_knowledge.go:202`、`internal/router/routes_agent.go:36` |
| 会话 user_id 过滤 | `internal/application/repository/session.go:20-26` |
| 渠道会话管理视图 | `internal/types/session.go:137-170` |
| 知识库复制(整库克隆) | `internal/router/routes_knowledge.go:226-239`、`internal/application/service/knowledgebase.go:1109-1198` |
| 知识库创建副本(仅设置) | `internal/application/service/knowledgebase.go:1200-1233` |
| 消息历史(整空间搜索) | `internal/router/routes_chat.go:16-33`、`internal/application/repository/message.go:157-174` |
| 数据源路由 | `internal/router/routes_infra.go:242-290` |
| 组织/共享路由(管理空间) | `internal/router/routes_agent.go:83-205` |
| 空间 KV 配置 | `internal/handler/tenant.go:1246-1314`、`internal/router/routes_auth_tenant.go:73-81` |
| 集成密钥可见性判断 | `internal/handler/dto/role.go:14-42` |
| 前端能力配置分组 | `frontend/src/config/apiKeyCapabilities.ts` |
| 前端能力文案 | `frontend/src/i18n/locales/zh-CN.ts:385-482` |
| 前端 API Key 创建 UI | `frontend/src/views/integrations/ApiIntegrationSettings.vue:485-534,1308-1312` |
