import { type ReaderAdapter, type ReaderSelection } from "@reader/core";
import { composeChatContext } from "./compose-context";

/** Capture the sending position before any asynchronous page extraction. */
export async function readingContext(
  adapter:
    | Pick<ReaderAdapter, "getLocation" | "getContext" | "getChapterContext">
    | undefined,
  provider: string,
  selected: ReaderSelection[],
  useSection: boolean,
  hasImages: boolean,
  options: Pick<
    Parameters<typeof composeChatContext>[0],
    "includeChapter" | "marks" | "confirmOversize"
  > = {},
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
  const documentAgent =
    !hasImages && (provider === "codex" || provider === "api");
  const quotes = structuredClone(useSection ? [] : selected);
  const { context, references } = await composeChatContext({
    ...options,
    adapter:
      adapter && readingLocation
        ? {
            ...adapter,
            getLocation: () => readingLocation,
            getContext: () => adapter.getContext(),
            getChapterContext: adapter.getChapterContext?.bind(adapter),
          }
        : undefined,
    selected: quotes,
    // Explicit preferences and quotations keep main's chapter context. For
    // an Agent, the page action only supplies its captured reading position.
    useSection: !documentAgent && (useSection || hasImages),
  });
  return {
    context,
    references,
    readingLocation,
    positionOnly: documentAgent && !context,
  };
}
