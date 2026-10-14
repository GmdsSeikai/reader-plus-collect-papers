import type { Annotation, PDFLocation } from "@reader/core";
import type {
  DocumentInitParameters,
  PDFDocumentLoadingTask,
  PDFPageProxy,
} from "pdfjs-dist/types/src/display/api";
import { sentenceTextRanges } from "./pdf-sentence-matching";

// Writes Reader's PDF annotations into a copy of the original as standard PDF
// annotations, through PDF.js's own incremental save: the original bytes stay
// intact and the new objects are appended. Highlights, notes and questions
// become Highlight annotations; notes, question text and AI answers become
// their comments. Underlines become a thin strip under the text. Bookmarks have no PDF annotation and are left out.

type Rect = { x: number; y: number; width: number; height: number };
/** A quadrilateral in PDF user space: top-left, top-right, bottom-left, bottom-right. */
type Quad = [number, number, number, number, number, number, number, number];

export interface AnnotatedPDF {
  bytes: Uint8Array;
  exported: number;
  /** Annotations that could not be placed, such as unmatched translation marks. */
  skipped: number;
}

const editorPrefix = "pdfjs_internal_editor_";
const highlightType = 9;
/** Points; the reader draws a 2 px underline at 100 % zoom. */
const underlineWidth = 1.5;
const defaultColor = "#facc15";

export function hexColor(value: string): [number, number, number] {
  const match =
    /^#?([0-9a-f]{6})$/i.exec(value.trim()) ??
    /^#?([0-9a-f]{6})$/i.exec(defaultColor)!;
  const n = parseInt(match[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

/** Rects on one text line, as returned by Range.getClientRects(), become one box. */
export function lineBoxes(rects: Rect[]): Rect[] {
  const lines: Rect[] = [];
  for (const r of [...rects].sort((a, b) => a.y - b.y || a.x - b.x)) {
    const line = lines.find((l) => {
      const overlap =
        Math.min(l.y + l.height, r.y + r.height) - Math.max(l.y, r.y);
      return overlap > Math.min(l.height, r.height) / 2;
    });
    if (!line) {
      lines.push({ ...r });
      continue;
    }
    const right = Math.max(line.x + line.width, r.x + r.width);
    const bottom = Math.max(line.y + line.height, r.y + r.height);
    line.x = Math.min(line.x, r.x);
    line.y = Math.min(line.y, r.y);
    line.width = right - line.x;
    line.height = bottom - line.y;
  }
  return lines;
}

/** The comment written into the PDF, or "" for none. */
export function commentText(annotation: Annotation, answer?: string) {
  const note = annotation.note.trim();
  if (annotation.kind !== "question") return note;
  const parts = [`问题：${note}`];
  if (answer?.trim()) parts.push(`回答：${answer.trim()}`);
  return parts.join("\n\n");
}

const compact = (text: string) => text.replace(/\s+/g, "").toLowerCase();

// Reader stores rects relative to the displayed page, which includes /Rotate
// and the crop box; the viewport maps them back to PDF user space.
function displayQuads(page: PDFPageProxy, rects: Rect[]): Quad[] {
  const viewport = page.getViewport({ scale: 1 });
  const point = (x: number, y: number) =>
    viewport.convertToPdfPoint(x * viewport.width, y * viewport.height) as [
      number,
      number,
    ];
  return lineBoxes(rects).map((r) => [
    ...point(r.x, r.y),
    ...point(r.x + r.width, r.y),
    ...point(r.x, r.y + r.height),
    ...point(r.x + r.width, r.y + r.height),
  ]) as Quad[];
}

// A mark made on the translation stores the whole source block. Find the
// source sentences inside that block in the page text and cover only them.
async function sentenceQuads(
  page: PDFPageProxy,
  block: Rect,
  quote: string,
): Promise<Quad[]> {
  const viewport = page.getViewport({ scale: 1 });
  const content = await page.getTextContent();
  type Glyphs = {
    start: number;
    text: string;
    transform: number[];
    width: number;
    height: number;
  };
  const items: Glyphs[] = [];
  let text = "";
  for (const item of content.items) {
    if (!("str" in item) || !item.str.trim()) continue;
    const [a, b, , , e, f] = item.transform;
    // Only horizontal text is placed; rotated runs keep their block untouched.
    if (Math.abs(b) > 1e-3 || a <= 0) continue;
    const [cx, cy] = viewport.convertToViewportPoint(
      e + item.width / 2,
      f + item.height / 2,
    );
    const x = cx / viewport.width,
      y = cy / viewport.height;
    if (
      x < block.x ||
      x > block.x + block.width ||
      y < block.y ||
      y > block.y + block.height
    )
      continue;
    const value = compact(item.str);
    items.push({
      start: text.length,
      text: value,
      transform: item.transform,
      width: item.width,
      height: item.height,
    });
    text += value;
  }
  const needle = compact(quote);
  const [range] = sentenceTextRanges(text, text, [needle]);
  if (!range) return [];
  const quads: Quad[] = [];
  for (const item of items) {
    const from = Math.max(range.start, item.start) - item.start;
    const to = Math.min(range.end, item.start + item.text.length) - item.start;
    if (to <= from) continue;
    const [, , , d, e, f] = item.transform;
    const per = item.width / item.text.length;
    const left = e + from * per,
      right = e + to * per;
    const height = item.height || Math.abs(d);
    const bottom = f - height * 0.2,
      top = f + height * 0.85;
    quads.push([left, top, right, top, left, bottom, right, bottom]);
  }
  return quads;
}

function bounds(quads: Quad[]) {
  const xs = quads.flatMap((q) => [q[0], q[2], q[4], q[6]]);
  const ys = quads.flatMap((q) => [q[1], q[3], q[5], q[7]]);
  return [Math.min(...xs), Math.min(...ys), Math.max(...xs), Math.max(...ys)];
}

/** The bottom `thickness` points of a quad, along its own up direction. */
export function bottomStrip(q: Quad, thickness = underlineWidth): Quad {
  const height = Math.hypot(q[0] - q[4], q[1] - q[5]);
  const t = height > 0 ? Math.min(1, thickness / height) : 1;
  const up = (bx: number, by: number, tx: number, ty: number) => [
    bx + (tx - bx) * t,
    by + (ty - by) * t,
  ];
  return [
    ...up(q[4], q[5], q[0], q[1]),
    ...up(q[6], q[7], q[2], q[3]),
    q[4],
    q[5],
    q[6],
    q[7],
  ] as Quad;
}

function editorValue(
  annotation: Annotation,
  pageIndex: number,
  quads: Quad[],
  comment: string,
) {
  // An underline is an opaque strip along the bottom of each line, matching
  // the reader's 2 px underline. It is a highlight rather than an Ink line
  // because macOS Preview draws Ink annotations in white.
  const underline = annotation.kind === "underline";
  const marks = underline ? quads.map((q) => bottomStrip(q)) : quads;
  const rect = bounds(marks);
  return {
    annotationType: highlightType,
    pageIndex,
    rect,
    rotation: 0,
    color: hexColor(annotation.color || defaultColor),
    opacity: underline ? 1 : 0.5,
    quadPoints: marks.flat(),
    // Outline polygons go around each quad: TL, TR, BR, BL.
    outlines: marks.map((q) => [
      q[0],
      q[1],
      q[2],
      q[3],
      q[6],
      q[7],
      q[4],
      q[5],
    ]),
    user: "Reader",
    date: new Date(annotation.createdAt || Date.now()),
    popup: comment
      ? {
          contents: comment,
          deleted: false,
          rect: [rect[2], rect[3] - 100, rect[2] + 180, rect[3]],
        }
      : undefined,
  };
}

export async function annotatedPDF(
  getDocument: (params: DocumentInitParameters) => PDFDocumentLoadingTask,
  source: DocumentInitParameters,
  annotations: Annotation[],
  answers: Record<string, string> = {},
): Promise<AnnotatedPDF> {
  const task = getDocument(source);
  try {
    const pdf = await task.promise;
    let exported = 0,
      skipped = 0;
    for (const annotation of annotations) {
      if (annotation.kind === "bookmark" || annotation.location.type !== "pdf")
        continue;
      const location = annotation.location as PDFLocation;
      const rects = location.rects ?? [];
      if (location.page < 1 || location.page > pdf.numPages || !rects.length) {
        skipped++;
        continue;
      }
      const page = await pdf.getPage(location.page);
      const quads = location.translation
        ? await sentenceQuads(
            page,
            rects[0],
            location.quote ?? annotation.quote,
          )
        : displayQuads(page, rects);
      if (!quads.length) {
        skipped++;
        continue;
      }
      pdf.annotationStorage.setValue(
        `${editorPrefix}${annotation.id}`,
        editorValue(
          annotation,
          location.page - 1,
          quads,
          commentText(annotation, answers[annotation.answerId ?? ""]),
        ),
      );
      exported++;
    }
    const bytes = exported ? await pdf.saveDocument() : await pdf.getData();
    return { bytes, exported, skipped };
  } finally {
    await task.destroy();
  }
}
