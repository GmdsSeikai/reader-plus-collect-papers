import { useCallback, useEffect, useRef, useState } from "react";
import {
  CirclePlus,
  Download,
  Loader2,
  Play,
  RefreshCw,
  Search,
  Settings2,
  X,
} from "lucide-react";
import { toast } from "sonner";
import { Button } from "@reader/ui/components/button";
import { Checkbox } from "@reader/ui/components/checkbox";
import { Input } from "@reader/ui/components/input";
import { Badge } from "@reader/ui/components/badge";
import { Tabs, TabsList, TabsTrigger } from "@reader/ui/components/tabs";
import { ScrollArea } from "@reader/ui/components/scroll-area";
import { Separator } from "@reader/ui/components/separator";
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from "@reader/ui/components/select";
import {
  collectionApi,
  type CollectionCandidate,
  type CollectionCitation,
  type CollectionRun,
  type CollectionTask,
  type CollectionTaskConfig,
  type CollectionAnalysis,
  type CollectionAnalysisJob,
} from "@reader/api";
import { TaskForm } from "./TaskForm";
import { AnalysisPanel } from "./AnalysisPanel";
import { CollectionSettingsDialog } from "./CollectionSettingsDialog";

type CandidateRow = { candidate: CollectionCandidate; score: number };

function defaultConfig(): CollectionTaskConfig {
  return {
    v: 1,
    research_topic: "",
    keyword_groups: [],
    group_operator_or: true,
    authors: [],
    categories: [],
    not_interested: "",
    frequency_hours: 24,
    latest_window_days: 30,
    hot_window_months: 12,
  };
}

const statusLabel: Record<string, string> = {
  queued: "排队",
  running: "运行中",
  succeeded: "完成",
  partial: "部分完成",
  failed: "失败",
  cancelled: "已取消",
  interrupted: "已中断",
  downloading: "下载中",
  downloaded: "已下载",
  awaiting_confirmation: "待确认",
  importing: "入库中",
  imported: "已入库",
  queued_import: "排队",
};

function CandidateDetail({
  candidate,
  citation,
  score,
  analyses,
  analysisJob,
  taskNames,
  onClose,
}: {
  candidate: CollectionCandidate;
  citation: CollectionCitation | null;
  score: number;
  analyses: CollectionAnalysis[];
  analysisJob: CollectionAnalysisJob | null;
  taskNames: Record<string, string>;
  onClose: () => void;
}) {
  return (
    <div className="flex h-full flex-col">
      <div className="flex items-start justify-between gap-3 p-4">
        <div className="min-w-0">
          <h2 className="text-base font-semibold leading-snug">
            {candidate.title}
          </h2>
          <p className="mt-1 text-xs text-muted-foreground">
            {(candidate.authors ?? []).map((a) => a.name).join(", ")}
          </p>
        </div>
        <Button
          size="icon-sm"
          variant="ghost"
          aria-label="关闭详情"
          onClick={onClose}
        >
          <X className="size-4" />
        </Button>
      </div>
      <ScrollArea className="flex-1 px-4">
        <div className="space-y-3 pb-6 text-sm">
          <div className="flex flex-wrap gap-1.5">
            {(candidate.categories ?? []).map((c) => (
              <Badge key={c} variant="secondary">
                {c}
              </Badge>
            ))}
          </div>
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs text-muted-foreground">
            <dt>arXiv</dt>
            <dd>
              <a
                className="text-foreground underline-offset-2 hover:underline"
                href={`https://arxiv.org/abs/${candidate.arxivId}`}
                target="_blank"
                rel="noreferrer"
              >
                {candidate.arxivId} (v{candidate.latestVersion})
              </a>
            </dd>
            <dt>首次提交</dt>
            <dd>{(candidate.publishedAt ?? "").slice(0, 10)}</dd>
            <dt>版本更新</dt>
            <dd>{(candidate.updatedAt ?? "").slice(0, 10)}</dd>
            <dt>累计引用</dt>
            <dd>
              {citation?.count == null
                ? citation?.stale
                  ? "未知（刷新失败，保留旧值）"
                  : "未知"
                : `${citation.count}${citation.stale ? "（过期）" : ""}`}
            </dd>
            {citation?.successAt ? (
              <>
                <dt>引用更新</dt>
                <dd>{citation.successAt.slice(0, 10)}</dd>
              </>
            ) : null}
            {candidate.doi ? (
              <>
                <dt>DOI</dt>
                <dd>{candidate.doi}</dd>
              </>
            ) : null}
            <dt>相关排序分</dt>
            <dd>{score >= 0 ? String(score) : "待分析"}</dd>
          </dl>
          <Separator />
          <p className="whitespace-pre-wrap leading-relaxed">
            {candidate.abstract}
          </p>
          <AnalysisPanel
            candidateId={candidate.arxivId}
            analyses={analyses}
            job={analysisJob}
            taskNames={taskNames}
          />
        </div>
      </ScrollArea>
    </div>
  );
}

export function CollectionPage({
  navOpen,
  onToggleNav,
  onOpenDocument,
  onOpenReaderSettings,
}: {
  navOpen: boolean;
  onToggleNav: () => void;
  onOpenDocument: (id: string) => void;
  onOpenReaderSettings?: () => void;
}) {
  const [tab, setTab] = useState<"latest" | "hot" | "tasks">("latest");
  const [tasks, setTasks] = useState<CollectionTask[]>([]);
  const [rows, setRows] = useState<CandidateRow[]>([]);
  const [nextCursor, setNextCursor] = useState("");
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [detail, setDetail] = useState<{
    candidate: CollectionCandidate;
    citation: CollectionCitation | null;
    score: number;
    analyses: CollectionAnalysis[];
    analysisJob: CollectionAnalysisJob | null;
  } | null>(null);
  const [taskForm, setTaskForm] = useState<CollectionTask | "new" | null>(null);
  const [runStates, setRunStates] = useState<Record<string, CollectionRun>>({});
  const [query, setQuery] = useState("");
  const [taskFilter, setTaskFilter] = useState("");
  const [citationFilter, setCitationFilter] = useState("");
  const [ignoredFilter, setIgnoredFilter] = useState("0");
  const [listError, setListError] = useState("");
  const [sort, setSort] = useState<"date" | "citations" | "relevance">("date");
  const [busy, setBusy] = useState(false);
  const [settingsOpen, setSettingsOpen] = useState(false);
  type ImportState = {
    requestId: string;
    status: string;
    error?: string;
    documentId?: string;
    condition?: string;
    confirmPages?: number;
    downloadVersion?: number;
    documentState?: string;
    // Local write generation: which fetch or command produced this entry.
    seq: number;
  };
  const [imports, setImports] = useState<Record<string, ImportState>>({});
  const requestSeq = useRef(0);
  const detailSeq = useRef(0);
  const importSeq = useRef(0);
  // Synchronous ownership mirror of the imports map: slot -> owning request
  // and the generation of the last accepted write. Refs are updated outside
  // React state so async continuations can gate on fresh values.
  const importOwner = useRef<
    Record<string, { requestId: string; seq: number }>
  >({});

  // Ownership rule: an entry is owned by its requestId. Restores (mount
  // snapshot, detail latestImport, polls) may only fill an empty slot or
  // refresh their own request, and only when their fetch generation is at
  // least as new as the last accepted write. A late response never rolls the
  // slot back to another request or an older status. User commands claim the
  // slot with a fresh generation and always win.
  const writeImport = (
    arxivId: string,
    next: Omit<ImportState, "seq">,
    issued: number,
  ) => {
    const owner = importOwner.current[arxivId];
    if (owner) {
      if (owner.requestId !== next.requestId) return;
      if (issued < owner.seq) return;
    }
    importOwner.current[arxivId] = { requestId: next.requestId, seq: issued };
    setImports((s) => ({ ...s, [arxivId]: { ...next, seq: issued } }));
  };
  const claimImport = (
    arxivId: string,
    next: Omit<ImportState, "seq">,
  ): number => {
    const issued = ++importSeq.current;
    importOwner.current[arxivId] = { requestId: next.requestId, seq: issued };
    setImports((s) => ({ ...s, [arxivId]: { ...next, seq: issued } }));
    return issued;
  };

  const loadTasks = useCallback(() => {
    collectionApi
      .tasks()
      .then((items) => {
        if (!aliveRef.current) return;
        setTasks(items);
        // Restore open runs after switching pages: the server derives
        // activeRun from persisted runs, never from this page's memory.
        setRunStates((old) => {
          const next = { ...old };
          for (const task of items) {
            const run = (task as CollectionTask & { activeRun?: CollectionRun | null })
              .activeRun;
            if (run) next[task.id] = run;
          }
          return next;
        });
      })
      .catch((e) => toast.error((e as Error).message));
  }, []);

  const loadCandidates = useCallback(
    (cursor?: string) => {
      const seq = ++requestSeq.current;
      const params: Record<string, string> = {
        sort,
        channel: tab === "hot" ? "hot" : tab === "latest" ? "latest" : "",
      };
      if (taskFilter) params.task = taskFilter;
      if (citationFilter) params.citation = citationFilter;
      params.ignored = ignoredFilter;
      setListError("");
      if (query.trim()) params.q = query.trim();
      if (cursor) params.cursor = cursor;
      collectionApi
        .candidates(params)
        .then((page) => {
          // Reject late responses from superseded requests.
          if (seq !== requestSeq.current) return;
          setRows((old) => (cursor ? [...old, ...page.items] : page.items));
          setNextCursor(page.nextCursor);
        })
        .catch((e) => {
          if (seq === requestSeq.current) setListError((e as Error).message);
        });
    },
    [query, sort, tab, taskFilter, citationFilter, ignoredFilter],
  );

  useEffect(() => {
    loadTasks();
  }, [loadTasks]);
  useEffect(() => {
    setSelected(new Set());
    if (tab !== "tasks") loadCandidates();
  }, [tab, loadCandidates]);

  const runTask = (task: CollectionTask) => {
    collectionApi
      .runTask(task.id)
      .then((run) => {
        setRunStates((s) => ({ ...s, [task.id]: run }));
        toast.success("已开始发现，任务在后台推进");
      })
      .catch((e) => toast.error((e as Error).message));
  };

  const activeRuns = Object.values(runStates).filter(
    (run) => run.status === "queued" || run.status === "running",
  );
  const activeRunIDs = activeRuns
    .map((run) => run.id)
    .sort()
    .join(",");
  useEffect(() => {
    if (!activeRunIDs) return;
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      const results = await Promise.allSettled(
        activeRuns.map((run) => collectionApi.run(run.id)),
      );
      if (disposed) return;
      let finished = false;
      for (const result of results) {
        if (result.status === "rejected") {
          toast.error((result.reason as Error).message);
          return;
        }
        const run = result.value;
        setRunStates((states) => {
          const current = states[run.taskId];
          if (
            current?.id !== run.id ||
            (current.status !== "queued" && current.status !== "running")
          )
            return states;
          return { ...states, [run.taskId]: run };
        });
        finished ||= run.status !== "queued" && run.status !== "running";
      }
      if (finished) loadCandidates();
      timer = setTimeout(poll, 800);
    };
    timer = setTimeout(poll, 800);
    return () => {
      disposed = true;
      clearTimeout(timer);
    };
  }, [activeRunIDs, loadCandidates]);

  const startImport = () => {
    const items = rows
      .filter((r) => selected.has(r.candidate.arxivId))
      .map((r) => ({
        arxivId: r.candidate.arxivId,
        version: r.candidate.latestVersion,
        expectedContentHash: r.candidate.contentHash,
      }));
    if (!items.length) return;
    setBusy(true);
    collectionApi
      .importItems(items)
      .then((res) => {
        for (const item of res.items) {
          if (item.error) {
            toast.error(`${item.arxivId}: ${item.error}`);
            continue;
          }
          // User command: the server-issued request owns the slot.
          claimImport(item.arxivId, { requestId: item.requestId, status: "queued" });
          void pollImport(item.arxivId, item.requestId);
        }
      })
      .catch((e) => toast.error((e as Error).message))
      .finally(() => setBusy(false));
  };

  // Polling belongs to this page: switching away stops the loops, and mount
  // restores every active request from the server's persisted state.
  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    collectionApi
      .activeImports()
      .then((reqs) => {
        if (!aliveRef.current) return;
        // Snapshot restore fills empty slots or refreshes its own request;
        // it never replaces the map or rolls back a newer command. The
        // generation is fixed at fetch start so a slow snapshot cannot
        // outrank writes that happened while it was in flight.
        const issued = ++importSeq.current;
        for (const req of reqs) {
          writeImport(
            req.arxivId,
            {
              requestId: req.requestId,
              status: req.status,
              error: req.error,
              documentId: req.documentId,
              condition: req.condition,
              confirmPages: req.confirmPages,
              downloadVersion: req.downloadVersion,
            },
            issued,
          );
          if (
            req.status === "queued" ||
            req.status === "downloading" ||
            req.status === "importing"
          ) {
            void pollImport(req.arxivId, req.requestId);
          }
        }
      })
      .catch(() => {});
    return () => {
      aliveRef.current = false;
      requestSeq.current++;
      detailSeq.current++;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const pollImport = async (arxivId: string, requestId: string) => {
    for (let i = 0; i < 600; i++) {
      await new Promise((r) => setTimeout(r, 500));
      if (!aliveRef.current) return;
      // A superseded request is no longer this page's concern.
      if (importOwner.current[arxivId]?.requestId !== requestId) return;
      const issued = ++importSeq.current;
      try {
        const req = await collectionApi.importRequest(requestId);
        if (!aliveRef.current) return;
        writeImport(
          arxivId,
          {
            requestId,
            status: req.status,
            error: req.error,
            documentId: req.documentId,
            condition: req.condition,
            confirmPages: req.confirmPages,
            downloadVersion: req.downloadVersion,
            documentState: req.documentState,
          },
          issued,
        );
        // Long papers wait for the user's explicit confirmation; polling
        // never confirms on its own.
        if (
          req.status === "failed" ||
          req.status === "cancelled" ||
          req.status === "imported" ||
          req.status === "awaiting_confirmation"
        ) {
          return;
        }
      } catch {
        return;
      }
    }
  };

  // User action: confirm one long paper (51–150 pages) for the final commit.
  const confirmImport = (arxivId: string) => {
    const state = imports[arxivId];
    if (!state) return;
    const issued = ++importSeq.current;
    collectionApi
      .confirmImport(state.requestId)
      .then(() => {
        writeImport(arxivId, { ...state, status: "importing" }, issued);
        void pollImport(arxivId, state.requestId);
      })
      .catch((e) => toast.error((e as Error).message));
  };

  // User action: cancel one queued or downloading request. The response's
  // real status wins — a committing import reports importing, not cancelled.
  const cancelImport = (arxivId: string) => {
    const state = imports[arxivId];
    if (!state) return;
    const issued = ++importSeq.current;
    collectionApi
      .cancelImport(state.requestId)
      .then((res) => {
        writeImport(arxivId, { ...state, status: res.status }, issued);
        if (res.status !== "cancelled") {
          toast.info(`请求仍在${statusLabel[res.status] || res.status}，未取消`);
          void pollImport(arxivId, state.requestId);
        }
      })
      .catch((e) => toast.error((e as Error).message));
  };

  // User action: retry one failed request as a fresh pinned-version import
  // using the version and content hash the row already knows.
  const retryImport = (arxivId: string) => {
    const state = imports[arxivId];
    const row = rows.find((r) => r.candidate.arxivId === arxivId);
    if (!state || !row) return;
    setBusy(true);
    collectionApi
      .importItems([
        {
          arxivId,
          version: state.downloadVersion ?? row.candidate.latestVersion,
          expectedContentHash: row.candidate.contentHash,
        },
      ])
      .then((res) => {
        const item = res.items[0];
        if (!item || item.error) {
          toast.error(item?.error || "重试失败");
          return;
        }
        // User command: the new request claims the slot from the failed one.
        claimImport(arxivId, { requestId: item.requestId, status: "queued" });
        void pollImport(arxivId, item.requestId);
      })
      .catch((e) => toast.error((e as Error).message))
      .finally(() => setBusy(false));
  };

  const openDetail = (row: CandidateRow) => {
    const seq = ++detailSeq.current;
    const issued = ++importSeq.current;
    collectionApi
      .candidate(row.candidate.arxivId)
      .then((res) => {
        if (!aliveRef.current || seq !== detailSeq.current) return;
        setDetail({
          candidate: res.candidate,
          citation: res.citation,
          score: row.score,
          analyses: res.analyses,
          analysisJob: res.analysisJob,
        });
        if (res.latestImport) {
          const req = res.latestImport;
          // The restore generation was fixed when this fetch started: a
          // retry or new import claimed after that keeps the slot.
          writeImport(
            req.arxivId,
            {
              requestId: req.requestId,
              status: req.status,
              error: req.error,
              documentId: req.documentId,
              condition: req.condition,
              confirmPages: req.confirmPages,
              downloadVersion: req.downloadVersion,
              documentState: req.documentState,
            },
            issued,
          );
        }
      })
      .catch((e) => toast.error((e as Error).message));
  };

  const ignoreCandidate = (row: CandidateRow) => {
    collectionApi
      .setIgnored(row.candidate.arxivId, !row.candidate.ignored)
      .then(() => {
        setDetail(null);
        loadCandidates();
      })
      .catch((e) => toast.error((e as Error).message));
  };

  return (
    <main className="library-main">
      <header className="library-topbar">
        <div className="flex items-center gap-3">
          <Button
            size="icon-sm"
            variant="ghost"
            aria-label="切换侧栏"
            onClick={onToggleNav}
          >
            {navOpen ? (
              <X className="size-4" />
            ) : (
              <CirclePlus className="size-4" />
            )}
          </Button>
          <Tabs
            value={tab}
            onValueChange={(v) => {
              setTab(v as typeof tab);
              if (v === "hot") setSort("citations");
              else if (v === "latest") setSort("date");
            }}
          >
            <TabsList>
              <TabsTrigger value="latest">最新相关</TabsTrigger>
              <TabsTrigger value="hot">近期高被引</TabsTrigger>
              <TabsTrigger value="tasks">任务</TabsTrigger>
            </TabsList>
          </Tabs>
        </div>
        {tab !== "tasks" ? (
          <div className="flex flex-1 items-center gap-2">
            <div className="relative w-64">
              <Search className="absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
              <Input
                className="pl-8"
                placeholder="在候选中搜索"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            </div>
            <Button
              size="sm"
              variant="outline"
              onClick={() =>
                setSort(
                  sort === "date"
                    ? "citations"
                    : sort === "citations"
                      ? "relevance"
                      : "date",
                )
              }
            >
              排序：
              {sort === "date"
                ? "日期"
                : sort === "citations"
                  ? "引用"
                  : "相关性"}
            </Button>
            <Button
              size="sm"
              variant="outline"
              onClick={() => loadCandidates()}
            >
              <RefreshCw className="size-3.5" />
              刷新
            </Button>
            <Button
              size="sm"
              disabled={!selected.size || busy}
              onClick={startImport}
            >
              {busy ? (
                <Loader2 className="size-3.5 animate-spin" />
              ) : (
                <Download className="size-3.5" />
              )}
              下载并入库（{selected.size}）
            </Button>
          </div>
        ) : (
          <Button size="sm" onClick={() => setTaskForm("new")}>
            <CirclePlus className="size-3.5" />
            新建任务
          </Button>
        )}
        <Button
          size="icon-sm"
          variant="ghost"
          aria-label="采集设置"
          onClick={() => setSettingsOpen(true)}
        >
          <Settings2 className="size-4" />
        </Button>
      </header>
      {tab !== "tasks" ? (
        <div className="flex flex-wrap items-center gap-2 border-b px-4 py-2">
          <Select
            value={taskFilter || "all"}
            onValueChange={(value) =>
              setTaskFilter(value === "all" ? "" : value || "")
            }
          >
            <SelectTrigger aria-label="任务筛选" size="sm">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部任务</SelectItem>
              {tasks.map((task) => (
                <SelectItem key={task.id} value={task.id}>
                  {task.name}
                  {task.enabled ? "" : "（已关闭）"}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Select
            value={citationFilter || "all"}
            onValueChange={(value) =>
              setCitationFilter(value === "all" ? "" : value || "")
            }
          >
            <SelectTrigger aria-label="引用状态筛选" size="sm">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部引用状态</SelectItem>
              <SelectItem value="known">引用已知</SelectItem>
              <SelectItem value="unknown">引用未知</SelectItem>
            </SelectContent>
          </Select>
          <Select
            value={ignoredFilter}
            onValueChange={(value) => setIgnoredFilter(value || "0")}
          >
            <SelectTrigger aria-label="忽略状态筛选" size="sm">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="0">未忽略</SelectItem>
              <SelectItem value="1">已忽略</SelectItem>
            </SelectContent>
          </Select>
          {listError ? (
            <p role="alert" className="text-xs text-destructive">
              {listError}
            </p>
          ) : null}
        </div>
      ) : null}
      <div className="flex min-h-0 flex-1">
        <div className="min-w-0 flex-1">
          {tab === "tasks" ? (
            <ScrollArea className="h-full">
              <div className="space-y-2 p-4">
                {tasks.length === 0 ? (
                  <p className="py-16 text-center text-sm text-muted-foreground">
                    还没有发现任务。建立关键词组或作者关注后手动运行。
                  </p>
                ) : (
                  tasks.map((task) => {
                    const run = runStates[task.id];
                    return (
                      <div
                        key={task.id}
                        className="flex items-center gap-3 rounded-lg border p-3"
                      >
                        <div className="min-w-0 flex-1">
                          <p className="truncate text-sm font-medium">
                            {task.name}
                          </p>
                          <p className="mt-0.5 text-xs text-muted-foreground">
                            {(task.config?.keyword_groups ?? []).length
                              ? `关键词 ${(task.config?.keyword_groups ?? []).length} 组`
                              : ""}
                            {(task.config?.authors ?? []).length
                              ? ` · 作者 ${(task.config?.authors ?? []).length}`
                              : ""}
                            {(task.config?.categories ?? []).length
                              ? ` · 分类 ${(task.config?.categories ?? []).length}`
                              : ""}{" "}
                            · 每 {task.config?.frequency_hours ?? 24} 小时
                            {!task.enabled ? " · 已关闭" : ""}
                            {run
                              ? ` · ${statusLabel[run.status] || run.status}`
                              : ""}
                          </p>
                        </div>
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => runTask(task)}
                          disabled={
                            !task.enabled ||
                            run?.status === "queued" ||
                            run?.status === "running"
                          }
                        >
                          <Play className="size-3.5" />
                          运行
                        </Button>
                        {run?.status === "queued" ||
                        run?.status === "running" ? (
                          <Button
                            size="sm"
                            variant="ghost"
                            onClick={() => {
                              collectionApi
                                .cancelRun(run.id)
                                .then(() =>
                                  setRunStates((states) => ({
                                    ...states,
                                    [task.id]: { ...run, status: "cancelled" },
                                  })),
                                )
                                .catch((e) =>
                                  toast.error((e as Error).message),
                                );
                            }}
                          >
                            取消运行
                          </Button>
                        ) : null}
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => setTaskForm(task)}
                        >
                          编辑
                        </Button>
                      </div>
                    );
                  })
                )}
              </div>
            </ScrollArea>
          ) : (
            <ScrollArea className="h-full">
              <div className="space-y-1 p-2">
                {rows.length === 0 ? (
                  <p className="py-16 text-center text-sm text-muted-foreground">
                    暂无候选。在“任务”里建立任务并运行后，这里显示发现的论文原文。
                  </p>
                ) : (
                  rows.map((row) => {
                    const c = row.candidate;
                    const state = imports[c.arxivId];
                    return (
                      <div
                        key={c.arxivId}
                        className="flex items-start gap-2 rounded-md border border-transparent p-2 hover:border-border hover:bg-accent/40"
                      >
                        <Checkbox
                          className="mt-1"
                          aria-label={`选择 ${c.title}`}
                          checked={selected.has(c.arxivId)}
                          onCheckedChange={(checked) => {
                            const next = new Set(selected);
                            if (checked) next.add(c.arxivId);
                            else next.delete(c.arxivId);
                            setSelected(next);
                          }}
                        />
                        <button
                          className="min-w-0 flex-1 text-left"
                          onClick={() => openDetail(row)}
                        >
                          <p className="truncate text-sm">{c.title}</p>
                          <p className="mt-0.5 truncate text-xs text-muted-foreground">
                            {c.arxivId} · {(c.publishedAt ?? "").slice(0, 10)} ·{" "}
                            {row.score >= 0 ? `分 ${row.score}` : "待分析"}
                            {state
                              ? ` · ${statusLabel[state.status] || state.status}${
                                  state.downloadVersion
                                    ? ` v${state.downloadVersion}`
                                    : ""
                                }`
                              : ""}
                          </p>
                        </button>
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => ignoreCandidate(row)}
                        >
                          {c.ignored ? "取消忽略" : "忽略"}
                        </Button>
                        {state?.status === "awaiting_confirmation" ? (
                          <Button
                            size="sm"
                            onClick={() => confirmImport(c.arxivId)}
                          >
                            {state.condition === "trash-restore"
                              ? "确认恢复回收站文档"
                              : state.condition === "pages"
                                ? `确认入库（${state.confirmPages ?? "?"} 页）`
                                : "确认入库"}
                          </Button>
                        ) : null}
                        {state?.status === "queued" ||
                        state?.status === "downloading" ? (
                          <Button
                            size="sm"
                            variant="ghost"
                            onClick={() => cancelImport(c.arxivId)}
                          >
                            取消
                          </Button>
                        ) : null}
                        {state?.status === "imported" &&
                        state.documentState === "trash" ? (
                          <span className="text-xs text-muted-foreground">
                            文档在回收站
                          </span>
                        ) : state?.status === "imported" &&
                          state.documentState === "missing" ? (
                          <span className="text-xs text-muted-foreground">
                            文档已删除
                          </span>
                        ) : state?.status === "imported" ? (
                          <Button
                            size="sm"
                            variant="secondary"
                            onClick={() =>
                              onOpenDocument(state.documentId || "")
                            }
                          >
                            已入库 · 打开
                          </Button>
                        ) : null}
                        {state?.status === "failed" ? (
                          <>
                            <span className="text-xs text-destructive">
                              {state.error || "失败"}
                            </span>
                            <Button
                              size="sm"
                              variant="outline"
                              disabled={busy}
                              onClick={() => retryImport(c.arxivId)}
                            >
                              重试
                            </Button>
                          </>
                        ) : null}
                      </div>
                    );
                  })
                )}
                {nextCursor ? (
                  <div className="flex justify-center p-3">
                    <Button
                      size="sm"
                      variant="outline"
                      onClick={() => loadCandidates(nextCursor)}
                    >
                      加载更多
                    </Button>
                  </div>
                ) : null}
              </div>
            </ScrollArea>
          )}
        </div>
        {detail ? (
          <div className="w-96 border-l">
            <CandidateDetail
              key={detail.candidate.arxivId}
              candidate={detail.candidate}
              citation={detail.citation}
              score={detail.score}
              analyses={detail.analyses}
              analysisJob={detail.analysisJob}
              taskNames={Object.fromEntries(
                tasks.map((task) => [task.id, task.name]),
              )}
              onClose={() => {
                detailSeq.current++;
                setDetail(null);
              }}
            />
          </div>
        ) : null}
      </div>
      {taskForm ? (
        <TaskForm
          task={taskForm === "new" ? null : taskForm}
          onClose={() => setTaskForm(null)}
          onSaved={() => {
            setTaskForm(null);
            detailSeq.current++;
            setDetail(null);
            loadCandidates();
            loadTasks();
          }}
        />
      ) : null}
      {settingsOpen ? (
        <CollectionSettingsDialog
          onClose={() => setSettingsOpen(false)}
          onOpenReaderSettings={onOpenReaderSettings}
          onSaved={() => {
            setSettingsOpen(false);
            detailSeq.current++;
            setDetail(null);
            loadCandidates();
            loadTasks();
          }}
        />
      ) : null}
    </main>
  );
}
