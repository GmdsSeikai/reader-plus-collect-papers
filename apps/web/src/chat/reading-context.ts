import {
  locationLabel,
  type ReaderAdapter,
  type ReaderSelection,
  type SourceReference,
} from "@reader/core";
import { contextReferences } from "../references";

/** Capture the sending position before any asynchronous page extraction. */
export async function readingContext(
  adapter: Pick<ReaderAdapter, "getLocation" | "getContext"> | undefined,
  provider: string,
  selected: ReaderSelection[],
  useSection: boolean,
  hasImages: boolean,
) {
  const position = adapter?.getLocation();
  const readingLocation = position
    ? position.type === "pdf"
      ? {
          type: "pdf" as const,
          page: position.page,
          x: position.x,
          y: position.y,
        }
      : {
          type: "epub" as const,
          href: position.href,
          locator: position.locator,
          progression: position.progression,
          blockId: position.blockId,
          start: position.start,
          end: position.end,
          endBlockId: position.endBlockId,
        }
    : undefined;
  const quotes = useSection ? [] : selected;
  let context = quotes
    .map((s, i) => `[选区 ${i + 1} · ${locationLabel(s.location)}]\n${s.text}`)
    .join("\n\n");
  let references: SourceReference[] = quotes.map(({ text, location }) => ({
    text,
    location: structuredClone(location),
    kind: "selection",
  }));
  const documentAgent =
    !hasImages && (provider === "codex" || provider === "api");
  if (!context && !documentAgent) {
    context = (await adapter?.getContext()) || "";
    references = readingLocation
      ? contextReferences(context.slice(0, 21000), readingLocation)
      : [];
  }
  return {
    context,
    references,
    readingLocation,
    positionOnly: documentAgent && !context,
  };
}
