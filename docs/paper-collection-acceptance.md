# 论文采集验收矩阵

关联设计：[论文采集改造方案](paper-collection-adoption.md)。状态：implementing；2026-10-12 起在 feat/paper-collection 实施（见 .agent-handoff 轮次报告）。下表状态按执行证据更新；多数仍为 not run，不得据入口名称推断功能完整。下表“生产入口”为拟实现的消费者，所有行为初始状态均为 `not run`，不能根据入口名称推断功能已存在。

执行时将状态改为 `passed / failed / blocked / not run`，补充提交、命令、退出结果、目标平台、隔离数据与证据路径。自动测试使用项目正式测试目录和构造数据；临时脚本/日志放任务归档。真实账号和正式用户书库不得作为普通 fixture。

| 编号 / 行为 | 拟生产入口 | 失败或中断场景 | 最小证据层级 | 验证方法与预期 | 状态 |
| --- | --- | --- | --- | --- | --- |
| A01 作者独立与可执行条件 | 任务表单 → query builder → discovery | 作者论文不命中关键词/分类；只填自然语言主题 | 单元 + HTTP/SQLite 集成 | 检查真实请求，作者只带 au 与日期；主题分支独立；仅主题不能保存；任务并集保存 | passed（round-1：TestCollectionAuthorBranchIsIndependent、TestCollectionTopicOnlyTaskRejected；多分支并集仅代码路径未单测） |
| A02 双窗口与首次日期 | manager → slices → 列表 API | 月末/闰年、版本刚更新但首次提交很早 | 单元 + 集成 | 固定时钟验证 30 天/12 日历月；使用 published 而非 updated；两个通道覆盖才成功 | not run |
| A03 稳定身份与版本 | Atom parser → commit page | 新旧 ID、v1/v2、跨任务/作者/latest/hot 重复 | SQLite 集成 | 一候选、多发现关系；版本单存；完整原文及依据不丢 | not run |
| A04 第二页失败与原子提交 | commit page | 第一页成功，第二页 503 或事务中途失败 | 真实 SQLite + HTTP fixture | 第一页保留；失败页无半页候选/关系/断点；运行 partial；重启第 0 页复扫去重 | not run（部分证据：TestResumeMustFetchFinalPage 失败页无半页+重启复扫去重；TestScanPageRejectsCancelledRunToken 断点门禁；run partial 与事务中途断电边界归 A28） |
| A05 超量细分与来源上限 | slice planner → discovery | 多日超 1,000、分钟仍超量、执行预算耗尽 | 单元 + 集成 | 子片完整且更小；source_limit 或预算待办不能成功；不是候选总量上限 | not run（部分证据：TestSplitSliceRangeBuckets 子片精确覆盖、TestOversizedSliceSubdividesAndCovers 超量细分；source_limit 全链与预算耗尽中断待补专测） |
| A06 空页与总量漂移 | discovery → slice state | 总量应有数据但空页、HTTP 200 错误 entry、页间新增/减少 | HTTP + SQLite 集成 | 错误分类与重试持久化；重开切片；明确总量零正常完成；异常页不推进覆盖 | not run（部分证据：TestPositiveTotalEmptyPageCannotComplete、TestInvalidPublishedEntryCannotComplete、TestMixedPageKeepsValidButNotComplete、TestBadIDEntryCannotComplete、TestMissingFieldsEntryRejected（U02，bad-entry 重试持久化）；总量零/漂移重开待补专测） |
| A07 窗口/修订变化 | task update → manager → SQL query | 扩大、缩窄、改词；旧响应迟到 | 集成 | 扩大补缺口复用覆盖；缩窄不调度不相交旧待办；新修订不使用旧排序关系 | not run（部分证据：TestHotHistoryRevisionChangeRebuildsQueue、TestHotHistoryNarrowedWindowDropsNonIntersecting 覆盖复核队列的修订/窗口重建；正常运行窗口扩缩与旧响应迟到待补） |
| A08 重复点击与取消 ownership | run/stop API → manager | 自动与手动同时、暂停后旧响应、旧 end 回调 | 并发集成 | 每任务一开放运行；执行 token 拒绝旧响应；旧回调不释放新 owner；已提交页保留 | not run（部分证据：TestDoubleConfirmClaimsOnce、TestImportCancelIsTerminalAndOwned、TestCancelCannotClaimCommittingImport、TestScanPageRejectsCancelledRunToken、TestStaleDrainResumeCannotReopenNewerDrain；开放运行唯一索引仲裁含于 TestHotHistory 短命 run） |
| A09 延迟索引与热门复核 | persistent cursors → scheduler | 补扫中退出、复核周期只完成一部分 | 重启集成 | 最近 7 天复扫；热门待重开列表跨重启继续；整周期完成才推进标记 | passed（2026-10-13 Windows：TestCollectionHistoryCrashRestartAndClock，独立进程持有真实库锁，生产启动恢复/worker/tick 路径、TLS fixture 和固定时钟，杀进程后续跑同一周期；单轮 10 片、完整周期及七日期限均有断言。TestCollectionM1DelayedIndexAppearsOnNextRun 经运行 HTTP 入口证明延迟候选补入，TestCollectionM1AllBranchesAreRechecked 证明全部作者分支；原 HotHistory/ForcedRescan 回归通过） |
| A10 公共来源限流与代理 | source gate + 现有 metadata lookup | 预览/发现/查找并行，不同根的两个进程；429/Retry-After；代理启用 | 多进程集成 + 实际代理 | 同一用户 arXiv 请求单连接且间隔 ≥3 秒；跨根共享；退避可取消；真实 transport 使用代理；不声称覆盖其他机器 | not run |
| A11 引用精确身份 | citation queue → S2 batch | 返回顺序改变、部分 null、external ID 不符 | HTTP + SQLite 集成 | 按 externalIds 匹配；错误身份不更新；不用标题替代；候选可见与下载不受影响 | passed（round-1：TestCitationBatchMatchesByExternalID；真实 S2 仍 not run） |
| A12 引用零/未知/过期 | citations → 列表排序 | 0、缺数、负数/浮点/字符串、旧值刷新失败、成功下降 | 集成 + 前端行为 | 0 显示 0、未知 NULL、旧值过期保留；7 天成功刷新/24 小时失败退避；接受下降 | failed（round-1 误标 passed 已纠正；round-2 修复首败退避并有 TestCitationZeroUnknownAndStale、TestCitationFirstFailureSetsBackoff；前端展示与真实调度证据缺失） |
| A13 热门与相关性语义 | latest/hot list API → UI | 未知引用、低分/无分、仅关闭任务命中 | SQL + 前端测试 | 按首次窗口及累计引用；未知保留；有效相关性独立排序；原文和任务筛选仍可用 | not run |
| A14 无 AI 闭环 | collection UI → import → PDF reader | 没 CLI、没 Key、额度为零、统计服务离线 | 生产 HTTP 集成 + 人工 UI | 创建作者/关键词任务、发现、查看原文、选择入库、打开 PDF；失败统计/分析不删除候选 | not run（round-1 后端闭环已测 TestCollectionManualDiscoveryLoop、TestCollectionCandidateImportFlow；人工 UI 未做） |
| A15 不可信原文与严格 JSON | paper analysis adapter → validator | 假 quote、未知/重复/漏任务、错 profile、浮点分、注入命令、半流无终态 | 单元 + 协议集成 | 全部拒绝；不写成功与分数；已发送计量；无工具、下载或状态执行 | not run（2026-10-13：部分证据：AnalysisWireRejectsMissingNullAndDuplicateFields、ValidateAnalysisResultStrict、PaperAnalysisInvalidJSONCountsWithoutScore；原生注入/半流取消组合待补） |
| A16 translation-only 与输入预算 | input builder → native attempts | 纯作者/分类；多任务需拆；基础材料超 24,000 字符 | 单元 + 集成 | 空 assessments 有明确合同且译文完整；拆分按实际请求计；基础过大不截断、零发送 | not run（2026-10-13：部分证据：AnalysisUnicodeBudgetUsesCharacters、BudgetCountsSerializedEscapes、TranslationOnlyRejectsJudgments、BuildAnalysisInputs；生产拆分计量及纯作者待补） |
| A17 缓存失效与展示一致 | cache → SQL projections → detail | 正文/profile/模型/effort/prompt/schema 变化；只升版本号 | SQLite + 前端测试 | 对应变化使结果失效，列表和详情一致；正文未变版本可复用；缓存零计量；部分判断可复用 | not run（2026-10-13：部分证据：PaperAnalysisProductionAPIAndCache、ChangedIdentityRemovesStaleScores、ReusesIndependentJudgmentsAcrossTaskSets；effort/prompt/schema/前端待补） |
| A18 分析与聊天/翻译隔离 | taskPaperAnalysis → CLI/API sessions | 三类同时执行，逐个取消与超时，已有聊天缓存 | 真实适配器协议集成 + 各服务真实账号 | 不共享 conversation/临时目录/取消器；取消分析不毁聊天与翻译；不串上下文或污染聊天能力 | not run（2026-10-13：部分证据：PaperAnalysisNativeCLIAdapters 在三种受控本地进程中走生产 HTTP/native prompt 写入；并行取消和真实账号待补） |
| A19 禁隐式备用与隐藏重试 | generation → native dispatch | 自动模型失败、text-api 配置存在、输出部分 JSON 后失败 | 协议集成 | 采集不切备用、不产生未计 native 重试；只由采集调度重试；聊天原冻结 SSE 合同回归 | not run |
| A20 原子额度与降额 | quota transaction → dispatch hook | limit=1 并发手动/后台、额度降低、pre-send 失败/取消 | 真实 SQLite 并发集成 | sent+reserved 不超额度；未交付释放；已发失败/取消计一次；每次重试独立 attempt | not run |
| A21 UTC 跨日与崩溃计量 | attempts → startup recovery | 23:59 预占后次日发送、旧日释放、dispatching 断电、发送后未结算 | 可控时钟 + 服务进程重启 | 未发跨日需重新预占；旧日释放不扣新日；不确定发送保守计用并可查状态；重启不退款超发 | not run（2026-10-13：部分证据：AnalysisDispatchRechecksDayAndLoweredLimit、StartupSettlesAttemptsExactlyOnce、UnknownDispatchCannotBeRefunded、UnknownDispatchSettlesAsSent；PaperAnalysisProcessCrashMetering 已通过真实隔离子进程在 dispatching/sent 两点退出、两次生产启动对账，任务 interrupted、计量不重复、不超发。UTC 跨日完整生产发送组合待补；不等同物理断电证据） |
| A22 共享生成门禁与公平 | admission + persistent scheduler | 历史大量积压，高优先级持续新增，多个入口排队 | 确定性调度 + 生产集成 | 全发送路径共用原子门禁；70/30 和 5:4:3:2:1 长期推进；200/5 不是上限；取消排队不发请求 | not run（2026-10-13：部分证据：PaperAnalysisBackgroundKeepsWeightedTasksMoving，生产 scheduler/API 6 轮、每轮 5 论文、30 次任务推进 25:5、持久 keyset；70/30/完整权重/共享候选/取消等待待补） |
| A23 固定版本与重复导入请求 | explicit import API → strict download | vN 不存在、来源 vN+1 更新、重复点击、旧响应结束 | HTTP + SQLite 集成 | 不退回 latest，不自动换版本；一活跃请求；已读文件不被自动替换 | not run |
| A24 错误 PDF 与禁止目标 | download transport → validation | HTML、空/截断/超长、非 HTTPS/私网/非白名单跳转、超过 5 跳 | HTTP/文件集成 | 拒绝且不创建正式 Document；清理所属临时文件；代理可见性边界明确 | not run |
| A25 目标文件范围 | download → paperLimit → confirmation UI | 50 MiB 边界、51/150/151 页、页数读取失败 | 集成 + 人工 UI | 50 MiB/150 页上限；51–150 需确认；无法读页数沿用仅检查大小；不静默导到图书库 | not run |
| A26 缓存归属与篡改 | downloaded cache → import recovery | 目录穿越、symlink/junction、摘要改动、文件缺失 | 文件系统集成，Windows/macOS | 不复用越界/篡改文件；核验完整 SHA256；保留正式文件；可重试下载 | not run |
| A27 延迟提交与整批部分失败 | write admission → import commit | 一个下载失败、数据库迟迟提交、点击停止、响应断开 | 生产 HTTP/SQLite/文件集成 | 一项失败不终止整批；提交前可取消；提交后 owner 等到真实结束；成功仅在文件及关联提交后 | not run（部分证据：TestCancelCannotClaimCommittingImport 提交后 owner 保持、TestImportCancelIsTerminalAndOwned 提交前可取消；整批部分失败专测待补） |
| A28 文件/数据库崩溃恢复 | import_commits → startup recovery | prepared/installed/DB commit 各边界杀隔离服务、同名未知文件 | 服务进程故障集成 | 恢复无重复、不覆盖未知文件；同库关联原子；缺正式文件仅用核验缓存修复；日志与文件一致 | passed（2026-10-13 Windows：三项 TestProcessCrash* 使用仅测试构建启用的故障点，真实 reader-server 退出后重启；TestCollectionM1CommittedMissingFileRepairsAtStartup、CommittedChangedFileCannotReportImported、ExistingDocumentFileIdentityIsChecked、RemovedDocumentIsNotRecreated，以及原 Recover* 回归通过。进程终止证据不代表物理断电或其他平台 fsync 验收） |
| A29 原有资料保护 | importFile/Zotero/merge/trash → collection link | 同 PDF 已有笔记/标注/进度、在图书库/回收站、合并不同版本 | 真实 SQLite 与文件回归 | 原状态保留；不自动移库/恢复/重翻译；显式合并分开记录历史摘要与目标摘要 | not run（部分证据：TestRecoverNeverRestoresTrash、TestDuplicateCollectionImportPreservesExistingFile、TestInstallFailureKeepsReusedDest；图书库位置与合并版本专测待补） |
| A30 Document ID 改变或不匹配 | open imported → reconciliation | 模拟主记录换 ID/错误占用、同摘要多个记录、实际 PDF 不符 | SQLite + 文件集成 | 只按完整摘要唯一匹配修复；歧义不打开；不能仅信内容派生 ID；显式 merge 目标有依据 | not run（部分证据：TestRecoverMismatchedTargetKeepsUnknownFile 拒绝不符摘要、TestImportFullDigestConflictRejected 摘要冲突拒绝；ID 变化/歧义匹配待补） |
| A31 模块 schema 与主库升级 | OpenStore → collection migration | 旧库、损坏 JSON、高版本、迁移中失败 | 真实 SQLite 集成 | 增量迁移保留已有阅读数据；高版本禁写不重建；失败回滚；不挪用主库所有 schema 的版本 | not run（部分证据：TestCollectionMigrationCreatesModuleTables、TestCollectionMigrationIdempotentAndPreservesData、TestCollectionSchemaTooNewDisablesWrites；游标/恢复写入已接 collectionWritableTx，高版本下其拒绝专测待 M2 补） |
| A32 关闭、超时与跨平台 | Electron close → drain → Go/Store stop | 下载/分析并行、保存超时、Windows child kill、无 renderer | 进程集成 + Windows/macOS 人工 | 关新入场、取消可取消请求、等真实写入；未排空不关 DB/杀服务；正常退出后重开可恢复 | not run（部分证据：TestDrain* 系列、TestCancelWhileDrainStopInitializes、TestStaleDrainResumeCannotReopenNewerDrain、TestProcessCrash* 重开可恢复、恢复先于 worker 的 main 接线；Electron 实机与 macOS 人工未做） |
| A33 迁移/缓存/备份边界 | write admission + 模块缓存清理 | 活跃下载、提交与缓存清理竞争、关闭后复制数据根 | 文件/数据库集成 + 人工 | 清临时缓存保留任务/额度/成功分析/正式文件/阅读产物；移动后相对路径有效；不新增未授权在线迁移 | not run |
| A34 设置与秘密保护 | collection settings → existing config owner | Key 保留/清除、服务删除、设置快照竞争、未启用分析 | HTTP 集成 | GET/日志/报告无 Key；额度不被旧快照覆盖；无配置待分析；不隐式换服务；仅阅读 AI 不自动触发采集分析 | not run |
| A35 全文处理与采集计量分离 | import → initialDocumentProcessing → UI | autoTranslatePDF 开/关；新旧文档重复入库 | 集成 + 人工 UI | 遵循原开关、界面告知；全文请求不计采集额度且有原 usage；关闭 AI 仍能入库阅读 | not run |
| A36 本地权限与调用合同 | authenticated collection API | 无 token、错误 Origin/Host、猜候选/路径、超量请求体 | HTTP 安全集成 | 复用现有 401/403；API 不接受任意下载 URL/绝对路径，不扩大 loopback 绑定 | not run |
| A37 真实来源 | scholar production transport | arXiv Atom / S2 真实未收录、限流、动态字段变化 | opt-in 真实 API | 校验真实 ID/日期/状态/重定向，不断言固定引用数；与 fixture 证据分别记录 | not run |
| A38 完整 UI 与阅读回归 | App / collection UI / Settings / existing readers | 路由切换、浅深色/系统、窄窗口、长标题、reduced motion、键盘 | React 行为测试 + 人工桌面/开发预览 | 页面使用生产 API；切页任务继续；表单错误可纠正；主题与可访问性符合既有 UI；PDF/EPUB、聊天正常 | not run |
| A39 正式构建与安装 | pnpm scripts + package pipeline | 生成类型漂移、缺 Go/Web/processor 资源、安装后重启 | typecheck/test/build + Windows/macOS 安装 | 执行真实命令；包包含采集生产代码；人工无开发依赖闭环；源测试不替代包验收 | not run |

## 验证执行顺序

先验证 A27–A32 的导入/数据/关闭边界，再验证无 AI 闭环 A01–A08、A14、A23–A26；接着验证引用、完整恢复、AI/额度/公平；最后真实来源/模型、人工 UI、正式构建与安装。每阶段复用仍有效证据，新修改触及 owner 时重跑相关条目。

项目检查为 `pnpm typecheck`、`pnpm test`、`pnpm build`；API 变更后执行 `pnpm api:generate` 并检查生成结果。测试不访问正式 `.reader` 或已安装书库，不读取 CLI 凭据；真实模型验证仅使用已登录 CLI 的正常调用且单独记下实际请求数量。

## 人工预览与验收步骤

1. 实施完成后启动 `pnpm dev`，使用终端给出的完整本地地址；或启动隔离开发书库的桌面预览。不要分享带 token 的地址。
2. 无 AI 创建一个关键词任务和一个纯作者任务，手动运行，核对最新/热门日期、作者独立结果、原始摘要和未知引用。切换到读书再返回，确认任务仍存在并推进。
3. 选择两篇正常 PDF 和一篇下载失败样例，点击下载入库；核对逐项结果、失败重试、已有文档去重和超过 50 页的确认。
4. 打开已入库论文，添加高亮、笔记及进度，再重复选择入库；核对内容不丢失。新版本不得自动替换。
5. 显式启用采集分析，核对完整中文译文、任务依据和原文 quote；同时发送阅读聊天并分别取消，确认会话独立。
6. 暂停/恢复，关闭再打开，核对运行断点、热门复核、导入日志和额度。使用受控延迟提交验证关闭等待，不能对正在使用的用户程序强杀。
7. 检查浅色、深色、系统外观、最小窗口、长标题、键盘操作和 reduced motion；随后在 Windows/macOS 正式安装包分别执行闭环。

按项目要求，以上视觉验收由人工完成，不用浏览器自动化截图替代。HTTP fixture、React 测试、真实 API、真实 CLI、人工 UI、构建、安装各自独立记录；缺平台或账号应标 blocked/not run。
