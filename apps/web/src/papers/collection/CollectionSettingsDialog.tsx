import { useEffect, useRef, useState } from "react";
import { api, collectionApi, type CollectionSettings } from "@reader/api";
import type { AIConfig, AgentModel } from "@reader/core";
import { Button } from "@reader/ui/components/button";
import { Checkbox } from "@reader/ui/components/checkbox";
import { Input } from "@reader/ui/components/input";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from "@reader/ui/components/dialog";
import {
  Select,
  SelectTrigger,
  SelectValue,
  SelectContent,
  SelectItem,
} from "@reader/ui/components/select";
import {
  AlertDialog,
  AlertDialogContent,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogCancel,
  AlertDialogAction,
} from "@reader/ui/components/alert-dialog";

export function CollectionSettingsDialog({
  onClose,
  onSaved,
  onOpenReaderSettings,
}: {
  onClose: () => void;
  onSaved: (settings: CollectionSettings) => void;
  onOpenReaderSettings?: () => void;
}) {
  const [settings, setSettings] = useState<CollectionSettings | null>(null);
  const [ai, setAI] = useState<AIConfig | null>(null);
  const [models, setModels] = useState<AgentModel[]>([]);
  const [autoTranslate, setAutoTranslate] = useState(true);
  const [key, setKey] = useState("");
  const [error, setError] = useState("");
  const [modelError, setModelError] = useState("");
  const [busy, setBusy] = useState(false);
  const [confirmEnable, setConfirmEnable] = useState(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    Promise.all([collectionApi.settings(), api.aiConfig(), api.settings()])
      .then(([cfg, connection, reader]) => {
        if (!alive.current) return;
        setSettings(cfg);
        setAI(connection);
        setAutoTranslate(reader.autoTranslatePDF ?? true);
      })
      .catch((e) => {
        if (alive.current) setError((e as Error).message);
      });
    return () => {
      alive.current = false;
    };
  }, []);
  const provider = settings?.analysisProvider || ai?.primary || "";
  useEffect(() => {
    if (!provider) return;
    const controller = new AbortController();
    setModels([]);
    setModelError("");
    api
      .agentModels(provider, controller.signal)
      .then((items) => {
        if (!controller.signal.aborted) setModels(items);
      })
      .catch((e) => {
        if (!controller.signal.aborted) setModelError((e as Error).message);
      });
    return () => controller.abort();
  }, [provider]);
  const change = (patch: Partial<CollectionSettings>) =>
    setSettings((value) => (value ? { ...value, ...patch } : value));
  const save = async () => {
    if (!settings) return;
    if (
      ![settings.dailyLimit, settings.requestRpm].every(
        (value) => Number.isInteger(value) && value! >= 0,
      )
    ) {
      setError("额度和请求频率须为非负整数");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const saved = await collectionApi.saveSettings(settings);
      if (alive.current) onSaved(saved);
    } catch (e) {
      if (alive.current) setError((e as Error).message);
    } finally {
      if (alive.current) setBusy(false);
    }
  };
  const saveKey = async (clear: boolean) => {
    setBusy(true);
    setError("");
    try {
      const latest = await api.aiConfig();
      const saved = await api.saveAIConfig({
        ...latest,
        citationsApiKey: clear ? "" : key.trim(),
        clearCitationsKey: clear,
      });
      if (alive.current) {
        setAI(saved);
        setKey("");
        change({ hasCitationsKey: saved.hasCitationsKey });
      }
    } catch (e) {
      if (alive.current) setError((e as Error).message);
    } finally {
      if (alive.current) setBusy(false);
    }
  };
  return (
    <>
      <Dialog open onOpenChange={(open) => !open && !busy && onClose()}>
        <DialogContent className="max-h-[85dvh] max-w-lg overflow-y-auto">
          <DialogHeader>
            <DialogTitle>采集设置</DialogTitle>
          </DialogHeader>
          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          ) : null}
          {settings ? (
            <div className="space-y-4 text-sm">
              <label className="flex items-center gap-2">
                <Checkbox
                  checked={settings.analysisEnabled ?? false}
                  onCheckedChange={(checked) => {
                    if (checked) setConfirmEnable(true);
                    else change({ analysisEnabled: false });
                  }}
                />
                启用手动摘要分析
              </label>
              <div className="grid grid-cols-2 gap-3">
                <label className="space-y-1">
                  分析服务
                  <Select
                    value={settings.analysisProvider || "default"}
                    onValueChange={(value) =>
                      change({
                        analysisProvider:
                          value === "default" ? "" : value || "",
                        analysisModel: "",
                      })
                    }
                  >
                    <SelectTrigger className="w-full" aria-label="分析服务">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="default">
                        主服务{ai?.primary ? `（${ai.primary}）` : ""}
                      </SelectItem>
                      <SelectItem value="codex">Codex</SelectItem>
                      <SelectItem value="claude">Claude</SelectItem>
                      <SelectItem value="kimi">Kimi</SelectItem>
                      <SelectItem value="api">API</SelectItem>
                    </SelectContent>
                  </Select>
                </label>
                <label className="space-y-1">
                  Effort
                  <Select
                    value={settings.analysisEffort || "default"}
                    onValueChange={(value) =>
                      change({
                        analysisEffort: value === "default" ? "" : value || "",
                      })
                    }
                  >
                    <SelectTrigger className="w-full" aria-label="分析 Effort">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {["default", "low", "medium", "high", "max"].map(
                        (value) => (
                          <SelectItem key={value} value={value}>
                            {value === "default" ? "默认" : value}
                          </SelectItem>
                        ),
                      )}
                    </SelectContent>
                  </Select>
                </label>
              </div>
              <label className="block space-y-1">
                模型
                <Select
                  value={settings.analysisModel || "default"}
                  onValueChange={(value) =>
                    change({
                      analysisModel: value === "default" ? "" : value || "",
                    })
                  }
                >
                  <SelectTrigger className="w-full" aria-label="分析模型">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="default">自动推荐</SelectItem>
                    {settings.analysisModel &&
                    !models.some(
                      (model) => model.id === settings.analysisModel,
                    ) ? (
                      <SelectItem value={settings.analysisModel}>
                        {settings.analysisModel}
                      </SelectItem>
                    ) : null}
                    {models.map((model) => (
                      <SelectItem key={model.id} value={model.id}>
                        {model.name || model.id}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </label>
              {modelError ? (
                <p className="text-xs text-destructive">{modelError}</p>
              ) : null}
              <div className="grid grid-cols-2 gap-3">
                <label className="space-y-1">
                  每日模型请求额度
                  <Input
                    type="number"
                    min={0}
                    step={1}
                    value={settings.dailyLimit ?? 100}
                    onChange={(e) =>
                      change({ dailyLimit: Number(e.target.value) })
                    }
                  />
                </label>
                <label className="space-y-1">
                  每分钟模型请求数
                  <Input
                    type="number"
                    min={0}
                    step={1}
                    value={settings.requestRpm ?? 0}
                    onChange={(e) =>
                      change({ requestRpm: Number(e.target.value) })
                    }
                  />
                </label>
              </div>
              <p className="text-xs text-muted-foreground">
                额度为 0 时不发起分析；频率为 0 时不额外限流。
              </p>
              {settings.quota ? (
                <p className="text-xs text-muted-foreground">
                  UTC {settings.quota.utcDay} · 已发 {settings.quota.sent} ·
                  预占 {settings.quota.reserved} · 发送不确定{" "}
                  {settings.quota.uncertain}
                </p>
              ) : null}
              <label className="flex items-center gap-2">
                <Checkbox
                  checked={settings.paused ?? false}
                  onCheckedChange={(checked) =>
                    change({ paused: checked === true })
                  }
                />
                暂停采集
              </label>
              <div className="space-y-2 border-t pt-3">
                <label className="block space-y-1">
                  引用统计 Key（可选）
                  <Input
                    type="password"
                    autoComplete="off"
                    placeholder={
                      ai?.hasCitationsKey ? "已保存，留空保留" : "未配置"
                    }
                    value={key}
                    onChange={(e) => setKey(e.target.value)}
                  />
                </label>
                <div className="flex gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={busy || !key.trim()}
                    onClick={() => void saveKey(false)}
                  >
                    保存 Key
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={busy || !ai?.hasCitationsKey}
                    onClick={() => void saveKey(true)}
                  >
                    清除 Key
                  </Button>
                </div>
              </div>
              <div className="space-y-1 border-t pt-3 text-xs text-muted-foreground">
                <p>
                  入库后全文翻译：{autoTranslate ? "已开启" : "已关闭"}
                  ，模型请求独立计费。
                </p>
                {onOpenReaderSettings ? (
                  <Button
                    size="sm"
                    variant="link"
                    className="px-0"
                    onClick={() => {
                      onClose();
                      onOpenReaderSettings();
                    }}
                  >
                    阅读与翻译设置
                  </Button>
                ) : null}
              </div>
            </div>
          ) : (
            <p className="text-sm text-muted-foreground">加载设置中…</p>
          )}
          <DialogFooter>
            <Button variant="outline" disabled={busy} onClick={onClose}>
              取消
            </Button>
            <Button disabled={busy || !settings} onClick={() => void save()}>
              保存
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <AlertDialog open={confirmEnable} onOpenChange={setConfirmEnable}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>启用手动摘要分析？</AlertDialogTitle>
            <AlertDialogDescription>
              分析只在你主动提交时发起，不会自动运行。模型请求可能产生费用；失败或取消的已发请求仍占用每日额度，一篇论文涉及多个任务时可能拆成多次请求。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                change({ analysisEnabled: true });
                setConfirmEnable(false);
              }}
            >
              启用
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
