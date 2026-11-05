# 影策（open-ai-canvas）全仓源码审查报告

审查对象：`antcloudsman/open-ai-canvas` 工作区，对齐上游 `ddcat-ai/open-ai-canvas`。  
本文只记录审查当时已读源码能够证明的事实。读不到的实现写「未在源码中证实」。README、AGENTS.md 与专题文档只作为「文档声明」，不代替实现证据。

## 1. 审查基线

| 项 | 值 |
| --- | --- |
| 审查日期 | 2026-11-04 |
| 审查时分支 | `main` |
| 审查时 HEAD | `cc229fefa8585c1f9bb1622b7c041022731d503d` |
| HEAD 说明 | `feat(web): 界面多语言 - 引入 i18n 并支持中英法德日韩六种语言（第一期）`（作者 `ddcat666`，提交时间 `2026-11-04 21:19:46 +0800`） |
| 版本文件 | `VERSION` 全文为 `v1.7.3.5`；`CHANGELOG.md` 首节标题为 `## v1.7.3.5` |
| 语言规模（文件系统计数，非语义覆盖率） | `backend` 下 `*.go` 1335 个；`web/src` 下 `*.ts`/`*.tsx` 1248 个；`plugin-packages` 一级目录 111 个，其中 `*.yingce-plugin` 109 个 |

### 1.1 审查方法

只读。未改应用逻辑、依赖或配置默认值。未运行测试、构建或浏览器。结论来自对下列入口的直接阅读，以及围绕这些入口的符号定位（`Grep` / 结构体与函数体阅读）：

- 根：`VERSION`、`CHANGELOG.md` 首节、`README.md` 文档链接、`docs/index.md`、`AGENTS.md` 仅作对照、`.github/workflows/quality.yml` 头部、`.github/pull_request_template.md`、`nginx.conf` 代理段、`docker-compose.deploy.yml`、`Dockerfile` 未逐行展开（Compose 与 nginx 已读）。
- 后端入口：`backend/cmd/server/main.go` `main`/`run`、`backend/cmd/migrate-schema/main.go` 命令分派、`backend/go.mod` 模块与直接依赖、`backend/internal/handler/api.go` `RegisterCanvasAPI`、`handler/response.go`、`handler/routes.go` `RegisterTaskRoutes`、`handler/agent.go` `RegisterAgentRoutes` 开头、`kernel/error_codes.go`、`service/doc.go`、`app/service.go` `New`/`newService`/`StartWorker`、`app/task_creation.go` 计费分支、`app/task_execution.go` `processTask`、`app/task_worker.go` `taskWorkerCoordinator.start`、`app/provider.go` `processCanvasGenerationTask`、`database/migrations.go` 版本常量与迁移表头、`database/schema.go` `Models`、`platform/coordination.go` `NewCoordinator`/`Allow`、`auth/auth.go` 注册与 `CurrentUser`、`outbound` 仅通过环境变量名在 Compose 中出现，函数体未在本次逐行展开。
- 短剧与算法：`app/drama_service.go` `CreateDrama` 开头、`app/drama_orchestrator.go` 调度循环与 `plan`/`advanceEpisode`/`enqueueSegmentVideos`、`app/drama_segment_video.go` `prepareDramaSegment` 与尾帧函数名、`production/storyboard.go` 规则常量、`app/workflow_provider_values.go` `workflowPromptCandidateScore`。
- Agent：`app/cloud_agent_pi_runtime.go` `runCloudAgentPi`、`yingce-agent/server.mjs` 头部。
- 前端：`web/src/router.tsx` 路由表、`web/src/services/api/request.ts`、`web/src/services/api/task-center.ts` `createGenerationTask`、`web/src/lib/canvas/canvas-project-generation.ts` `runBackendCanvasGenerationTask` 签名、`web/src/types/canvas.ts` 视口与节点枚举、`web/src/stores/use-config-store.ts` 持久化、`web/src/i18n/index.ts`、`web/src/i18n/locales.ts`、`web/src/i18n/messages/zh-CN.ts` 前段、`web/src/components/layout/workspace-wallet-modal.tsx` `WorkspaceWalletHost`、`web/src/constant/navigation-tools.ts`。
- 模型抽样：`model/models_task.go` `Task`、`model/models_project.go` `CanvasProject`/`CanvasShare`、`model/models_canvas_history.go`、`model/models_logical_model.go` `LogicalModelPriceSKU`。

未逐文件审阅 `backend/internal/app` 全部实现、全部 handler、全部 plugin manifest、全部页面组件。下文「未在源码中证实」表示本次阅读范围没有对应实现证据，不等于仓库一定不存在。

## 2. 架构总览

影策是前后端分仓、由同一仓库发布的 AI 影视/短剧工作台。浏览器只通过统一 JSON 信封访问后端；生成不在浏览器直连模型供应商完成主路径，而是创建服务端任务，由 worker 按模式分派到文本/图片/视频/音频或声明式插件协议。短剧制作是后端调度器，按数据库状态推进，再把每一步提交成同一套任务。云端画布 Agent 是另一条控制面：浏览器只订阅事件，执行由 Pi 运行时（内嵌 Node 或独立 `yingce-agent`）经 bridge 回调后端。

```
浏览器 (web/, Vite/React)
  |  Cookie 会话 + /api JSON 信封 (http)
  |  画布文档另有浏览器 localforage 降级副本（见第 7 节，服务端权威副本是 canvas_projects.payload_json）
  v
Gin (backend/cmd/server)  /api/*
  |  handler -> service 别名 -> app.Service
  |            |-> auth / canvas / skills / platform / production 等域包
  |            |-> repository -> GORM -> SQLite 或 PostgreSQL
  |            |-> platform.Coordinator -> Redis（Postgres 必填；SQLite 可进程内）
  v
后台循环 (StartWorker)
  |-- taskWorker：租约领取 Task，processTask -> processCanvasGenerationTask
  |-- drama scheduler：tickDrama -> plan/dispatch -> CreateTask
  |-- 资源删除/转存、技能同步、支付、计费退款、Agent runner 恢复
  v
上游模型
  |-- 声明式 plugin-packages（yingce.plugin）由 protocol registry 装载
  |-- 遗留 provider_text/image/video/audio 与 workflow provider
  v
对象：dataDir/resources 与用户 OSS（Resource.Provider/ObjectKey；OSS 实现细节未逐文件展开）

Agent：
  runCloudAgentPi
    |-- 未设置 YINGCE_AGENT_URL：本进程 exec node agent-runtime.mjs
    |-- 已设置：HTTP POST 到 yingce-agent /v1/runs（生产 Compose 走此路）
  Pi 通过 bridge 调模型与工具；工具里的生成再进 CreateTask
```

### 2.1 目录职责（以仓库实际目录 + 已读入口为准）

| 路径 | 已证实职责 |
| --- | --- |
| `web/src/pages`、`router.tsx` | 路由页面。路由表见第 7 节 |
| `web/src/components`、`lib`、`stores`、`services` | 画布 UI、纯逻辑、Zustand、API。画布生成入口函数见第 4 节 |
| `web/src/i18n` | i18next 实例与六种语言包。文案自身声明第一期未覆盖全部页面 |
| `backend/cmd/server` | 进程入口：数据目录、数据库、迁移、种子、Gin、worker、优雅退出 |
| `backend/cmd/migrate-schema` | 独立迁移 CLI：`up` / `status` / `verify` / 两项 repair。无 `down` |
| `backend/internal/handler` | HTTP 注册与 `{code,data,msg,reason}` 写出 |
| `backend/internal/service` | `doc.go` 写明：只再导出 `internal/app` 的类型、常量与包级函数 |
| `backend/internal/app` | 组合根与尚未拆出的业务。`newService` 组装 repo、coordinator、插件运行时、计费、任务 worker、skills、auth、canvas、workbench、platform |
| `backend/internal/{auth,canvas,skills,platform,production,protocol,payment,storage,outbound,repository,model,database,kernel}` | 域包与基础设施。本次读到 auth 会话、production 分镜规则、platform 协调、database 模型清单、kernel 错误码 |
| `backend/internal/agent`、`backend/agent-runtime`、`yingce-agent` | Agent 进程与远程 HTTP 包装。远程入口文件已读；`agent-runtime.mjs` 正文未展开 |
| `plugin-packages/` | 109 个 `*.yingce-plugin` 与 111 个一级目录。单个 manifest 字段未逐包阅读 |
| `backend/payment-plugins`、`backend/payment-sdk` | 目录存在。支付 RPC 实现未逐文件阅读；前端充值面板已接到 `WorkspaceWalletHost` |
| `docs/content/docs` | 22 个 mdx。索引指向的部分文件不存在，见第 9 节 |
| `docker-compose.deploy.yml` 等 | 生产拓扑已读。其余 compose 文件未逐行对照，不在本文展开差异表 |
| `.github/workflows/quality.yml` | CI 名 `Quality checks`，含 Agent 静态检查、后端分片测试、Web 检查（文件后半未逐 job 展开） |

`AGENTS.md` 约定的调用链 `HTTP -> handler -> service（别名）-> app -> 域包 -> repository` 与已读代码一致：`service/doc.go`、`handler` 使用 `service.Service`、`app.New` 由 `service.New` 转调（`app/service.go` `New`）。handler 生产代码是否完全不 import `repository`：本次未做全仓 import 审计，不把「零违规」写成已证实。

## 3. 功能 / 模块清单

状态含义：已实现 = 读到可执行入口；部分实现 = 入口存在但覆盖、契约或文档不完整；仅文档或桩 = 只有声明、空路由或未接入迁移的类型。

### 3.1 仓与工程

| 项 | 状态 | 证据 |
| --- | --- | --- |
| Go 模块 `yingce/backend`，Go 1.25 | 已实现 | `backend/go.mod` |
| Gin 1.12、GORM、pgx、sqlite、go-redis | 已实现（依赖声明） | `backend/go.mod` `require` |
| 前端 React 路由与 API 客户端 | 已实现 | `web/src/router.tsx`、`request.ts` |
| 版本号 `v1.7.3.5` 与 CHANGELOG 标题一致 | 已实现 | `VERSION`、`CHANGELOG.md` 第 2 行 |
| CHANGELOG 对 Schema 的升级说明 | 部分实现 | 同版本写「Schema 从 78 升至 79」；代码 `CurrentSchemaVersion = 81`（`database/migrations.go`） |
| 文档索引指向的 Docker / 快速开始 / 画布手册 | 仅文档链接，文件不存在 | `docs/index.md` 第 9 行；`README.md` 第 12、246、251 行。`docs/content/docs` 下无对应 mdx |

### 3.2 前端与画布

| 项 | 状态 | 证据 |
| --- | --- | --- |
| 登录 / 注册 / 忘记密码 | 已实现（路由） | `router.tsx` `/login` `/register` `/forgot-password` |
| 创作、短剧项目、画布列表与画布编辑、素材、技能、插件、任务、设置、管理后台 | 已实现（路由挂载） | `router.tsx` 126–278 行。页面组件内部行为未逐页阅读 |
| 画布节点类型 | 已实现（类型） | `types/canvas.ts` `CanvasNodeType`：image、text、drawing、script、skill、config、video、audio、frame、markdown、svg、html、panorama、compare、chart、colorgrade、media-conversion、batch-table；另有插件字符串 `CanvasNodeTypeId` |
| 视口 | 已实现（类型） | `ViewportTransform { x, y, k }` |
| 生成提交到后端任务 | 已实现 | `runBackendCanvasGenerationTask`；`createGenerationTask` → `POST /tasks` |
| 分享画布 | 已实现（路由 + 模型） | `/share/canvas/:token`；`CanvasShare.TokenHash` |
| 积分中心 | 部分实现 | 导航 slug `wallet` 存在；路由 `/wallet` 的 element 为 `null`。真实 UI 是布局弹层 `WorkspaceWalletHost`（充值 / 兑换 / 记录 tab） |
| i18n 六语 | 部分实现 | `SUPPORTED_LOCALES` 六种；`zh-CN.ts` `language.pending`：「部分页面仍在翻译中」 |
| DEV 实验室路由 | 已实现且仅 DEV | `router.tsx` `devRoutes`，`import.meta.env.DEV` 才展开 |

### 3.3 后端 API、数据、Agent、AIGC

| 项 | 状态 | 证据 |
| --- | --- | --- |
| `/api` 聚合注册 | 已实现 | `RegisterCanvasAPI`：认证、外观、Agent、管理、财务、支付、渠道、任务、工作台、电商、技能、用户数据、画布实时、分片上传、插件、短剧（功能开关）、分享等 |
| 健康检查 | 已实现（注册调用） | `main.go` `registerSystemStatusRoutes`。具体路径未在本次打开 `system_status.go` |
| Cookie 会话 | 已实现 | `SessionCookieName = "open_ai_canvas_session"`；`CurrentUser` 校验过期与 `TokenHash`。未在源码中证实 JWT 登录态 |
| 统一错误信封 | 已实现 | `ok` / `fail` / `failService`；`CodeOK = 0`，`CodeRateLimited = 42901` 等 |
| 任务创建与 worker | 已实现 | `POST /tasks` → `CreateTask`；`StartWorker` → `taskWorkerCoordinator.start` → `ClaimNextTask` |
| 文本/图/视频/音频分派 | 已实现 | `processCanvasGenerationTask` 的 `switch input.Mode` |
| 短剧无人值守调度 | 已实现 | `startDramaScheduler`、`tickDrama`、`dramaRun.plan`、`advanceEpisode`、`submitDramaTask` → `CreateTask` |
| 段视频尾帧衔接 | 已实现 | `prepareDramaSegment`：非并行时把上一段 `TailFrameResourceID` 作为 `first_frame`；`extractDramaTailFrame` / `saveDramaTailFrame` |
| 云端 Agent | 已实现 | `RegisterAgentRoutes` 注释：「浏览器流不驱动执行」；`runCloudAgentPi` 内嵌或远程 |
| 独立 yingce-agent | 已实现 | `server.mjs`：`/healthz` 端口默认 8081、Bearer、`MAX_CONCURRENT_SESSIONS` 默认 30 且上限 64、token 短于 32 字符则退出 |
| 协议插件包 | 已实现（产物在库） | 109 个 `*.yingce-plugin`。装载函数 `newPluginRuntime` 在 `newService` 被调用；manifest 解析器正文未读 |
| 前台模型 SKU 类型 `LogicalModelPriceSKU` | 仅类型，未接入 | 全仓该符号只出现在 `models_logical_model.go`；不在 `Models()` |
| 电商 Agent、工作台、支付、推荐 | 部分证实 | 路由与 `StartWorker` 中的 worker 已挂上；业务规则未逐函数阅读，不把文档中的能力清单当成已实现范围 |

## 4. 核心业务流程

### 4.1 进程启动

`backend/cmd/server/main.go` `run`：

1. `CANVAS_BACKEND_DATA_DIR` 默认 `data`，`MkdirAll`。
2. `database.Open`：驱动默认 `sqlite`，DSN 来自 `DATABASE_URL`。
3. `CANVAS_AUTO_MIGRATE` 默认 true：真则 `MigrateSchema`，否则 `RequireSchemaVersion`。生产 Compose 把该变量设为 `"false"`，迁移交给 `migrate` 服务（`docker-compose.deploy.yml`）。
4. `repository.New` → `service.New`。若 `CANVAS_UPDATER_TOKEN` 非空，配置 updater socket，默认 `/run/open-ai-canvas-updater/updater.sock`。
5. `ValidateRuntime` 失败则进程退出。`newService` 把 `platform.NewCoordinator` 的错误放进 `runtimeErr`；Postgres 且无 `REDIS_URL` 时 `NewCoordinator` 返回错误「PostgreSQL 多实例模式必须配置 REDIS_URL」。
6. 启动前种子：系统渠道模型、默认用户组、提示词模板、电商知识、行业分类、内置工作流模板、内置技能与技能包、内置工具、工作台目录。`MigrateLegacyStorage` 失败只打警告，不中止启动。
7. Gin：访问日志、Recovery、关联 ID、CORS；`/api` 上再挂 `StorageReadOnlyGuard`。`NoRoute` 交给 `SystemProxyNoRouteHandler`。
8. 监听 `CANVAS_BACKEND_ADDR` 默认 `:8080`。`ReadHeaderTimeout` 10s。
9. `StartWorker`，并异步 `BackfillPlaybackTranscodes`、`BackfillVideoResourceDimensions`。
10. 关闭：HTTP `Shutdown` 30s，再 `StopWorker`，超时默认 `CANVAS_SHUTDOWN_TIMEOUT` 10 分钟。

`StartWorker` 拉起的循环（`app/service.go`）：任务 worker、资源删除、资源转存、技能同步、电商技能同步、支付、计费自动退款、短剧调度、一次 `syncAgentSessionLimit`、每 3 秒 `recoverCloudAgentPiRunners`。

### 4.2 登录与资源归属

- 公开设置：用户数为 0 时 `PublicAuthSettings` 强制 `RegistrationEnabled: true`、`FirstUser: true`（`auth.go`）。
- `Register`：用户数大于 0 才检查 `RegistrationEnabled()`，关闭时返回 `kernel.Forbidden("管理员未开放新用户注册")`。
- `CurrentUser`：Cookie 解析出 sessionID 与 token；查 `AuthSession`；过期或 `TokenHash` 不匹配则删会话并 401；用户非 active 则 403。Cookie 值格式与写入选项未在本次重读 `setSessionCookie`，不描述 Secure/SameSite。
- 管理接口 `RequireAdmin` 存在于 `app/admin_users.go`。本次未逐路由核对每个管理 handler 是否都调用它。
- 短剧 HTTP 组额外 `RequireFeature(..., FeatureShortDrama)`（`api.go`）。

### 4.3 打开工作台到触发生成

1. 前端路由：`/`、`/create` 为创作页；`/canvas` 列表；`/canvas/:id` 为 `CanvasProjectPage`，外层 `RequireAuth`。
2. 画布文档在服务端是 `CanvasProject.PayloadJSON`（`type:text`）加 `Revision`。历史副本是 `CanvasSnapshot.PayloadJSON`。浏览器侧另有本地缓存；同步实现文件 `user-data-sync.ts` 本次未逐行阅读，因此不描述冲突合并算法。
3. 节点生成：`runBackendCanvasGenerationTask` 收集 projectId、nodeId、mode、prompt、config、参考图/视频/音频。`createGenerationTask` 调用 `http.post("/tasks", input)`，成功后发 `wallet:updated`。
4. `POST /tasks`（`handler/routes.go`）：`currentUser`；按 `tasks:{userID}` 与 `policy.Request.TaskCreatePerMinute` 做一分钟窗口限流；body 上限 `16<<20`；`svc.CreateTask`。
5. `CreateTask`：普通任务走 `taskBillingOrder`。若 Agent admission 的报价 `AmountMicrocredits > MaxCharge`，返回「模型调用报价超过本轮 Agent 积分上限，尚未创建任务或扣费」。Agent 根任务在 `admission.NonBillable` 时不进计费、状态设为 `TaskStatusTextReplay`，注释写明不进 worker 队列，由 Pi 为每次模型调用另建可计费任务。
6. Worker：`runtimeConcurrencySetting` 的 `WorkerConcurrency` 与 Redis/本地租约 `AcquireLease(..., "workers", ...)` 限制全局并发；`ClaimNextTask(owner, 45s)` 领取。注释写明每次领取使用独立 owner，避免同进程旧执行者覆盖新执行者。
7. `processTask`：解密输入后，仅 `canvas_text|image|video|audio` 与 `video_` 前缀进入 `processCanvasGenerationTask`；其他类型返回「任务类型没有可用的执行分支」。
8. `processCanvasGenerationTask`：解析 JSON；视频模式不用提示词模板覆盖输入框；解析工具提及、渠道配置；工作流接口走 `runWorkflowProviderTask`；否则按 mode 调用 `runImageTask` / `runTextTask` 或 `runAgentToolTask` / `runVideoTask` / `runAudioTask`。文本在 `canvas_text` 且能力允许时挂 SSE 增量发布器。Gemini 非已安装插件协议时直接拒绝。缺少 BaseURL、APIKey 或模型名则失败。

任务成功后的素材落盘、播放地址回填与节点写回：CHANGELOG 描述了相关修复，但本次未阅读 `canvas-generation-consumer` 全文，不把落盘步骤写成已逐行证实。前端预览函数未展开。

### 4.4 短剧流水线

`CreateDrama`（`drama_service.go`）：作品名 1–80 字；章节数 1–3000；预算不能为负。注释：一次性写入项目、章节与制作控制面；`start=true` 时进入自动制作。单章字数上限等后续校验未读完，不转述。

调度（`drama_orchestrator.go`）：

- 周期 `dramaTickInterval = 3s`，每轮最多 `dramaProjectsPerTick = 20` 个运行中项目。
- 项目租约键 `drama:{ProjectID}`，TTL `dramaLeaseTTL = 2m`，并发 1。
- 暂停项目仍 `syncJobs`，注释说明否则段会停在生成中、下一段拿不到首帧。
- 运行中：同步任务 → `plan` → `CreateDramaJobs` → `dispatch`。
- `plan` 幂等。阶段为 `DramaStageBible` 时只入队「作品圣经」。未完成分集规划时：按章模式调用 `groupRemainingChapters`；否则按游标批处理，每批最多 `dramaPlanBatchChapters = 12` 且字数不超过 `dramaPlanBatchRunes = 30000`。
- 角色参考图：`ImageStatus` 为空且资产已放行，或状态为 queued，且未锁定，则入队 `DramaJobCastImage`。
- 集窗口 `dramaEpisodeWindow = 6`。`advanceEpisode` 顺序：剧本 `DramaJobScript`（可 `applyDirectStoryboard`）→ 段提示词就绪后进入审阅 → 审阅/抽样/手动放行通过后进入视频 → 段视频齐后 `DramaEpisodeRender` → `DramaJobRender` 合成。`allDone` 时项目状态 `DramaStatusCompleted`。
- 尾帧续接：`enqueueSegmentVideos` 在非 `parallelVideo()` 时按段序只提交下一段。`prepareDramaSegment` 在并行模式下把 `previous` 置 nil；否则 `Operation = "image_to_video"`，上一段尾帧未就绪则 `FirstFramePending`，就绪则引用 role `first_frame`。
- 预算暂停原因常量：`dramaBudgetPauseReason = "已达到预算上限，提高预算后可继续"`，`dramaCreditsReason = "积分不足，充值后点击继续制作"`。触发条件函数体未在本次读完。
- 成片：`buildDramaTimeline` 失败则不提交；成功则 `createDramaRenderTask`。时间线算法正文未读。

短剧任务最终仍进入 `CreateTask`（`submitDramaTask`，约 `drama_orchestrator.go` 1704 行一带；测试可通过 `dramaTaskHook` 替换）。

### 4.5 云端 Agent

- HTTP：`GET /agent/capabilities` 要求登录，返回权限模式 `read_only` / `request_approval` / `auto`、工具名与能力集版本。同文件还注册会话、消息、事件、审批、取消、撤销、插话；除 capabilities 外的 handler 正文未逐个阅读，不列举其校验细节。
- 执行分支 `runCloudAgentPi`：`YINGCE_AGENT_URL` 非空则 `yingceagent.Run`，否则 `agentruntime.Run`。生产 Compose 设置 `YINGCE_AGENT_URL: http://yingce-agent:8081`，并传 `YINGCE_AGENT_TOKEN` 与 `YINGCE_AGENT_BRIDGE_HOST: backend`。
- `server.mjs` 用 `timingSafeEqual` 比较 `Authorization` 与 `Bearer {token}`。未读排队与 `/v1/runs` 后半段，不描述拒绝码。

### 4.6 关键断点

| 断点 | 已读行为 |
| --- | --- |
| 未登录 | `CurrentUser` 返回 `Unauthorized`；`failService` 按其 `AppError` 写状态。`POST /tasks` 的登录失败走 `failService`，创建失败不走，见风险 R1 |
| 功能关闭 | 短剧路由组有 `RequireFeature`。其他功能开关中间件是否覆盖全部前台路由：未逐路由证实 |
| Postgres 无 Redis | `NewCoordinator` 返回错误，经 `ValidateRuntime` 阻止启动 |
| SQLite 无 Redis | `NewCoordinator` 返回无 Redis 的协调器，限流走进程内 `localRate`。多副本一致性未在源码中证实 |
| 迁移版本不符且关闭自动迁移 | `RequireSchemaVersion` 失败则 `run` 返回 |
| 短剧租约未拿到 | `tickDramaProductions` 跳过该项目 |
| 生成模式非法 | `processCanvasGenerationTask` 返回「不支持的生成模式」 |
| 任务类型无分支 | `processTask` 返回错误，不降级为成功 |
| Agent 远程进程 | token 不足 32 字符时 `server.mjs` `process.exit(1)`，Compose 中 backend 依赖其 healthcheck |

## 5. 数据模型与持久化

### 5.1 迁移

- 当前版本常量 `CurrentSchemaVersion int64 = 81`（`database/migrations.go`）。
- 记录表 `schema_migrations`，字段 `Version` 主键、`Name`、`Checksum`、`AppliedAt`。
- `schemaMigrations` 从 v1 `baseline_gorm_schema` 起按版本追加；v1 调用 `migrateSchemaV1`，其中 `AutoMigrate(Models()...)`。
- Postgres 迁移锁常量 `postgresSchemaMigrationLockID`。锁实现函数体未读。
- CLI 无向下迁移。`default` 分支列出的命令只有 `up`、`status`、`verify`、`repair-asset-bytes`、`repair-canvas-resources`。
- `Models()` 注释：「应用持久化表的唯一清单」。清单含用户与会话、渠道与逻辑模型、积分与支付、插件状态、方舟人像、技能、资源、素材、项目与分镜工作流、画布与快照、任务与创作运行、短剧全套表、电商项目与 Agent 表、云 Agent 执行与记忆表。表名未在模型上逐个 `TableName()` 核对；除 `schemaMigration.TableName()` 外，未在源码中证实自定义表名。GORM 默认蛇形复数。

### 5.2 已读实体与关系

下列不是全表字段手册，只列本次打开的结构体。

| 实体 | 主键与关系 | 存储要点 |
| --- | --- | --- |
| `Task` | `ID`；`UserID`、`ProjectID`、`Status` 等复合索引；`LeaseOwner`/`LeaseExpiresAt` 为领取租约 | `InputJSON`、`ResultJSON`、`TextDraft`、`MediaRecoveryJSON` 均为 `type:text`。计费订单、逻辑模型、渠道路由 ID 为索引字段，不是本次读到的数据库外键 |
| `CanvasProject` | `ID` size 80；`UserID`；可选 `ProjectID` | 整份画布 `PayloadJSON text`；`Revision` 默认 1 |
| `CanvasSnapshot` | `(CanvasID, Revision)` 唯一 | 结构 JSON；`PayloadBytes`、节点数、连线数 |
| `CanvasSnapshotResource` | `(SnapshotID, ResourceID)` | GORM 外键 `ResourceID`，`OnDelete:RESTRICT`。这是本次读到的少数数据库级约束 |
| `CanvasShare` | `TokenHash` 唯一；`(UserID, ProjectID)` 唯一 | `TokenCipher` 不进 JSON |
| `LogicalModelPriceSKU` | 结构体自带唯一索引标签 | 未进入 `Models()`，全仓无其他引用。未在源码中证实该表会被创建 |

短剧表 `DramaProduction`、`DramaCast`、`DramaEpisode`、`DramaShot`、`DramaTake`、`DramaSegment`、`DramaJob` 在 `Models()` 中注册。字段级外键未在本次打开 `models_drama.go` 全文，不列举。

会话存在 `AuthSession`（`Models()` 注册，`CurrentUser` 读取）。未在源码中证实会话放在 Redis。

Redis 已读用途（`coordination.go`）：键前缀 `canvas:rate:` + 调用方 key，固定窗口 Lua。并发槽位另有 `acquireSlotScript`（ZSET）。熔断、路由目录、画布 Pub/Sub 的键名未在本次打开对应函数，不列举。无 Redis 时限流与槽位用进程内 map。

文件与对象：启动数据目录由 `CANVAS_BACKEND_DATA_DIR` 决定。资源物理路径、OSS SDK（`go.mod` 含 AWS、七牛、腾讯 COS、火山引擎）的写入函数本次未逐个打开。不把「默认本地目录布局」写成已证实的完整清单。`Resource` 模型在 `Models()` 中，字段未展开。

## 6. 外部依赖与配置面

### 6.1 已在源码或生产 Compose 中读到的配置

| 变量 | 读取或注入位置 | 默认 / 约束 |
| --- | --- | --- |
| `CANVAS_BACKEND_DATA_DIR` | `main.go` | `data`；生产 `/data` |
| `CANVAS_DATABASE_DRIVER` | `main.go` | `sqlite`；生产 `postgres` |
| `DATABASE_URL` | `main.go`、migrate 服务 | 空则由 sqlite 打开逻辑处理（打开函数未读） |
| `CANVAS_AUTO_MIGRATE` | `main.go` | 默认 true；生产 `"false"` |
| `CANVAS_BACKEND_ADDR` | `main.go` | `:8080`；生产不映射到宿主机端口 |
| `CANVAS_SHUTDOWN_TIMEOUT` | `main.go` | `10m` |
| `CANVAS_UPDATER_TOKEN` / `CANVAS_UPDATER_SOCKET` | `main.go` | token 空则不配置更新客户端 |
| `REDIS_URL` | `NewCoordinator` | 空且 postgres 则错误；生产 `redis://redis:6379/0` |
| `CANVAS_REGISTRATION_ENABLED` | 生产 Compose 默认 `false`；`RegistrationEnabled()` 的环境解析函数本次未打开 | 首用户路径不看该开关 |
| `CANVAS_ALLOW_PRIVATE_UPSTREAMS` / `CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS` | 生产 Compose 默认 false / 空 | 校验函数在 `outbound` ，本次未读函数体 |
| `CANVAS_CORS_ORIGINS` | `main.go` 调用 `cors()` | 策略函数体未读，不描述放行主机算法 |
| `YINGCE_AGENT_URL` / `YINGCE_AGENT_TOKEN` / `YINGCE_AGENT_BRIDGE_HOST` | `runCloudAgentPi` 与生产 Compose | 生产指向 `http://yingce-agent:8081` |
| `YINGCE_AGENT_TOKEN` 长度 | `server.mjs` | 少于 32 字符进程退出 |
| `MAX_CONCURRENT_SESSIONS` | `server.mjs` | 默认 30，上限 64 |
| `VITE_CANVAS_BACKEND_URL` | `request.ts` | 默认 `"/api"`；`withCredentials: true` |

`go.mod` 直接依赖还包括 Gin、pgx、sqlite、redis、AWS/七牛/COS/火山 SDK、`golang.org/x/crypto`。前端打包器与 UI 库版本未打开 `web/package.json`，不列举。

### 6.2 密钥面（已读）

- 浏览器：`use-config-store.ts` 把整个 `config`（含渠道 `apiKey`、`runningHub.apiKey` 等字段）经 `partialize` 写入 `scopedLocalStorage`。这是浏览器持久化，不是服务端密钥库。
- 渠道密钥入库加密函数未在本次打开。不声称「全部 API Key 已加密」。
- 画布分享 token 以 `TokenHash` / `TokenCipher` 存储，JSON 不输出这两个字段。
- 任务 `MediaRecoveryJSON` 的 json tag 为 `"-"`，注释写明服务端检查点、不是公开结果。解密发生在 `processTask` 的 `decryptTaskInputJSON`。加解密算法未读。
- 生产数据库口令经 Compose `${POSTGRES_PASSWORD:?...}` 强制提供。`.env` 实例未读取。
- `yingce-agent` 与 updater 可共用 token：Compose 写 `YINGCE_AGENT_TOKEN: ${YINGCE_AGENT_TOKEN:-${CANVAS_UPDATER_TOKEN:?...}}`。

### 6.3 生产拓扑（`docker-compose.deploy.yml`）

服务：`postgres:17-alpine`、`redis:7.4-alpine`（AOF everysec）、`migrate`（同一 backend 镜像，`migrate-schema up`）、`yingce-agent`、`backend`、`web`。仅 `web` 发布端口 `${CANVAS_HTTP_PORT:-3000}:3000`。backend 镜像变量必须含 digest 占位（Compose 的 `:?` 错误文本如此要求）。`yingce-agent` 镜像默认可退回 tag `CANVAS_IMAGE_TAG`，注释写明旧更新器兼容，与 backend/web 的 digest 约束不一致。

`nginx.conf`：通用 `location /api/` 反代 `backend:8080`，读超时 3600s，未全局关闭缓冲。仅以下正则关闭缓冲与 gzip：`/api/tasks/.../text-events` 与 `/api/agent/runs/.../events`；以及 `/api/ai/system/...` 的 responses、chat/completions、streamGenerateContent。画布 realtime 路径升级 WebSocket 并 `proxy_buffering off`。SPA：`/` `try_files` 到 `index.html`；`/canvas`、`/welcome`、`/inspirations`、`/share/canvas/` 有单独回退，避免目录 301。

## 7. 交互与前端状态机要点

路由守卫（`router.tsx`）：

- 认证页在 `AuthScene` 下，无工作台壳。
- 业务页多数包 `RequireAuth`。短剧四条路径再包 `RequireFeature feature="shortDramaEnabled"`。图片/视频工作台、任务中心、插件中心各有对应 feature。
- `/admin` 有大量子路由（用户、渠道、逻辑模型、电商、插件、支付、运行策略、存储、系统更新等）。前端隐藏不能代替服务端鉴权；服务端是否每条都 `RequireAdmin` 未逐条证实。
- `/wallet` 不渲染页面。`WorkspaceWalletHost` 挂在顶栏模块（`app-top-nav.tsx` 的 import 已见到），用 `useState(open)` 控制弹层，tab 类型为 `topup | redeem | history`。积分关闭时注释写明仍可购买存储空间。

API 状态机（`request.ts`）：

- 成功条件是信封 `code === 0`，返回 `data`。HTTP 200 且 `code !== 0` 抛 `ApiError`，保留 `reason`、`details`、`retryable`、`Retry-After`。
- 可重试状态：408、425、429、5xx；无 HTTP 响应的传输失败也标为可重试。
- 取消变为 `AbortError`，不包装成普通 `ApiError`。

生成侧：创建任务成功即通知钱包刷新，注释写明积分在创建时已预占。任务观察、SSE `text-events`、节点写回的状态机函数本次未逐行阅读。

i18n：`i18n/index.ts` 同步 `init`，`lng` 取 `useLocaleStore`，`fallbackLng` 为 `zh-CN`。语言切换订阅 store。文案键覆盖 common、language、nav、以及文件后部未全文摘录的 shell。`language.pending` 明确第一期未翻译完。

画布本地状态：`use-config-store` 使用 Zustand persist。画布文档 store 的 localforage 封装文件本次未打开，因此不描述 journal 键名。`AGENTS.md` 声称画布走 localforage、localStorage 只放小配置；与已读的 config store 一致的部分是「渠道配置进 scoped localStorage」。画布大文档是否严格不进 localStorage：未在源码中证实。

节点交互（选中、连线、视口缩放）的事件处理未逐组件阅读。类型层已有节点、以及 `CanvasConnection` 所在文件后部未摘录。不描述快捷键表。

## 8. 算法与编排细节

不存在单独命名为「评分服务」或「衔接评分」的包。编排是数据库状态机 + 任务队列，规则集中在 `production` 与 `drama_orchestrator`。

### 8.1 任务领取

输入：运行时并发上限、协调器租约、数据库中可领取任务。  
输出：至多 `workerConcurrency` 个并行执行；领不到任务则释放全局槽。  
边界：领取上下文 5 秒；租约时长参数 45 秒；全局槽租约 `workerSlotLeaseDuration = time.Minute`。失败重试、路由 failover、上游轮询推迟的函数（`task_route_executor`、`shouldDeferProviderTask`）本次未读，不描述次数与退避。

### 8.2 生成模式分派

输入：任务 `InputJSON` 反序列化后的 `canvasGenerationInput`（mode、prompt、config、metadata、参考媒体）。  
输出：对应 provider 的 result map，或错误。  
边界：

- 视频不套用 `promptTemplateOperation` 编译结果；图片与文本会。
- 工作流接口在普通能力校验之前返回。
- 文本 Agent（`input.AgentRequests != nil`）走 `runAgentToolTask`，否则 `runTextTask`；若 `ecommercePlanningBatchable` 为真则走 `runEcommercePlanningBatches`（该函数未读）。
- 空 prompt、缺密钥、未安装的 Gemini 格式、即梦缺 SecretKey、非法 mode，均在出站前失败。

### 8.3 短剧规划

输入：`DramaProduction` 阶段、章节摘要（字数与游标）、角色、集与段状态、设置中的分集模式与是否并行出片。  
输出：本轮要插入的 `DramaJob` 与集阶段迁移。  
边界：见第 4.4 节常量。`plan` 注释写明重复执行跳过已排队项。手动生图、抽样审阅、逐集审阅通过 `mediaHeld` / `sampleBlocked` / `needsReview` 卡住视频阶段；这些函数体已读到条件表达式，未读设置枚举的全部取值定义。

### 8.4 分镜规则（`production/storyboard.go`）

输入结构 `StoryboardSettings`：段模式 `auto|fixed|range`、段秒数、节奏五档（`rapid` 到 `slow`，注释默认 `fast`）、对白密度、转场、表演强度与运镜动感 1–10。  
常量边界：转场默认且下限相关常量 `DefaultTransitionMs = 150`；单镜手动上限 `MaxStoryboardShotSeconds = 60`；段时长可设范围常量 `MinSegmentSeconds = 4`、`MaxSegmentSeconds = 60`。注释写明实际还要被视频模型单次上限收紧。`SegmentLimits.TargetSeconds`：range 模式返回 `(MinMs+MaxMs)/2000` 的整数秒。切分函数正文未读完，不给出切段伪代码。

### 8.5 尾帧衔接

输入：当前段草稿、上一段 `DramaSegment`、角色表、画幅与视频模型档。  
输出：`DramaSegmentRequest`（秒数、画幅、分辨率、提示词上限、是否出声）以及参考图列表。  
边界：并行出片不传上一段；上一段视频或尾帧未就绪则 `FirstFramePending` 而不发首帧；段时长超过模型上限返回错误。尾帧提取：`saveDramaTailFrame` 注释为用 ffmpeg 取本地视频最后一帧。ffmpeg 参数未读。

### 8.6 工作流提示词字段启发式

`workflowPromptCandidateScore(fieldName, classType, metaTitle) int`：把字段名与标题拼成描述，若包含 negative / 负面 / 反向等标记则减 100。这是工作流节点字段匹配，不是成片质量分。其余加减分未读完。

### 8.7 限流

`Coordinator.Allow`：Redis 上 `INCR canvas:rate:{key}`，首次设置 `PEXPIRE` 为窗口毫秒；计数大于 limit 则拒绝。无 Redis 时用进程内窗口。任务创建的 limit 来自运行时策略 `TaskCreatePerMinute`，窗口 1 分钟。策略默认数值未读 `runtime_policy.go`，不写死每分钟次数。

未发现独立的「镜头衔接打分」或「视频质量评分」模块（在 `backend/internal` 搜索 `func .*Score` 只命中上述工作流启发式）。未在源码中证实有单独的预算求解器；已读到的是短剧预算/积分暂停文案，以及 Agent `MaxCharge` 比较。

## 9. 质量与风险

分级：阻塞 = 按已读代码会在默认生产路径上丢失数据、错误鉴权或无法启动；高 = 错误契约、密钥暴露面或升级误导，会直接造成错误扣费/错误运维；中 = 边界清晰但容易在二开或故障时放大；低 = 死代码或文档缺口。

| ID | 级 | 问题 | 证据 | 说明 |
| --- | --- | --- | --- | --- |
| R1 | 高 | `POST /tasks` 创建失败一律 `fail(..., 400, err)` | `handler/routes.go` 49–51 行；对比 `failService`（`response.go` 30 行起）会按 `AppError.Status` 与 reason 写出 | 登录失败仍走 `failService`。`CreateTask` 返回的配额、积分、权限类 `AppError` 会被压成 HTTP 400、`code` 等于 400。前端 `ApiError` 依赖 `reason` 与状态判断重试和钱包/扩容。本次未枚举 `CreateTask` 的全部分支是否都返回 `AppError`，但计费超限路径返回 `BadAuthRequest`（`task_creation.go` 226 行）。同文件时间线转写接口对用户解析使用 `failService`，创建任务没有 |
| R2 | 高 | 第三方渠道密钥随前端配置持久化在浏览器存储 | `use-config-store.ts` `partialize: (state) => ({ config: state.config })`，storage 为 `scopedLocalStorage`；`apiKey` 字段在渠道与 RunningHub 结构上 | XSS 或本机配置泄露即可拿走密钥。与「任务经登录态后端中转」不矛盾：中转之前密钥已经在浏览器。服务端是否拒绝把密钥打进日志：未在源码中证实全局保证 |
| R3 | 高 | 发布说明中的 Schema 版本落后于代码 | `CHANGELOG.md` v1.7.3.5：「数据库 Schema 从 78 升至 79」；`CurrentSchemaVersion = 81` | 按 CHANGELOG 判断「已升到 79 即最新」会漏掉 80 与 81。迁移只向前且无 down，误判版本的回滚只能靠备份 |
| R4 | 中 | 无全局登录中间件 | `RegisterCanvasAPI` 只 `Use(StorageReadOnlyGuard)`；各 handler 自行 `currentUser` | 已读的任务与 Agent capabilities 有校验。新增路由若遗漏 `currentUser`，编译器不会失败。未发现某一条具体业务路由已缺少校验（未做全路由审计） |
| R5 | 中 | 首个用户注册不看注册开关 | `PublicAuthSettings` 在 `count == 0` 时 `RegistrationEnabled: true`；`Register` 仅 `count > 0` 时检查开关 | 空库暴露在公网时，先到达的请求成为首个账号。是否同时写成 admin：本次未读完 `Register` 后半，不断言角色 |
| R6 | 中 | 数据库迁移不可逆 | `migrate-schema` 无 `down` | 生产 `migrate` 服务在 backend 之前执行 `up`。回滚应是备份恢复，不是 schema downgrade。备份脚本本身未在本次阅读 |
| R7 | 中 | 大 JSON 放在主库 | `CanvasProject.PayloadJSON`、`CanvasSnapshot.PayloadJSON`、`Task.InputJSON`/`ResultJSON`/`TextDraft` | 画布与任务历史随项目增长。SQLite 单文件与 Postgres TOAST 的容量后果是推断，不写成已测量 |
| R8 | 中 | SQLite 默认可无 Redis | `NewCoordinator` | 单进程开发成立。多副本部署若仍用 sqlite 且不配 Redis，限流与 worker 槽只在进程内。生产 Compose 已配 Redis，这条主要约束源码自建 |
| R9 | 中 | 文档入口失效 | `docs/index.md` 链到 `content/docs/overview/docker.mdx`；`README.md` 链到 `quick-start.mdx` 与 `canvas/canvas-node-manual.mdx` | 这三个路径不在现有 22 个 mdx 中。部署与画布操作无法从这些链接读到 |
| R10 | 中 | 生产 Agent 镜像 digest 约束弱于 backend/web | `docker-compose.deploy.yml` `yingce-agent.image` 默认 `ghcr.io/ddcat-ai/open-ai-canvas-yingce-agent:${CANVAS_IMAGE_TAG}` | backend/web/migrate 使用 `:?` 且错误文本要求 digest。Agent 可随 tag 浮动。注释说明是为了旧更新器 |
| R11 | 低 | `LogicalModelPriceSKU` 未注册、无引用 | `models_logical_model.go` 84–105 行；`schema.go` `Models()` 无此类型；全仓符号搜索仅此一处 | 统一定价 SKU 若曾计划使用，当前不会建表也不会被读写。不是运行中的错误路径 |
| R12 | 低 | `/wallet` 路由元素为 `null` | `router.tsx` 177–178 行 | 积分 UI 在弹层。直接打开 `/wallet` 没有页面。导航 slug 仍叫积分中心 |
| R13 | 低 | i18n 与产品表面不一致 | `zh-CN.ts` `language.pending` | 六语切换已接线，大部分页面是否仍硬编码中文：未做全文件字符串审计。文案自己承认未完成 |
| R14 | 低 | `internal/app` 仍是巨大组合根 | `newService` 同时组装计费、任务、插件、技能、认证、画布、工作台 | 与 `service` 包「稳定导入面」的拆分方向一致，但短剧、生成、Agent 仍大量留在 `app`。二开时容易改到上游核心文件 |

未发现（在已读路径内）的问题，避免误报：

- 未发现登录使用 JWT。会话是服务端记录加 Cookie。
- 未发现 `processTask` 把未知类型当成成功。
- 未发现远程 Agent 在 `YINGCE_AGENT_URL` 已设置时仍回退内嵌 Node：`runCloudAgentPi` 是 if/return 分支。
- 未发现支付回调、资源删除竞态、SSRF 主机判断的具体缺陷：相关函数体未读，不能给出「安全」或「不安全」的实现级结论。`CANVAS_ALLOW_PRIVATE_UPSTREAMS` 默认 false 只说明 Compose 默认值。

可回滚性：应用镜像方面，生产 backend/web 变量文本要求 digest；Agent 见 R10。数据方面见 R6。Web 卷注释写「保留当前与上一版 hashed chunks」，entrypoint 脚本未读，不描述保留天数。

## 10. 二开建议

以下边界来自已读代码的模块切分，不是风格偏好。

### 10.1 适合扩展的位置

| 扩展 | 落点 | 原因 |
| --- | --- | --- |
| 新模型协议 | `plugin-packages` 的 `*.yingce-plugin`，由 `newPluginRuntime(dataDir)` 装入 | 生成总线已按 `protocolRegistry` 与 `runWorkflowProviderTask` / mode 分派。未读插件 SDK 手册，接入前应读 `internal/protocol` 的解析与一个现有包的 manifest |
| 新短剧步骤 | `dramaRun.plan` / `advanceEpisode` 的 job kind，执行仍 `CreateTask` | 调度是幂等入队，不要在前端定时器里重做状态机 |
| 新分镜约束 | `production.StoryboardSettings` 与注释所写的「提示词、时长、切段、费用、成片读同一份规则」 | 避免只改 UI 滑杆 |
| 新管理配置 | 已有 `/admin/settings/*` 与 `SystemSetting`（在 `Models()` 中） | 未读设置读写 API，不指定函数名 |
| 新页面 | `web/src/pages` + `router.tsx`，请求只走 `http` | `request.ts` 注释禁止业务模块再 `axios.create` |
| 技能（Agent Markdown） | 启动路径 `EnsureBuiltinSkills` / `EnsureSkillPackages` | 与协议插件不是同一套包。技能目录 `backend/internal/skills` 本次未逐文件阅读 |

### 10.2 不建议在二开里改写的上游核心

| 区域 | 原因 |
| --- | --- |
| `handler/response.go` 信封与 `kernel/error_codes.go` | 前端 `ApiError` 按 `code`/`reason` 分支。改码会破坏重试与配额提示 |
| `CreateTask` 的计费与 `NonBillable` Agent 根任务 | 直接决定是否预占积分、是否进 worker |
| `taskWorkerCoordinator.start` 的租约 owner 与 `ClaimNextTask` | 注释明确防止旧执行者覆盖。改领取语义会影响多副本 |
| `processTask` / `processCanvasGenerationTask` 的模式开关 | 所有画布生成与短剧出片的汇合点 |
| `runCloudAgentPi` 的远程/内嵌互斥 | 生产依赖不回退。加回退会让同一步在两个运行时各执行一次（这是代码结构表达的风险，不是本次复现的事故） |
| `CurrentSchemaVersion` 与 `schemaMigrations` 只追加 | 已发布库靠 checksum 向前。改旧迁移 checksum 会导致已升级库无法对齐 |
| `Models()` | 注释规定启动与跨库迁移共用。漏注册的类型不会建表（R11 即例子） |
| `CanvasSnapshotResource` 的 `OnDelete:RESTRICT` | 已读到的数据库级防删约束。去掉会让历史快照指向已删资源 |
| nginx 里 SSE/WebSocket 的单独 location | 通用 `/api/` 没有 `proxy_buffering off`。把事件流并进通用 location 会缓冲流 |
| 生产 Compose 的 migrate 先于 backend、`CANVAS_AUTO_MIGRATE=false` | 避免多副本同时 AutoMigrate |

二开时优先新增域包或插件，避免把客户逻辑继续堆进 `internal/app` 的已有大文件。`service` 包应保持再导出，handler 不要开始 import `app` 或 `repository`（与 `service/doc.go` 的稳定导入面一致）。

## 11. 附录：关键文件索引

| 主题 | 路径 | 符号 |
| --- | --- | --- |
| 进程入口 | `backend/cmd/server/main.go` | `main`，`run` |
| 路由聚合 | `backend/internal/handler/api.go` | `RegisterCanvasAPI` |
| 任务 HTTP | `backend/internal/handler/routes.go` | `RegisterTaskRoutes` |
| 响应信封 | `backend/internal/handler/response.go` | `ok`，`fail`，`failService` |
| 错误码 | `backend/internal/kernel/error_codes.go` | `CodeOK`，`CodeRateLimited` |
| 导入面 | `backend/internal/service/doc.go` | package 注释 |
| 组合根 | `backend/internal/app/service.go` | `newService`，`StartWorker` |
| 创建任务 | `backend/internal/app/task_creation.go` | `CreateTask` |
| 执行分派 | `backend/internal/app/task_execution.go` | `processTask` |
| Worker | `backend/internal/app/task_worker.go` | `taskWorkerCoordinator.start` |
| 生成总线 | `backend/internal/app/provider.go` | `processCanvasGenerationTask` |
| 短剧创建 | `backend/internal/app/drama_service.go` | `CreateDrama` |
| 短剧调度 | `backend/internal/app/drama_orchestrator.go` | `startDramaScheduler`，`tickDrama`，`plan`，`advanceEpisode` |
| 段视频 | `backend/internal/app/drama_segment_video.go` | `prepareDramaSegment`，`saveDramaTailFrame` |
| 分镜规则 | `backend/internal/production/storyboard.go` | `StoryboardSettings`，`SegmentLimits` |
| 工作流字段分 | `backend/internal/app/workflow_provider_values.go` | `workflowPromptCandidateScore` |
| Agent 分支 | `backend/internal/app/cloud_agent_pi_runtime.go` | `runCloudAgentPi` |
| Agent HTTP | `backend/internal/handler/agent.go` | `RegisterAgentRoutes` |
| Agent 进程 | `yingce-agent/server.mjs` | `authorized`，`maxSessions` |
| 会话 | `backend/internal/auth/auth.go` | `PublicAuthSettings`，`Register`，`CurrentUser` |
| 协调 | `backend/internal/platform/coordination.go` | `NewCoordinator`，`Allow` |
| 模型清单 | `backend/internal/database/schema.go` | `Models` |
| 迁移版本 | `backend/internal/database/migrations.go` | `CurrentSchemaVersion`，`schemaMigrations` |
| 迁移 CLI | `backend/cmd/migrate-schema/main.go` | `main` |
| 任务表 | `backend/internal/model/models_task.go` | `Task` |
| 画布表 | `backend/internal/model/models_project.go` | `CanvasProject`，`CanvasShare` |
| 快照 | `backend/internal/model/models_canvas_history.go` | `CanvasSnapshot`，`CanvasSnapshotResource` |
| 未接入 SKU | `backend/internal/model/models_logical_model.go` | `LogicalModelPriceSKU` |
| 前端路由 | `web/src/router.tsx` | `router` |
| API 客户端 | `web/src/services/api/request.ts` | `http`，`ApiError` |
| 创建任务客户端 | `web/src/services/api/task-center.ts` | `createGenerationTask` |
| 画布生成 | `web/src/lib/canvas/canvas-project-generation.ts` | `runBackendCanvasGenerationTask` |
| 节点类型 | `web/src/types/canvas.ts` | `CanvasNodeType`，`ViewportTransform` |
| 渠道配置 | `web/src/stores/use-config-store.ts` | persist `partialize` |
| 语言 | `web/src/i18n/index.ts`，`locales.ts`，`messages/zh-CN.ts` | `i18n`，`SUPPORTED_LOCALES` |
| 积分弹层 | `web/src/components/layout/workspace-wallet-modal.tsx` | `WorkspaceWalletHost` |
| 生产编排 | `docker-compose.deploy.yml` | `postgres`，`redis`，`migrate`，`yingce-agent`，`backend`，`web` |
| 网关 | `nginx.conf` | SSE location、`/api/`、SPA |
| 版本 | `VERSION`，`CHANGELOG.md` | `v1.7.3.5` |
| 文档缺口 | `docs/index.md`，`README.md` | 失效链接见 R9 |
| CI | `.github/workflows/quality.yml` | `Quality checks` |

## 12. 审查覆盖声明

本报告每一条实现结论都能回到上表或正文中的路径与符号。下列内容明确未证实，后续审查应单独立项，而不能从本报告外推：

- 全部 Gin 路由是否都调用了 `currentUser` 或管理员校验。
- `CreateTask` 之后的扣费、退款、路由 failover、上游轮询与媒体落盘的完整状态机。
- 资源删除的引用扫描是否覆盖全部 JSON。
- SSRF、CORS 空策略、支付回调签名的函数体。
- 109 个插件包的协议正确性。
- 前端画布交互、撤销、多人实时与 localforage 同步的正确性。
- 测试是否通过。本次未执行 `go test` 或 `bun test`。
