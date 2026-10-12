import { useState } from "react";
import { toast } from "sonner";
import { Button } from "@reader/ui/components/button";
import { Input } from "@reader/ui/components/input";
import { Textarea } from "@reader/ui/components/textarea";
import { Checkbox } from "@reader/ui/components/checkbox";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@reader/ui/components/dialog";
import {
  collectionApi,
  type CollectionTask,
  type CollectionTaskConfig,
} from "@reader/api";

function parseGroups(text: string): string[][] {
  return text
    .split("\n")
    .map((line) =>
      line
        .split(/[,，;；]/)
        .map((w) => w.trim())
        .filter(Boolean),
    )
    .filter((g) => g.length);
}

function parseAuthors(text: string): { name: string; aliases?: string[] }[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter(Boolean)
    .map((line) => {
      const m = line.match(/^(.*?)\((.*)\)$/);
      if (m) {
        return {
          name: m[1].trim(),
          aliases: m[2]
            .split(/[,，;；]/)
            .map((a) => a.trim())
            .filter(Boolean),
        };
      }
      return { name: line };
    });
}

/** Task form: keyword groups one per line, synonyms comma-separated; one
 * author per line as Name(alias1, alias2). Topic alone cannot be saved. */
export function TaskForm({
  task,
  onClose,
  onSaved,
}: {
  task: CollectionTask | null;
  onClose: () => void;
  onSaved: (task: CollectionTask) => void;
}) {
  const cfg = task?.config;
  const [name, setName] = useState(task?.name || "");
  const [enabled, setEnabled] = useState(task?.enabled ?? true);
  const [priority, setPriority] = useState(String(task?.priority ?? 3));
  const [topic, setTopic] = useState(cfg?.research_topic || "");
  const [notInterested, setNotInterested] = useState(cfg?.not_interested || "");
  const [groups, setGroups] = useState(
    (cfg?.keyword_groups || []).map((g) => g.join(", ")).join("\n"),
  );
  const [authors, setAuthors] = useState(
    (cfg?.authors || [])
      .map((a) => (a.aliases?.length ? `${a.name}(${a.aliases.join(", ")})` : a.name))
      .join("\n"),
  );
  const [categories, setCategories] = useState((cfg?.categories || []).join(", "));
  const [operatorAnd, setOperatorAnd] = useState(cfg?.group_operator_or === false);
  const [frequency, setFrequency] = useState(String(cfg?.frequency_hours ?? 24));
  const [latestDays, setLatestDays] = useState(String(cfg?.latest_window_days ?? 30));
  const [hotMonths, setHotMonths] = useState(String(cfg?.hot_window_months ?? 12));
  const [saving, setSaving] = useState(false);

  const save = () => {
    const config: CollectionTaskConfig = {
      v: 1,
      research_topic: topic.trim(),
      keyword_groups: parseGroups(groups),
      group_operator_or: !operatorAnd,
      authors: parseAuthors(authors),
      categories: categories
        .split(/[,，;；\n]/)
        .map((c) => c.trim())
        .filter(Boolean),
      not_interested: notInterested.trim(),
      frequency_hours: Number(frequency),
      latest_window_days: Number(latestDays),
      hot_window_months: Number(hotMonths),
    };
    setSaving(true);
    const done = (t: CollectionTask) => {
      setSaving(false);
      toast.success("任务已保存");
      onSaved(t);
    };
    const failed = (e: Error) => {
      setSaving(false);
      toast.error(e.message);
    };
    if (task) {
      collectionApi
        .updateTask(task.id, {
          name,
          enabled,
          priority: Number(priority),
          config,
          expectedRevision: task.queryRevision,
        })
        .then(done)
        .catch(failed);
    } else {
      collectionApi
        .createTask({ name, enabled, priority: Number(priority), config })
        .then(done)
        .catch(failed);
    }
  };

  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="max-w-xl">
        <DialogHeader>
          <DialogTitle>{task ? "编辑任务" : "新建任务"}</DialogTitle>
        </DialogHeader>
        <div className="space-y-3">
          <div className="grid grid-cols-[1fr_auto_auto] items-center gap-2">
            <Input
              placeholder="任务名称"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
            <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <Checkbox
                checked={enabled}
                onCheckedChange={(v) => setEnabled(v === true)}
              />
              启用
            </label>
            <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
              优先级
              <Input
                className="w-14"
                type="number"
                min={1}
                max={5}
                value={priority}
                onChange={(e) => setPriority(e.target.value)}
              />
            </label>
          </div>
          <div>
            <p className="mb-1 text-xs text-muted-foreground">
              关键词组（每行一组，组内逗号分隔同义词）
            </p>
            <Textarea
              rows={3}
              placeholder={"object detection, visual detection\nknowledge distillation"}
              value={groups}
              onChange={(e) => setGroups(e.target.value)}
            />
            <label className="mt-1 flex items-center gap-1.5 text-xs text-muted-foreground">
              <Checkbox
                checked={operatorAnd}
                onCheckedChange={(v) => setOperatorAnd(v === true)}
              />
              组间 AND（默认 OR）
            </label>
          </div>
          <div>
            <p className="mb-1 text-xs text-muted-foreground">
              作者（每行一位，Name(别名1, 别名2)；独立于关键词检索）
            </p>
            <Textarea
              rows={2}
              placeholder={"Ada Lovelace(A. Lovelace)"}
              value={authors}
              onChange={(e) => setAuthors(e.target.value)}
            />
          </div>
          <div>
            <p className="mb-1 text-xs text-muted-foreground">
              arXiv 分类（逗号分隔，约束主题分支）
            </p>
            <Input
              placeholder="cs.CV, cs.AI"
              value={categories}
              onChange={(e) => setCategories(e.target.value)}
            />
          </div>
          <div>
            <p className="mb-1 text-xs text-muted-foreground">
              研究主题与不关注方向（仅用于 AI 判断，不生成检索词）
            </p>
            <Textarea
              rows={2}
              value={topic}
              onChange={(e) => setTopic(e.target.value)}
            />
            <Input
              className="mt-1"
              placeholder="不关注方向"
              value={notInterested}
              onChange={(e) => setNotInterested(e.target.value)}
            />
          </div>
          <div className="grid grid-cols-3 gap-2">
            <label className="text-xs text-muted-foreground">
              频率（小时）
              <Input
                type="number"
                value={frequency}
                onChange={(e) => setFrequency(e.target.value)}
              />
            </label>
            <label className="text-xs text-muted-foreground">
              最新天数
              <Input
                type="number"
                value={latestDays}
                onChange={(e) => setLatestDays(e.target.value)}
              />
            </label>
            <label className="text-xs text-muted-foreground">
              热门月数
              <Input
                type="number"
                value={hotMonths}
                onChange={(e) => setHotMonths(e.target.value)}
              />
            </label>
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            取消
          </Button>
          <Button disabled={saving || !name.trim()} onClick={save}>
            保存
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
