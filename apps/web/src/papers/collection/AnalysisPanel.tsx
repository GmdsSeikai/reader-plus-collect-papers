import { useEffect, useRef, useState } from "react";
import { Button } from "@reader/ui/components/button";
import { Separator } from "@reader/ui/components/separator";
import {
  collectionApi,
  type CollectionAnalysis,
  type CollectionAnalysisJob,
} from "@reader/api";

const labels: Record<CollectionAnalysisJob["status"], string> = {
  queued: "等待分析",
  running: "分析中",
  succeeded: "分析完成",
  failed: "分析失败",
  cancelled: "分析已取消",
  interrupted: "分析已中断",
};

export function AnalysisPanel({
  candidateId,
  analyses: initialAnalyses,
  job: initialJob,
  taskNames,
}: {
  candidateId: string;
  analyses: CollectionAnalysis[];
  job: CollectionAnalysisJob | null;
  taskNames: Record<string, string>;
}) {
  const [analyses, setAnalyses] = useState(initialAnalyses);
  const [job, setJob] = useState(initialJob);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [pollRevision, setPollRevision] = useState(0);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);
  const active = job?.status === "queued" || job?.status === "running";
  useEffect(() => {
    setAnalyses(initialAnalyses);
    setJob(initialJob);
  }, [initialAnalyses, initialJob]);

  useEffect(() => {
    if (!active) return;
    let disposed = false;
    let timer: ReturnType<typeof setTimeout>;
    const poll = async () => {
      try {
        const detail = await collectionApi.candidate(candidateId);
        if (disposed) return;
        setAnalyses(detail.analyses);
        setJob(detail.analysisJob);
        if (
          detail.analysisJob?.status === "queued" ||
          detail.analysisJob?.status === "running"
        ) {
          timer = setTimeout(poll, 800);
        }
      } catch (e) {
        if (!disposed) setError((e as Error).message);
      }
    };
    timer = setTimeout(poll, 300);
    return () => {
      disposed = true;
      clearTimeout(timer);
    };
  }, [active, candidateId, job?.id, pollRevision]);

  const act = async (cancel: boolean) => {
    setBusy(true);
    setError("");
    try {
      if (cancel && job) {
        await collectionApi.cancelAnalysis(job.id);
        const detail = await collectionApi.candidate(candidateId);
        if (alive.current) {
          setJob(detail.analysisJob);
          setAnalyses(detail.analyses);
        }
      } else {
        const accepted = await collectionApi.analyze(candidateId);
        if (alive.current) setJob(accepted);
      }
    } catch (e) {
      if (alive.current) setError((e as Error).message);
    } finally {
      if (alive.current) setBusy(false);
    }
  };

  const translated = analyses.find((analysis) => analysis.result)?.result;
  const assessments = analyses.flatMap(
    (analysis) => analysis.result?.assessments ?? [],
  );
  return (
    <section className="space-y-3" aria-label="摘要分析">
      <Separator />
      <div className="flex items-center justify-between gap-2">
        <h3 className="font-medium">中文摘要与相关性</h3>
        <Button
          size="sm"
          variant="outline"
          disabled={busy}
          onClick={() => void act(active)}
        >
          {active
            ? "取消分析"
            : job?.status === "failed" || job?.status === "interrupted"
              ? "重试分析"
              : "分析摘要"}
        </Button>
      </div>
      {job ? (
        <p className="text-xs text-muted-foreground" role="status">
          {labels[job.status]}
        </p>
      ) : null}
      {error || job?.error ? (
        <p role="alert" className="text-xs text-destructive">
          {error || job?.error}
        </p>
      ) : null}
      {error && active ? (
        <Button
          size="sm"
          variant="ghost"
          onClick={() => {
            setError("");
            setPollRevision((value) => value + 1);
          }}
        >
          刷新状态
        </Button>
      ) : null}
      {translated ? (
        <div className="space-y-2">
          <p className="font-medium">{translated.title_zh}</p>
          <p className="whitespace-pre-wrap leading-relaxed">
            {translated.abstract_zh}
          </p>
        </div>
      ) : null}
      {assessments.map((assessment) => (
        <div
          key={assessment.taskId}
          className="space-y-1 rounded-md border p-2 text-xs"
        >
          <p className="font-medium">
            {taskNames[assessment.taskId] || "任务"} ·{" "}
            {assessment.relevance_score} 分
          </p>
          <p>{assessment.reason_zh}</p>
          {assessment.evidence?.map((evidence, index) => (
            <blockquote
              key={index}
              className="border-l-2 pl-2 text-muted-foreground"
            >
              <span>{evidence.field === "title" ? "标题" : "摘要"}：</span>
              {evidence.quote}
            </blockquote>
          ))}
        </div>
      ))}
    </section>
  );
}
