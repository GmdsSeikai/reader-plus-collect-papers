import { useCallback, useEffect, useRef, useState } from "react";
import {
  CirclePlus,
  Download,
  Loader2,
  Play,
  RefreshCw,
  Search,
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
  collectionApi,
  type CollectionCandidate,
  type CollectionCitation,
  type CollectionRun,
  type CollectionTask,
  type CollectionTaskConfig,
} from "@reader/api";
import { TaskForm } from "./TaskForm";

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
  onClose,
}: {
  candidate: CollectionCandidate;
  citation: CollectionCitation | null;
  score: number;
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
        <Button size="icon-sm" variant="ghost" aria-label="关闭详情" onClick={onClose}>
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
                : String(citation.count)}
            </dd>
            {candidate.doi ? (
              <>
                <dt>DOI</dt>
                <dd>{candidate.doi}</dd>
              </>
            ) : null}
            <dt>相关分</dt>
            <dd>{score >= 0 ? String(score) : "待分析"}</dd>
          </dl>
          <Separator />
          <p className="whitespace-pre-wrap leading-relaxed">
            {candidate.abstract}
          </p>
        </div>
      </ScrollArea>
    </div>
  );
}

export function CollectionPage({
  navOpen,
  onToggleNav,
  onOpenDocument,
}: {
  navOpen: boolean;
  onToggleNav: () => void;
  onOpenDocument: (id: string) => void;
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
  } | null>(null);
  const [taskForm, setTaskForm] = useState<CollectionTask | "new" | null>(null);
  const [runStates, setRunStates] = useState<Record<string, CollectionRun>>({});
  const [query, setQuery] = useState("");
  const [sort, setSort] = useState<"date" | "citations" | "relevance">("date");
  const [busy, setBusy] = useState(false);
  const [imports, setImports] = useState<
    Record<
      string,
      { requestId: string; status: string; error?: string; documentId?: string }
    >
  >({});
  const requestSeq = useRef(0);

  const loadTasks = useCallback(() => {
    collectionApi
      .tasks()
      .then(setTasks)
      .catch((e) => toast.error((e as Error).message));
  }, []);

  const loadCandidates = useCallback(
    (cursor?: string) => {
      const seq = ++requestSeq.current;
      const params: Record<string, string> = { sort };
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
        .catch((e) => toast.error((e as Error).message));
    },
    [query, sort],
  );

  useEffect(() => {
    loadTasks();
  }, [loadTasks]);
  useEffect(() => {
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
        const next = { ...imports };
        for (const item of res.items) {
          if (item.error) {
            toast.error(`${item.arxivId}: ${item.error}`);
            continue;
          }
          next[item.arxivId] = {
            requestId: item.requestId,
            status: "queued",
          };
          void pollImport(item.arxivId, item.requestId);
        }
        setImports(next);
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
        const next: typeof imports = {};
        for (const req of reqs) {
          next[req.arxivId] = {
            requestId: req.requestId,
            status: req.status,
            error: req.error,
            documentId: req.documentId,
          };
          if (
            req.status === "queued" ||
            req.status === "downloading" ||
            req.status === "importing"
          ) {
            void pollImport(req.arxivId, req.requestId);
          }
        }
        setImports(next);
      })
      .catch(() => {});
    return () => {
      aliveRef.current = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const pollImport = async (arxivId: string, requestId: string) => {
    for (let i = 0; i < 600; i++) {
      await new Promise((r) => setTimeout(r, 500));
      if (!aliveRef.current) return;
      try {
        const req = await collectionApi.importRequest(requestId);
        if (!aliveRef.current) return;
        setImports((s) => ({
          ...s,
          [arxivId]: {
            requestId,
            status: req.status,
            error: req.error,
            documentId: req.documentId,
          },
        }));
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
    collectionApi
      .confirmImport(state.requestId)
      .then(() => {
        setImports((s) => ({
          ...s,
          [arxivId]: { ...state, status: "importing" },
        }));
        void pollImport(arxivId, state.requestId);
      })
      .catch((e) => toast.error((e as Error).message));
  };

  // User action: cancel one queued or downloading request.
  const cancelImport = (arxivId: string) => {
    const state = imports[arxivId];
    if (!state) return;
    collectionApi
      .cancelImport(state.requestId)
      .then(() => {
        setImports((s) => ({
          ...s,
          [arxivId]: { ...state, status: "cancelled" },
        }));
      })
      .catch((e) => toast.error((e as Error).message));
  };

  const openDetail = (row: CandidateRow) => {
    collectionApi
      .candidate(row.candidate.arxivId)
      .then((res) =>
        setDetail({
          candidate: res.candidate,
          citation: res.citation,
          score: row.score,
        }),
      )
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
            {navOpen ? <X className="size-4" /> : <CirclePlus className="size-4" />}
          </Button>
          <Tabs
            value={tab}
            onValueChange={(v) => setTab(v as typeof tab)}
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
              onClick={() => setSort(sort === "date" ? "citations" : sort === "citations" ? "relevance" : "date")}
            >
              排序：{sort === "date" ? "日期" : sort === "citations" ? "引用" : "相关性"}
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
      </header>
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
                            · 每 {(task.config?.frequency_hours ?? 24)} 小时
                            {!task.enabled ? " · 已关闭" : ""}
                            {run ? ` · ${statusLabel[run.status] || run.status}` : ""}
                          </p>
                        </div>
                        <Button
                          size="sm"
                          variant="outline"
                          onClick={() => runTask(task)}
                        >
                          <Play className="size-3.5" />
                          运行
                        </Button>
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
                              ? ` · ${statusLabel[state.status] || state.status}`
                              : ""}
                          </p>
                        </button>
                        {state?.status === "awaiting_confirmation" ? (
                          <Button size="sm" onClick={() => confirmImport(c.arxivId)}>
                            确认入库
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
                        {state?.status === "imported" ? (
                          <Button
                            size="sm"
                            variant="secondary"
                            onClick={() => onOpenDocument(state.documentId || "")}
                          >
                            已入库 · 打开
                          </Button>
                        ) : null}
                        {state?.status === "failed" ? (
                          <span className="text-xs text-destructive">
                            {state.error || "失败"}
                          </span>
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
              candidate={detail.candidate}
              citation={detail.citation}
              score={detail.score}
              onClose={() => setDetail(null)}
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
            loadTasks();
          }}
        />
      ) : null}
    </main>
  );
}
