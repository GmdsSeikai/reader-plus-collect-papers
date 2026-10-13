# 论文采集改造方案

状态：proposed。日期：2026-10-12。目标：在 Reader 的既有论文库前增加任务驱动的论文发现、摘要筛选与人工入库能力。本阶段只交付设计，不实施业务代码、操作用户书库或更新安装包。

目标源码基线：`4ac8435cccba0ce70e36510a80f9571461b94ee8`，版本 `0.5.1`。设计输入为 anx-reader 的 `paper-collection-adoption-prompts.md` 及其引用的 `paper-collection.md`；后者声明的源项目基线为 `f4967642a57d3d744120f24bca47c3f2ead121e3`。本方案完整阅读两份参考文档并核对 Reader 源码，没有独立复验 anx-reader 实现。验收合同见[验收矩阵](paper-collection-acceptance.md)。

## 1. 问题与目标

Reader 已能管理、查找和阅读单篇论文，但没有研究任务、定期发现、发现范围断点、独立引用刷新及按请求计量的摘要分析。直接调用现有“在线导入”批量搜索会把发现与下载混在一起，也无法证明分页完整、固定下载版本或分析取消隔离。

建议保持既有 `Document` 和阅读器，在前面建立独立的候选域：

```text
任务 → arXiv 发现 → 候选与发现依据提交
                         ├→ Semantic Scholar 引用刷新
                         └→ 可选 AI 翻译与摘要相关性分析
                                      ↓
                         用户查看、筛选、明确选择
                                      ↓
                      固定版本下载 → 校验 → 既有论文库
                                      ↓
                           PDF.js、笔记、批注与聊天
```

候选不是 `Document`，没有下载的候选不进入“全部论文”、阅读进度、分类或解析队列。低分、未分析、引用未知、额度用尽、网络失败均保留候选。没有 AI 账户仍能完成任务保存、发现、原文展示、选择、PDF 入库和阅读。

第一版仅以 arXiv 为自动发现与下载源，接受预印本；引用统计用 Semantic Scholar。保留当前手动在线导入的其他来源。采集不执行全文解读，不新增云服务、系统常驻服务、跨设备同步、作者身份消歧或引用增速。

## 2. 已确认的项目构造与职责映射

以下为已有入口，不代表采集已实现。拟新增的文件在第 3 节单独列出。

| 参考职责 | Reader 已有入口与事实 | 适配及缺口 |
| --- | --- | --- |
| 用户与实例 | [library_lock.go](../apps/server/internal/reader/library_lock.go) 的 `LockLibrary` | 本地单用户；同一数据根只能由一个服务进程持有。不同数据根仍可并行，不能把这把锁当作整机来源限流 |
| 后端装配 | [main.go](../apps/server/cmd/reader-server/main.go) | Go 在 loopback 启动，调用 `StartProcessing`；增加同级采集生命周期，不交给 React 页面计时器 |
| 数据 | [store.go](../apps/server/internal/reader/store.go)、[organization.go](../apps/server/internal/reader/organization.go) | `modernc.org/sqlite`，WAL、外键、单连接池；采用主库独立前缀表，补模块版本与事务，不照抄源项目 schema 数字 |
| 服务鉴权 | [server.go](../apps/server/internal/reader/server.go) 的 `Handler` | 复用 Bearer token、Host、Origin 边界，所有采集 API 均经过现有中间件 |
| 网络与发现 | [scholar.go](../apps/server/internal/scholar/scholar.go)、[sources.go](../apps/server/internal/scholar/sources.go) | 已有 arXiv 单篇 Atom、其他来源查找和代理；缺搜索分页、完整日期/版本、来源串行门禁、切片与覆盖 |
| 引用 | `sources.go` 的 `s2`、`s2Search` | 已有元数据查询，当前字段没有 `citationCount`；新增按明确 arXiv ID 的批次统计，不能复用模糊标题匹配证明身份 |
| AI 配置 | [connections.go](../apps/server/internal/reader/connections.go)、[model_defaults.go](../apps/server/internal/reader/model_defaults.go) | 已有 Codex、Claude、Kimi、API 配置及文字能力检测；复用配置所有者，不读取 CLI 凭据文件 |
| AI 执行 | [generation.go](../apps/server/internal/reader/generation.go)、[cli.go](../apps/server/internal/reader/cli.go)、[codex.go](../apps/server/internal/reader/codex.go) | 可复用协议解析；现有备用路由、自动模型重试、阅读系统提示词与调用边界需适配，不能直接等同采集分析 |
| 会话与后台 | [codex_chat.go](../apps/server/internal/reader/codex_chat.go)、[translation_codex.go](../apps/server/internal/reader/translation_codex.go)、[processing.go](../apps/server/internal/reader/processing.go) | 聊天缓存与翻译 worker 已有独立会话；采集需要自己的请求、取消、等待与结果所有者 |
| 入库 | `server.go` 的 `importFile`、[metadata_lookup.go](../apps/server/internal/reader/metadata_lookup.go) 的 `applyFound` | 文件 SHA256 前 32 个十六进制字符为 `Document.id`，正式文件移动后提交文档与处理记录；提取共享提交入口，补文件中断日志、采集关联及完整摘要核验 |
| 既有导入竞争 | [zotero_import.go](../apps/server/internal/reader/zotero_import.go)、[merge.go](../apps/server/internal/reader/merge.go)、[trash.go](../apps/server/internal/reader/trash.go) | 已有 `importMu`，Zotero 逐附件事务、合并与回收站；采集与这些写入统一入场和排空，不能另建互不协调的锁 |
| 文件限制 | [libraries.go](../apps/server/internal/reader/libraries.go) | 论文库上限 `50 << 20` 字节、150 页；本地导入对 51–150 页需确认，在线 resolve 当前直接允许；采集沿用本地确认规则 |
| UI | [App.tsx](../apps/web/src/App.tsx)、[PaperSidebar.tsx](../apps/web/src/papers/PaperSidebar.tsx)、[PaperLibrary.tsx](../apps/web/src/papers/PaperLibrary.tsx) | React 19、Vite 7、Zustand 5；已有侧栏、表格、详情与导入对话框。扩展导航，候选使用独立数据与选择状态 |
| 阅读 | [reader-core](../packages/reader-core/src/index.ts)、`apps/web/src/readers/` | PDF.js 与 Readium 既有路径；不改变 EPUB locator 或把候选伪装成可读文件 |
| 关闭与交付 | [Electron main](../apps/desktop/main/index.ts)、[releasing.md](releasing.md) | 窗口先 flush renderer，然后 `child.kill()`；Go HTTP shutdown 为 5 秒。需增加跨进程排空确认；正式包为 Windows x64、macOS arm64 |

没有发现现有云同步、主库在线替换或完整存储根迁移 UI。不能把 anx-reader 的 WebDAV、WebView 元数据队列和 Windows 安装目录移入本项目。现有离线搬迁遵循关闭后复制整个书库的方式。

## 3. 提案：模块边界与生产调用链

沿用 `internal/reader` 作为应用装配和事务协调所有者，沿用 `internal/scholar` 作为外部论文服务所有者。不新增 Flutter、WebView、ORM、通用插件框架或第二套 HTTP 服务。

| 拟新增模块 | 责任与生产消费者 |
| --- | --- |
| `internal/reader/collection_models.go` | 任务、候选、结果、版本与状态；由持久化、API、调度及分析共同使用 |
| `collection_store.go` | 模块迁移、扫描页事务、额度、查询、状态恢复与排序投影 |
| `collection.go` | `CollectionManager`：任务互斥、时钟、取消、执行 token、恢复、公平队列和排空 |
| `collection_discovery.go` | 分支、窗口、切片、分页、补扫和覆盖判定 |
| `collection_citations.go` | 引用队列、身份核验、未知/零/过期、刷新与退避 |
| `collection_analysis.go` | 提示词、输入预算、完整结果校验、缓存及分析投影 |
| `collection_import.go` | 人工选择、固定版本下载、导入日志、对账和结果确认 |
| `collection_http.go` | 鉴权 API，提交命令并查询持久状态，不在 handler 生命周期内执行整次采集 |
| `internal/scholar/arxiv_search.go`、`citations.go`、`source_gate.go` | 官方来源查询与共享门禁；现有单篇查找也接门禁 |
| `internal/reader/import_commit.go` | 从 `importFile` 提取实际校验/提交步骤，共享文件日志和可等待的提交结果；现有入口保留 |
| `internal/reader/generation_admission.go` | 所有生成入口的原子入场与发出通知；采集额度挂在真实 attempt 边界 |
| `apps/web/src/papers/collection/` | `CollectionPage`、候选详情、任务表单、采集设置与运行状态；组件采用 `packages/ui` |

文件名为建议，责任与调用边界是实施合同。新增任务类型放 `packages/reader-core` 的采集文件并由现有入口导出；HTTP 格式以 `docs/openapi.yaml` 为 owner，运行 `pnpm api:generate` 生成类型，禁止手改 `schema.d.ts`。

实际链路：`main.go → StartCollection → CollectionManager → scholar → collection_store`；`collection_http → manager.Enqueue → 持久状态`；`collection_import → 共享导入提交 → Document → 既有 PDF 阅读页`。引用和分析分别从已提交候选取工作，不阻塞发现事务。

### 3.1 HTTP 合同草案

以下为新增路由提案，实施时与 OpenAPI、生成类型、handler 和正式 HTTP 测试一起提交。现有 `POST /api/documents/resolve` 保留单篇手动导入职责，不承担任务发现。

| 方法与路径 | 请求/结果合同 |
| --- | --- |
| `GET/POST /api/collection/tasks` | 列任务/保存新任务；返回 UUID、配置版本、修订、启用状态与下次自动时间 |
| `PUT /api/collection/tasks/{id}` | 更新需带预期修订/配置版本；冲突返回 409，避免旧表单覆盖新语义 |
| `POST /api/collection/preview` | 取表单配置的少量查询样例；不保存正式候选/覆盖，仍经过限流与输入校验 |
| `POST /api/collection/tasks/{id}/runs` | 提交单任务运行命令，202 返回 run ID；重复点击返回已有开放运行，不启动重复工作 |
| `GET /api/collection/runs/{id}` | 查询固定范围、修订、当前阶段、持久计数、状态、未覆盖原因和重试时间 |
| `POST /api/collection/runs/{id}/cancel` | 请求所属运行取消，响应表示接受请求；终态以后续查询为准 |
| `GET /api/collection/candidates` | task/channel/search/analysis/citation/ignored/sort + cursor；页长默认 50，排序用稳定 ID 作 tie-breaker |
| `GET/PATCH /api/collection/candidates/{id}` | 详情/仅修改 ignored；不允许前端修改原文、引用数、分数或下载地址 |
| `POST /api/collection/candidates/{id}/analysis` | 提交手动分析，202 返回 job/attempt 查询身份；不会绕过额度和启用任务 |
| `POST /api/collection/imports` | 仅接受已选候选 ID、固定版本及预期正文身份，202 返回逐项 request ID；过期选项要求刷新确认 |
| `GET /api/collection/imports/{id}` | 持久状态、所选版本、逐项错误、确认条件、成功 Document ID 和所在库 |
| `POST /api/collection/imports/{id}/confirm` | 确认篇幅/恢复回收站等明确条件；仅由此决定进入对应正式提交，不允许自动确认 |
| `POST /api/collection/imports/{id}/cancel` | 取消下载或后续排队，已经 importing 返回仍在提交的状态，不伪报 cancelled |
| `GET/PUT /api/collection/settings` | 无秘密模块设置、配置版本与 UTC 日 sent/reserved/不确定数；Key 写入走既有受保护配置 owner |
| `POST /api/collection/pause`、`POST /api/collection/resume` | 模块调度控制，与任务 enabled 独立；返回持久暂停偏好和活跃工作状态 |
| `POST /api/lifecycle/drain`、`GET /api/lifecycle/drain/{id}`、`POST /api/lifecycle/drain/{id}/cancel` | 幂等开始排空、查询状态或撤销尚未进入最终销毁的退出意图；只在真正 drained 后允许退出 |

分析查询状态并入候选详情，未完成输出不暴露为成功结果。初版 UI 定时查询持久状态，不复用聊天 SSE 传输任意调度事件。页面只取消自己的列表请求，生产任务通过明确 cancel 命令控制；迟到列表响应用请求序号拒绝。

游标包含排序方式、任务修订、筛选身份与最后排序值；请求配置变化重置游标，非法游标返回 400。新增错误使用稳定 `code` 与中文 `error`，例如 query-invalid、task-conflict、source-limit、quota-exhausted、analysis-invalid、download-rejected、import-busy；与既有 `RequestError` 等价接入。接受命令的 202 不代表采集覆盖、分析成功或入库完成。

## 4. 用户操作与界面

论文库侧栏增加“论文发现”入口，进入后提供“最新相关”“近期高被引”“任务”三个视图。已有论文库分类、标签、阅读状态、重复合并和 Zotero 导入保留原入口。采用现有列表与侧详情的交互风格，候选详情组件独立，不要求未入库候选具有 `Document`。

任务表单包含名称、开关、1–5 优先级、研究主题、关键词组、组间 OR/AND、作者及别名、可选 arXiv 分类、不关注方向、6/12/24/48 小时频率、最新天数和热门日历月数。组内同义词 OR；默认组间 OR。至少有关键词、分类或作者之一；仅自然语言主题不能保存为可执行来源任务。查询预览明确为少量样例，最多展示 5 条，不代表完整覆盖。

作者独立查询，不附加主题或分类。主题、不关注方向用于 AI 判断，不自动生成查询词、NOT 条件或新来源。作者姓名和机构按来源展示，不声称解决同名问题。

候选默认保留低分和未分析；支持任务、关键词、本地搜索、分析状态、引用状态、低分和已忽略筛选。忽略为候选全局标记，可找回，不删除记录。作者关注命中不因低分隐藏。列表每页 50，日期、累计引用与相关性是独立排序；未选任务时用启用任务中最宽窗口，关闭任务的记录可通过任务筛选查看。

详情显示原始标题、完整摘要、作者、分类、首次提交和版本更新时间、版本、arXiv 链接、发现任务/分支/日期、引用时间及状态、有效译文与证据。没有有效分析就展示待分析或具体失败，不用旧分数冒充新结果。

批量选择后明确点击“下载并入库”；逐项展示下载、等待确认、提交、已入库或失败。51–150 页要求用户确认，超过 50 MiB 或 150 页拒绝论文库入库，提供既有图书库手动导入入口。已有同一 PDF 在图书库时提示所在库，不自动移动。回收站中的文件要求明确恢复，发现或恢复调度不能自动恢复它。

设置复用现有 AI 服务，新增模块级分析启用、服务/模型/effort 覆盖、每日请求额度、应用请求频率限制与可选引用 Key；不增加没有执行消费者的逐任务模型字段。首次启用分析由用户确认；仅配置了阅读 AI 不自动启动摘要分析。入库后的全文处理沿用 `autoTranslatePDF`，界面显示当前开关及设置入口；它的请求与费用独立于采集每日额度。

## 5. 检索、身份、修订与覆盖

### 5.1 稳定身份与任务修订

候选身份为去掉 `vN` 的 arXiv ID，兼容 `YYMM.NNNNN` 与旧分类路径格式；大小写规范遵循来源格式，不能随意改变旧 ID。`latest_version` 单存，首次提交用 `published`，版本更新时间用 `updated`。保存作者原顺序、来源可用机构、类别、DOI、发表信息及发现时间。跨任务迟到的旧版本响应不得降低最新版本或覆盖新正文；用户选择的版本与原文元数据另存导入快照。

任务 UUID 稳定。主题、组间操作符、关键词、作者、分类和不关注方向变化提高 `query_revision`；名称、频率、窗口和模块模型不改变此修订。语义哈希包含相关性材料，查询分支另有哈希。运行保存配置快照、查询、修订与固定截止时间；编辑时使旧运行 token 失效，已提交候选保留，旧关系不参与当前修订排序。

同论文跨任务/分支/双窗口只有一份候选；发现关系按 `(task_id, revision, arxiv_id, channel, branch_hash)` 唯一，保留全部发现依据。查询条件不是本地精确命中证据；仅保存能在实际标题或摘要核对的短语，不编造命中。

规范正文 v1 为固定字段顺序的 UTF-8 JSON：标题、摘要、有序作者姓名、分类；规范化换行与空白，不改写词句。`content_hash = SHA256(canonical_bytes)`，另存 `canonical_version=1`；不照抄参考中“哈希字段实际为 JSON”的存储。PDF 完整 SHA256 是另一职责。

### 5.2 双窗口与切片

最新默认 30 天，热门默认 12 个日历月；UTC 计算、月末夹取。运行开始固定 `now`。最新用 UTC 日桶，热门用固定 UTC 锚点的 7 日桶；边缘桶的覆盖水位不得超过运行截止时间。窗口扩大复用已覆盖相交桶并补缺口，缩窄不调度不相交旧待办。停机补采可保存显示窗口外候选，但不把它们展示为窗口内结果。

首次运行扫描两个窗口；后续最新起点为当前回溯起点与已完成水位减 7 天中更早者。每轮重扫最近 7 天吸收延迟索引；热门当前桶补扫，历史每 7 天启动复核周期，每轮重开至多 10 个历史片段，周期待办和进度跨重启保存。

latest/hot 交替执行切片。默认页 100；总量超过 1,000 时按多日→日→小时→分钟细分；分钟仍超量标记 `source_limit`，不宣称完成。单片单轮 5,000 条为调度预算，达到后保留待办，不截断总候选。

每页同一事务写候选、发现关系、断点与执行 token 条件更新。不得先推进断点再保存候选。恢复从切片第 0 页复扫，通过稳定 ID 去重；页失败、总量漂移、异常空页、Atom 错误 entry、取消和来源上限均不能证明覆盖。来源明确总量为零与异常空页分别处理。总量变化重开当前切片，保留已提交资料。

父切片仅在全部子片覆盖后完成；两个通道所有当前分支都完成才标记运行 `succeeded`，否则为 `partial` 或具体终态。每次响应后检查 run/revision/token；过期响应不能推进断点或当前分数，已发请求仍按原 attempt 结算额度。

### 5.3 来源访问和限流

arXiv 用 HTTPS Atom API，按 `submittedDate` 降序；查询通过参数编码生成，不接受任意用户手写来源表达式。官方使用条款要求 legacy API 单连接且不超过每 3 秒一次，并约束受用户控制的机器总量。[arXiv 使用条款](https://info.arxiv.org/help/api/tou.html)。默认页 100 和超 1,000 细分属于本应用的保守策略。[官方分页说明](https://info.arxiv.org/help/api/user-manual.html#3112-start-and-max_results-paging)。

发现、预览和现有 `arxivMetadata` 经过同一门禁。一个库内使用共享服务；同一本机用户的不同数据根再通过固定用户缓存目录中的 OS 文件锁和最近请求时间串行协调，目录不依赖仓库路径。锁覆盖请求执行，下一请求还需满足间隔；等待可取消，异常退出由 OS 释放锁。现有不同客户端不能另建门禁绕过；本地协调不声称约束其他用户、其他程序或其他机器。

引用默认每秒至多一个请求，和既有 Semantic Scholar 元数据调用共享门禁；无 Key 仍可能被共享公共额度限流，不保证每秒成功。官方说明个人 Key 的初始速率为每秒一个请求，建议使用 batch。[Semantic Scholar 官方教程](https://webflow.semanticscholar.org/product/api/tutorial)。

请求明确 User-Agent；复用 `http.ProxyFromEnvironment` 与 Electron 的代理环境。429/503 按 `Retry-After` 的秒数或 HTTP 日期、指数退避与抖动等待，单次最多 3 次，鉴权错误停止普通重试。所有队列等待、退避和传输可取消；元数据限制响应体大小，完整网络期限与空闲期限分别控制。正式实施前再次核对官方参数和限制，不把参考数值当永久服务承诺。

## 6. 持久化、事务与旧数据兼容

### 6.1 推荐同库逻辑隔离

在 `reader.sqlite` 新增 `collection_*` 表，不另建 `paper_collection.db`。原因：目标已经拥有统一 Store 与导入写入，候选关联可与文档提交同事务，避免制造双库关联竞态。采集范围按本地数据根隔离；既有文件、文档 ID、笔记和阅读定位不变。

| 拟新增表 | 主键/关键约束 | 主要数据 |
| --- | --- | --- |
| `collection_meta` | `id=1` | 模块 schema 版本；高版本拒绝采集写入，不重建或删除 |
| `collection_settings` | `id=1` | 分析启用、额度、服务/模型引用、频率限制、暂停偏好及配置版本；无密钥 |
| `collection_tasks` | `task_id` | 名称、开关、优先级、版本化 config JSON、修订、语义哈希、下次自动运行时间 |
| `collection_candidates` | `arxiv_id` | 最新原始信息、日期/版本、规范化版本与正文 SHA256、全局 ignored |
| `collection_matches` | 任务/修订/论文/通道/分支唯一 | 发现时间、可核对短语、规则分和有效分析投影；任务/候选外键 |
| `collection_runs` | `run_id`；每任务仅一开放运行 | 快照、范围、触发方式、执行 token、状态、计数、失败类与时间 |
| `collection_slices` | 任务/修订/通道/分支/范围唯一 | 父子关系、查询、页断点、总量、覆盖状态、重试时间与来源上限 |
| `collection_citations` | `arxiv_id` | 可空累计数、服务论文 ID、成功/尝试时间、刷新期限与失败 |
| `collection_analyses` | `cache_key` | 正文/profile/model/prompt/schema 身份、完整译文/判断 JSON、结果状态与失败退避 |
| `collection_analysis_jobs` | `job_id`；同论文仅一开放命令 | 固定正文身份、queued/running/succeeded/failed/cancelled/interrupted、错误及完成时间 |
| `collection_quota_days` | `utc_day` | 当日已发与预占数；额度读最新设置，不通过旧快照回写计数 |
| `collection_attempts` | `attempt_id` | 分析请求身份、预占 UTC 日、reserved/dispatching/sent/released/settled、发送/完成时间与失败 |
| `collection_cursors` | `owner_key` | 公平队列、候选 keyset 与热门复核周期；每 owner 独立条件更新 |
| `collection_imports` | `request_id`；同论文仅一活跃请求 | 固定版本及元数据快照、状态、预期/实际完整文件 SHA256、受控相对路径、Document 关联及确认结果 |
| `import_commits` | `commit_id`；完整摘要/目标文件的冲突约束 | 共享文件安装日志：来源归属、目标路径、完整 SHA256、阶段与文档身份 |

关联的 `document_id` 可空；彻底删除文档时用 `ON DELETE SET NULL` 保留候选与历史入库身份。合并保留历史版本关联，若目标文件不同则不得仅靠摘要推断替换关系；打开行为见第 9 节。索引覆盖发布时间、当前任务关系、待重试切片、到期引用、有效分析分数、活跃导入和队列游标。

新表使用普通可索引字段及版本化 JSON，不把所有状态塞入大 JSON。枚举、JSON 和模块版本严格校验，未知值停止对应操作并报错。采用增量迁移事务与集成测试；首次版本为 1，当前模块版本为 3，不挪用主库全局 `user_version` 控制全部既有表。版本 2 的历史复核游标保存任务、分支和固定 UTC 周桶身份；版本 1 游标缺少分支身份，升级后重建待办，保留候选与已保存片段。版本 3 增加持久分析命令，升级仅新增表与索引。升级事务提交版本号后旧版本拒绝采集写入，不能直接用旧程序推进新游标或绕过命令取消；降级保留模块表并禁用采集，不恢复旧待办或删除资料。

### 6.2 必须成组提交的事实

1. 扫描页：候选、发现关系、切片断点和当前执行身份。
2. 分析入场：最新额度检查、UTC 日期归属、attempt 唯一身份和预占。
3. 分析成功：完整有效结果及其对应任务分数投影；不能投影到未返回的任务。
4. 入库数据库阶段：`documents`、元数据、`document_processing`、`collection_imports` 关联与共享日志终态；同库消除“主记录已提交但关联没提交”的正常路径。

文件与 SQLite 仍不能原子提交，需要共享持久日志及启动对账。网络、CLI 等待、下载和 PDF 页数解析均在数据库事务外；用短事务保存状态，避免单连接被长任务占用。

模块迁移失败回滚本次新表变化，原阅读功能保留；若已经成功提交过新格式则保留其表，不因禁用或降级删掉任务。实施现有用户库迁移前，确认目标版本、关闭写入并完成一致备份与恢复验证；本设计不执行该操作。

## 7. 引用统计合同

使用 `POST /graph/v1/paper/batch?fields=paperId,externalIds,citationCount`，请求 ID 为 `ARXIV:<stable-id>`，本地每批最多 100、调度每次先取 20。返回按 `externalIds.ArXiv` 建立映射，不只按数组位置；缺项、null 或身份不符逐项处理，不按标题补猜。

明确整数零为 0；未收录、未查询、缺字段或无有效数量为 NULL。负数、浮点、字符串不转为零。成功后 7 天刷新，失败后 24 小时重试；有成功数值再刷新失败保留旧数并标记“过期”，只有到期而尚未尝试不自动等同失败过期。允许成功引用数下降。

新候选和到期复核交替取工作，批次失败逐项持久退避。热门为首次提交时间窗口内论文的累计引用排序，已知数先于未知；未知候选仍可见。第一版不显示“近期增长”“趋势”或伪造引用历史。

可选 Key 通过既有 `/api/ai/config` 的只写 `citationsApiKey` 字段保存于受保护配置；响应只回 `hasCitationsKey`。空白或省略保留，`clearCitationsKey=true` 明确清除，清除命令不持久化，也不能同时提交新 Key。引用 transport 从该 owner 读取，采集设置不保存密钥或相信客户端提交的存在标记。设置响应的 `quota` 从当前 UTC 日的实际 sent/reserved/uncertain 计数派生，保存偏好时忽略调用方的额度计数。配置写入保持原子文件及权限；密钥不进入任务、模型指纹、普通偏好、日志或报告。库备份包含配置文件时仍需按既有密钥保护处理，不称“备份不含密钥”。本阶段未读取任何实际 AI 配置或 Key。

## 8. AI 分析、缓存、隔离与额度

### 8.1 可复用部分与必须修改的边界

复用 `AIConfig` 的服务、端点、模型目录与能力检测。模块设置可选服务/模型/effort；默认跟随主 Agent 并采用既有翻译任务的较快模型选择规则，发送前解析为具体模型 ID 并固定快照。无法解析/检测时待配置，不隐式使用其他供应商。配置改变使随后请求和对应缓存重新判定。

增加实际消费的 `taskPaperAnalysis`；给 `AIInput` 增加内部系统提示词选择及请求所有者，CLI/API adapter 都使用该选择。采集使用固定结构化提示词，聊天/全文翻译继续原提示词；HTTP 请求不能任意覆盖系统提示词。Codex 创建独立 session/工作目录，不访问 `codexChatCache`；Claude/Kimi 独立进程或会话；API 不复用阅读上下文。

手动分析入口为 `POST /api/collection/candidates/{id}/analysis`，返回 `202` 与已提交的持久命令；重复提交同一论文的开放命令返回同一 ID。服务拥有执行生命周期，关闭页面不取消命令。`GET /api/collection/analysis/{id}` 与候选详情返回状态；显式取消使用 `POST /api/collection/analysis/{id}/cancel`，只针对该 ID，已关闭命令不覆盖终态或取消后续命令。配置关闭、模型切换与暂停在设置事务中关闭开放命令，再取消运行句柄；保存结果事务再次核对命令、正文、任务语义与分析配置。启动对账将 queued/running 标为 interrupted，不自动重发；发送前预占可释放，已发送与发送不确定请求保守计费。

Windows 的 npm `.cmd/.bat` 入口不作为 prompt 的 shell 执行器。启动前核对对应包身份并定位其原生 Codex/Claude 程序；模型发现、请求、可用性检测与会话身份使用同一解析路径。原生安装沿用 PATH；缺少原生程序时明确失败，不将模型或原文拼入 shell。2026-10-13 已用受控原生协议进程验证 npm 布局下的生产分析路径，并用现机 Codex 0.162.1、Claude 2.1.281 的 `--version` 验证启动；后者不证明账号推理或取消行为。

后台自动分析由既有 scheduler 推进。Codex 分析会话只从 `config/read` 解析供应商标识，在本次 `thread/start.config` 覆盖该供应商的 HTTP/流重试次数为 0，不修改 CLI 用户配置。字段依据 [官方配置参考](https://learn.chatgpt.com/docs/config-file/config-reference) 和安装版本生成的 App Server schema；配置响应及密钥不写日志。该路径的受控协议测试不代替实际安装版本和账号验证。

采集路径禁用 `GenerationService` 的自动 `text-api`/`image-api` 备用和 `configuredCLIAdapter` 的未显式计量模型重试。重试由采集 owner 安排，每次解析发送身份、取得额度与门禁；模型不悄悄更换。失败不修改聊天模型的能力状态。请求取消、部分输出和超时不保存成功 JSON，也不写聊天消息；现有聊天 SSE 合同不变。

统一生成入场覆盖聊天、全文翻译、图片生成、能力测试和采集的真实发送路径。共享按连接身份分区的原子 RPM 门禁；CLI 无法读取账户身份时按 provider 保守共享，API 按协议/规范 URL 分区，指纹不包含密钥。模块分析并发默认 1，其他工作继续既有 worker 行为；门禁等待可取消，用户对话优先但用有限轮转避免后台永远无法发出。

RPM 是应用主动节流，不声明等于 CLI 账户官方配额。UI 提供明确的应用限制设置，数值需在接入时依据所选服务确认；不能把每天 100 次误当每分钟限制。每个真实发送均经过同一个 owner，禁止把多次 native attempt 藏在一个上层计量中。

### 8.2 输入与严格输出

仅发送标题、完整摘要、来源类别和当前启用任务的语义材料，不发送 PDF、阅读笔记或聊天历史。系统与用户输入总预算沿用 24,000 字符，明示这是字符预算；超出时拆任务集合，每个请求单独计量，单篇基础材料仍过大标记 `input_too_large`，不截断摘要。

结果 schema v1：非空中文 `title_zh`、`abstract_zh`，以及 `assessments[]` 中的 `task_id`、`profile_hash`、0–100 整数 `relevance_score`、非空中文 `reason_zh`、`evidence[{field,quote}]`。有主题或关键词的任务需要相关性；纯作者/分类可以只翻译，必须明确期望空 assessments 并仍校验完整译文。

每个请求中的相关性任务恰好返回一次，不允许未知、重复或遗漏任务；profile 与请求一致。证据 field 只允许 title/abstract，quote 必须在对应原文连续出现，仅做空白规范化。拒绝浮点分、伪造片段、未支持版本、不完整 JSON、模型新增链接/状态命令。论文内的提示注入只作为数据，不执行工具、不改变发现、下载或任务状态。

必须等待 provider 的正式完成信号，再解析并提交。严格结果校验失败保留候选、消耗已发请求、记录失败原因。整体分析期限 5 分钟，取消关闭所属传输并等待清理，不把已有流片段当可用分析。

### 8.3 缓存与排序一致

缓存身份为稳定 arXiv ID、规范正文版本/摘要、排序后的请求任务 ID/profile 集合、实际服务/协议/URL/模型/effort 指纹、promptVersion 与 resultVersion 的 SHA256。成功结果拥有译文与任务判断，任务分数只是可重建投影。

支持按有效正文/模型复用翻译、按对应 profile 复用判断；合并或拆分任务请求不会无条件丢弃可复用片段。仅版本号变化而正文一致可复用。标题/摘要、任务语义、实际模型/effort、协议/端点、prompt 或结果格式变化使相应结果失效。所有列表 SQL、详情和队列使用同一有效性函数与查询条件，不能旧分数排序而详情显示新材料。

### 8.4 原子额度与发送边界

默认每日 100 次，单位为应用发给 CLI/provider 的分析请求，不是论文篇数或供应商账单。缓存不计，已发重试/拆分分别计，已发失败、取消、超时或无效结果均计。

流程为：准备配置/输入 → 取得可取消门禁 → 数据库事务预占 → 再查取消/token/日期 → attempt 进入 dispatching → 真实协议发送通知 → sent → 收结果并结算。额度检查为当日 `sent + reserved < 最新daily_limit`；降低额度不退已用且不允许新增超发。

发送点必须位于 API transport 发出请求，或 Codex/ACP prompt 写入、Claude prompt 交付的 adapter 内，不能用启动 CLI 子进程代替发送。模型目录/鉴权准备等未交付 prompt 的失败不计。预占跨 UTC 午夜尚未发送时，事务释放原日预占并重新获取新日额度，才能发出；已发结果跨日归发送所属日，释放旧预占不扣新日。

数据库和外部发送无法恰好一次原子化：崩溃发生在 dispatching 的不确定记录保守计为已用并标明不确定，不自动退款；能证明 reserved 未交付的恢复释放。该差异是避免重启超额所需的明确策略，不能声称已精确核对供应商收款。adapter 必须给出“未交付”与“可能已交付”的失败类别。

按请求身份最多自动尝试 3 次，前两次分别退避 30/60 分钟。手动分析可解除失败退避，不能绕过能力、启用任务、额度或门禁。

候选采用持久 keyset 游标，每轮检查最多 200、实际批次最多 5；这些不是候选总量上限。长期最新/历史份额 70%/30%，空队列可借份额；任务权重 5:4:3:2:1，任务和通道轮转位置持久化。共享候选合并相同材料请求时记录参与任务并结算公平份额，验证低优先级任务在持续高优先级压力下也能推进。

## 9. 下载、正式入库与恢复

### 9.1 固定请求与严格下载

用户选择时固定 arXiv ID、版本及 `request_id`，同论文只允许一个活跃请求；批量串行，失败继续其他条目。重新点击返回原请求，新请求不能被旧响应的结束回调释放。新版本不自动替换用户已读 PDF，需要新的明确选择，作为不同文件保留。

不能直接使用现有 `Resolve`：它可能在版本 PDF 失败后退回无版本地址，且可以访问其他公开来源。新增 `DownloadArxivVersion`，只接受目标确定的 stable ID 与版本；版本失败直接失败，不切到 latest。

下载流式写入 `cache/paper-collection/<request-id>/` 的临时文件，禁止先把整批 PDF 放入内存。允许 HTTPS、标准端口、`arxiv.org`/`www.arxiv.org`/`export.arxiv.org`，每一跳重新检查且最多 5 次；检查实际连接目标/解析 IP，防止本机和私网目标。代理场景仍校验请求和每跳 URL，不能声称能检查代理内部 DNS。测试私有地址只通过显式测试 transport，生产不能由请求启用。

连接 10 秒、接收空闲 30 秒、整体 5 分钟；最大 `50 << 20` 字节，按 Content-Length 和实际读取双重限制。拒绝 HTML、空文件、错误 PDF 魔数、禁止重定向及完整性失败，复用现有页数边界。缓存复用核对相对路径归属、拒绝链接逃逸、文件大小、魔数和实际完整 SHA256，不能只信数据库值。

确认是有效文件后进入 downloaded/awaiting_confirmation。进入正式提交前可取消；提交开始后只能停止后续条目，当前提交保持 owner 至真实结束。

### 9.2 共享文件日志与数据库提交

现有 `importFile` 使用缩短 SHA256 ID，在文件移动后提交数据库。提取公共提交流程并保留已有 API 返回语义，新增完整 SHA256 核验及文件安装日志；需要核对全部普通上传、在线 resolve、Zotero、采集调用者。mutex 不嵌套，由最外层取得共享书库写入权限，底层 helper 接收明确持锁上下文。

1. 校验并计算完整 SHA256；下载原件保持可恢复，正式 staging 放同一数据根以支持同卷 rename。
2. 在日志事务记录 prepared、目标路径、完整 SHA256 和来源 owner。重复已有文档时核验真实文件摘要，不只比较缩短 ID；不同完整摘要冲突拒绝覆盖。
3. 共享写入锁内安装正式文件，必要时完成文件与目录持久化；日志记录 installed。已有非本请求登记文件不能覆盖或清理。
4. 同一数据库事务提交文档、来源元数据、处理队列、采集关联和日志 committed。元数据保留手动值，源原文与中文分析结果分开。
5. 查询正式文档并确认文件摘要/归属后报告成功；成功后才清理所属 staging。入库成功不要求版面学习/翻译完成，处理状态继续沿用封面阶段 UI。

启动处理 worker 前对账开放日志：有文件无记录时，核验 owner/摘要后恢复提交或保留可重试；有完整已提交文档时补检查并结束，不重复创建；有文档无文件时只用经验证的同摘要缓存恢复，保留笔记与阅读状态，否则标记 missing。恢复中清理只针对归属明确的临时文件，不删除未知原文件。

### 9.3 阅读身份与原状态

重复导入同一字节文件返回既有 Document，保留库位置、手动元数据、标签、分类、笔记、批注、聊天及阅读进度；回收站恢复需明确用户决定。不重复重置译文或后台处理任务。

打开入库记录时核验 Document 存在、库状态、类型、实际完整摘要与文件；完整 SHA256 唯一匹配可修复丢失关联。虽然当前 ID 为内容派生字符串，数据库替换/损坏仍不能只信 ID。歧义匹配要求人工选择，不猜测。

用户显式合并不同版本时，在既有 merge 事务记录对应采集导航目标为保留 Document，并区分“原下载摘要”与“用户选择的合并后文件摘要”。旧文件与批注仍在回收站；不得宣称文件摘要相同。移库跟随实际 Document 所在库，移入回收站显示已移除，彻底删除只断开关联，不删除候选或自动再下载。

## 10. 生命周期、迁移、清缓存与回滚

### 10.1 生命周期 owner

`CollectionManager` 由 Go 服务持有，在 Store 可用、共享恢复完成后启动；首次 tick 后每分钟检查到期任务。切换页面、折叠和最小化不终止工作；桌面退出停止；`pnpm dev` 下浏览器关闭但 Go 仍运行时工作继续。不是操作系统后台服务。

同任务自动/手动运行通过数据库开放运行唯一约束与进程 check-and-set 共享一次执行；手动发现可运行关闭任务，自动发现与分析只取启用任务。手动不改变原自动计划。暂停停止新工作并取消可取消执行，保存已提交页面，任务开关不批量改写；恢复从持久待办继续。

运行状态为 queued/running/succeeded/partial/failed/cancelled/interrupted；切片为 pending/running/completed/failed/interrupted；分析为 pending/running/succeeded/failed/interrupted/blocked；导入为 queued/downloading/downloaded/awaiting_confirmation/importing/imported/failed/cancelled/interrupted。支持状态必须都有实际写入与消费者，不能仅定义枚举就宣称完成。

### 10.2 桌面与 Go 的关闭握手

新增鉴权 `POST /api/lifecycle/drain` 和状态查询。窗口关闭先保存阅读数据，再请求 Go 关闭新入场、停止计时器、取消来源/分析/下载、等待已有文件提交及所有后台资源清理。只有 drain 成功且数据库写入结束，Electron 才让服务正常退出；不能以当前 `child.kill()` 代替排空，Windows 也使用此 HTTP/IPC 确认路径。

HTTP shutdown、collection stop、processing stop、chat session close 和 Store close 的顺序由一个 owner 管理，避免重复 WaitGroup 或数据库已关后保存 interrupted。drain 用短 HTTP 请求启动并轮询状态，长写入不依赖 renderer 请求连接。以 drain ID 幂等执行 running→quiescing→drained；状态查询不能重复关资源。超时返回 busy，保留进程/连接和请求 owner，用户可继续等待或返回应用；返回应用时撤销本次退出意图，保留原活跃提交，待其排空后用新 Context 重建已取消的调度并重开入场，不复用取消 Context。普通“仍然退出”的阅读 flush 决定不能覆盖尚在提交的后台写入。

启动恢复提高执行 token、标记开放运行 interrupted、恢复切片/引用/分析待办、保守结算发送不确定额度、对账文件日志与关联，再恢复调度。异常 OS 强制退出只能依靠持久日志恢复，不声称正常排空已发生。

### 10.3 存储与清理

复用现有 `importMu` 并增加可等待的书库写入入场控制，普通导入、在线导入、Zotero、合并、彻底删除和未来根目录迁移均使用同一互斥边界。普通查询与读书不持此长锁；网络阶段不持锁，SQL 事务不持有外部等待。

本期不新增在线主库替换或根迁移 UI。已有手动搬迁必须先正常关闭，复制整个数据根（含新表、正式文件与相关配置），重开核验；任何将来在线迁移先阻止新工作、排空、关闭数据库、复制并校验，然后重开及失败回滚。多库实例隔离不能只靠路径字符串，需处理别名路径指向同一根的风险。

模块清下载缓存仅清理归属明确且非活跃导入的临时文件，记录变为可重新下载；保留任务、候选、分析成功结果、额度、导入日志和正式 PDF。不得递归清空整个 `cache/`，其中有阅读处理产物。更一般的缓存清理入口若尚未存在，不额外扩展范围。

### 10.4 回滚

先停止新采集并完成排空，再回滚应用；保留新增表和任务，旧 reader 查询已有表，正式 PDF 按普通 Document 继续阅读。共享导入/关闭改造回滚前检查无开放日志与新状态，不能运行不会对账的旧服务处理未完成提交。

不自动降级或删模块表；版本不兼容禁用采集，正常阅读仍可用。需恢复迁移前数据库时必须使用匹配正式文件的完整一致备份，不能只恢复 SQLite 造成后续新增资料消失。本阶段未修改或迁移实际用户数据。

## 11. 方案取舍与明确差异

| 项目 | 分类 | 目标提案与理由 |
| --- | --- | --- |
| 高召回、作者独立、双窗口、原始先保存 | 沿用 | 保留来源合同，不由 AI、引用或额度决定是否保存 |
| Flutter/Riverpod/WebView | 等价适配 | Go manager + React/Zustand + 现有 PDF 处理；无共享 WebView 解析资源，不引入它 |
| 独立采集 SQLite | 等价适配 | 同库前缀表可事务关联 Document；仍需文件日志处理断电 |
| MD5/整数书籍 ID | 等价适配 | 保留现有 Document ID，另存完整 SHA256 核验文件；合并显式记录导航目标 |
| 正文 JSON 型 content_hash | 等价适配 | 新表定义 canonical v1 + SHA256，无迁移源库字段的消费者 |
| 参考仅 Windows | 产品变更 | 目标 Windows/macOS 共用 Go/React 路径；两个正式平台分别验证，Linux 仅开发可运行性，不承诺安装包 |
| 100 MiB 下载上限 | 产品变更 | 使用目标 50 MiB/150 页边界，51–150 页确认；不扩大既有论文库范围 |
| API AI registry 与模型覆盖 | 等价适配 | 复用目标 CLI/API 配置，新增真实采集 task 和系统提示词；不新增 Gemini 服务或读取凭据 |
| 已有 AI 就自动分析 | 产品选择 | 模块分析需显式启用，避免配置阅读 AI 自动触发新用途；每日默认 100 次保留 |
| 请求精确计数 | 等价适配 | 真实发出按次；外部发送与本地落账的崩溃不确定窗口保守计量，并显示不确定状态 |
| 模型备用与重试 | 等价适配 | 采集禁隐式备用，显式计量重试；阅读现有合同保持 |
| 全文翻译 | 沿用目标 | 入库后按 `autoTranslatePDF`；与采集额度分开显示，不把每日 100 解释为全部 AI 上限 |
| 24 小时频率、7 天刷新/24 小时重试、100/50 页、70/30、权重 | 沿用 | 实施时逐项接入设置与执行消费者，来源限制再次核对 |
| 云同步、主库在线替换、后台系统服务 | 不在范围 | 目标没有已确认入口，不为迁移引入 |
| 作者消歧、近期引用增长、全文自动研究、其他自动来源 | 不在范围 | 不从参考保留字段或未落地 UI 推断需求 |
| 完整独立下载队列/运行审计浏览器 | 不在范围 | 页面提供逐项持久状态、取消后续与失败重试，并落实恢复合同；不增独立管理页 |

考虑过的替代方案：独立数据库会增加当前并不存在的双库对账和备份成本；将候选直接存为 Document 会污染书库与后台解析；用 React 计时器在路由间调度会丢失运行 ownership；直接调用 `Resolve` 无法守住版本和来源；直接调用默认 generation 会引入备用、阅读提示词与计量漏洞，因此均不采用。无需数据库服务器、消息队列或分布式 worker；未来多用户/远程部署必须重新设计身份、租约、共享额度与文件权限，不能沿用本地锁作为证明。

主要成本是共享导入与退出路径需要真实故障测试，来源限流使初次热门回溯可能较慢，CLI 结构化输出需要严格拒绝与退避。用户仍能在等待统计或分析时查看原文和入库。

## 12. 分阶段实施与完成条件

实施业务改造时，先从干净基线建立专用 worktree/分支。每阶段按生产行为成组提交，API、生成类型与紧耦合测试同提交；不按目录机械拆分。源项目只读。

| 阶段 | 改造内容 | 阶段完成条件 |
| --- | --- | --- |
| P0 共享安全基础 | 同库增量 schema、模块版本；共享导入日志与结果；write admission；Electron/Go 排空握手 | 已有上传、resolve、Zotero、阅读 flush 回归；延迟提交/异常退出不丢资料；不必等待完整 UI 才能验证 |
| P1 最小闭环 | 单任务手动发现、原文候选列表/详情、稳定身份、人工固定版本下载/入库 | 无 AI/无引用 Key 实际完成“建任务→发现→选中→论文库→阅读”；当期 UI 接生产 API |
| P2 完整发现与恢复 | 双窗口、独立作者、切片/分页、覆盖、自动频率、暂停/重启、延迟索引和历史复核 | 第二页失败、取消、窗口修改、来源超量都能恢复且不假报成功 |
| P3 引用 | 精确 batch、共享来源门禁、NULL/零/过期、累计引用排序 | 查询与统计失败不会影响候选或下载；真实 API 单独记录 |
| P4 AI | 独立系统提示词/会话、完整 JSON 校验、真实 attempt 计量、原子额度、缓存与公平队列 | 跨日/重启/降额/并发不超发；与阅读聊天互不取消；真实 CLI/API 单独验证 |
| P5 完整交付 | 表单/筛选/设置、部分失败重试、浅深色/系统主题、Windows/macOS 验收与打包文档 | 验收矩阵全部要求达到对应证据；正式构建/安装包/人工 UI 各自记录 |

P1 是可以使用的首个闭环，不等同完整参考合同已经完成。完整交付需 P0–P5；缺 AI 账号、官方来源可用性或 macOS 机器时保留对应 blocked/not run，不能以 fixture 通过替代。

## 13. 验证、设计假设与交付边界

默认采用目标现有单用户、每库单服务、桌面运行期调度，不引入系统服务；支持目标正式 Windows/macOS 平台；自动发现仅 arXiv；分析复用现有配置且显式启用；保留现有文件限制与入库后自动翻译设置。这些是本方案的明确产品建议，没有阻碍完成设计的缺失信息。

实施前若用户选择扩大文件限制、应用退出后仍采集、新 AI 供应商或任务跨设备同步，需要分别更新产品边界和数据/权限合同；这些不能作为常规实现选择悄悄加入。

本次证据为参考文档、目标源码/依赖/Git/现有设计的只读核对，以及官方 arXiv 条款与 Semantic Scholar 教程的资料核验。Semantic Scholar 动态 API reference 在检索工具中未完整展开，batch 参数与外部身份字段需在实施时取得完整官方 schema 并验证。未运行真实来源、模型请求、应用自动测试、构建、安装或 UI 验收。

后续最小验证命令：

```powershell
pnpm api:generate
pnpm typecheck
pnpm test
pnpm build
git diff --check
```

Go 侧补充生产路径的 SQLite/HTTP/子进程并发与故障注入测试，必要时在支持的工具链下运行 `go test -race`；真实 API 为 opt-in。UI 按项目规定提供 `pnpm dev` 或桌面预览和人工步骤，不使用浏览器自动化作视觉验收。所有诊断脚本、fixture 运行输出和日志按工作区规则归档，正式测试与版本化夹具进入项目测试目录。

本文与验收矩阵是 proposed 设计；没有新增运行能力，没有复用源项目旧测试数量或通过结论，没有更新构建与安装入口。
