// ARIB STD-B24 captions, drawn.
//
// The live stream carries two subtitle renditions of the same words. One is
// WebVTT, which the browser draws itself: the words at the bottom, and nothing
// else the broadcast sent — no colours, no background boxes, no ruby, and a 〓
// where a DRCS glyph was. The other is `sub{N}.json`, the decoder's own model
// (see libaribcaption-rs `render::json`), and this is what draws it.
//
// The model is a grid, not a paragraph. Every cell carries its own place on a
// caption plane the broadcast declares — 960×540 for full-seg, 320×180 for
// one-seg — its own size, spacing and scale, and its own colours. So there is no
// layout to do here: each cell is drawn where it was sent, and the only real
// question is how big a glyph has to be to fill the cell it was given.
//
// Answering that is the one thing to be careful about, and it is the mirror of
// the trap the `.ass` path hit. An ASS `Fontsize` is a *line box*, so a 36-unit
// cell has to be asked for as 36 × the font's own ascent+descent over its em
// (`ass::DEFAULT_FONT_SIZE_RATIO`, 1.395 for the font that ships with the
// daemon). Canvas is the other way round: `font: 36px` **is** the em, so the
// size goes in unmultiplied — and the ratio comes back on the other side, as the
// baseline offset, because ARIB centres the character in its cell and a font's
// ink is not centred on its baseline. Both numbers come out of the same
// measurement (`fontBoundingBoxAscent`/`Descent`), which is why they are taken
// from the font rather than assumed.

/// The family the daemon ships, declared by globals.css and named by every
/// `.ass` this stack writes (`ass::DEFAULT_FONT`). A rounded gothic, because
/// that is what ARIB specifies and what a television draws. Nothing is fetched
/// from the internet: the `.woff2` travels in the binary.
export const CAPTION_FONT = '"Rounded Mplus 1c"';

/// How far behind the playhead a cue is kept before it is dropped. The store is
/// fed by fragments as they load and read by the frame callback, so it only has
/// to cover the buffer between them.
const KEEP_MS = 120_000;

export type AribStyle = {
  bold?: boolean;
  italic?: boolean;
  underline?: boolean;
  /** ARIB "ornament": the glyph is outlined in `stroke_color`. */
  stroke?: boolean;
};

/** Which sides of the character cell carry a rule. */
export type AribEnclosure = {
  top?: boolean;
  right?: boolean;
  bottom?: boolean;
  left?: boolean;
};

// Absent fields are the model's defaults — the decoder leaves them out, because
// a caption is 40-odd cells and almost all of them are plain.
export type AribChar = {
  /** The character, or absent when this cell is a DRCS glyph. */
  text?: string;
  /** Key into the caption's `drcs` map. */
  drcs?: number;
  x: number;
  y: number;
  char_width: number;
  char_height: number;
  char_horizontal_spacing?: number;
  char_vertical_spacing?: number;
  char_horizontal_scale?: number;
  char_vertical_scale?: number;
  text_color: string;
  back_color?: string;
  stroke_color?: string;
  style?: AribStyle;
  enclosure?: AribEnclosure;
};

export type AribRegion = {
  x: number;
  y: number;
  width: number;
  height: number;
  /** Ruby (furigana): half-size text riding above the line it annotates. */
  is_ruby?: boolean;
  chars: AribChar[];
};

/** A bitmap the broadcast defined on the fly, for a character no code set has. */
export type AribDrcs = {
  width: number;
  height: number;
  /** Distinct levels, `depth_bits` bits per pixel. Two means a stencil. */
  depth: number;
  depth_bits: number;
  md5: string;
  /** The bitmap as transmitted: packed, MSB first, row-major, base64. */
  pixels: string;
  alternative?: string;
};

export type AribCaption = {
  plane_width: number;
  plane_height: number;
  regions: AribRegion[];
  drcs?: Record<string, AribDrcs>;
};

export type AribCue = {
  /** Broadcast PTS, in milliseconds. Not a position on any player's timeline. */
  start_ms: number;
  end_ms: number;
  /** Still on screen: the start is real, the end is provisional. */
  open: boolean;
  top: boolean;
  text: string;
  caption?: AribCaption;
};

/** One `sub{N}.json`: the cues overlapping one video segment's PTS window. */
export type AribSegment = {
  segment: number;
  start_ms: number;
  end_ms: number;
  cues: AribCue[];
};

/** Whether a parsed object is the segment document this expects. */
export function isAribSegment(value: unknown): value is AribSegment {
  const doc = value as AribSegment | null;
  return (
    !!doc &&
    typeof doc.start_ms === "number" &&
    typeof doc.end_ms === "number" &&
    Array.isArray(doc.cues)
  );
}

// The captions seen so far, keyed by the one thing that identifies a caption
// across the copies of it that arrive: when it started.
//
// Every segment the caption is on screen in carries it, so the same caption is
// delivered several times: open, with the end of each window it was still up in,
// and finally closed with the end the broadcast gave it. Keying on the start is
// what makes those one caption rather than five, and it is why the server does
// not cut cues at segment boundaries here the way it does for WebVTT — a player
// dedups by start *and end and text*, so copies with different ends are separate
// cues to it and it draws them all.
export class AribCueStore {
  private cues = new Map<number, AribCue>();

  add(cue: AribCue) {
    const have = this.cues.get(cue.start_ms);
    if (!have) {
      this.cues.set(cue.start_ms, cue);
      return;
    }
    // A closed cue is the broadcast's own answer and nothing supersedes it —
    // not even a later-arriving open copy, which keeps being republished for as
    // long as the caption is in the playlist window.
    if (!have.open) return;
    // Between two open copies the later window wins, and "later" has to mean
    // the end rather than the arrival: segments are fetched in parallel, so
    // the one that says the caption was still up at 12s can land before the one
    // that only reaches 11s.
    if (!cue.open || cue.end_ms > have.end_ms) this.cues.set(cue.start_ms, cue);
  }

  /** Drop what is far enough behind the playhead to be unreachable. */
  prune(nowMs: number) {
    for (const start of this.cues.keys()) {
      if (start < nowMs - KEEP_MS) this.cues.delete(start);
    }
  }

  clear() {
    this.cues.clear();
  }

  /**
   * The caption on screen at a broadcast PTS: the one that started most
   * recently and has not ended.
   *
   * `end_ms` is taken as given, for an open cue as much as a closed one, and
   * that is the whole of the timing policy here. An open cue's end is not the
   * decoder's five-second guess: the publisher rewrites it to the end of each
   * segment window the caption is still up in, so it means "on screen at least
   * this far" and it advances as the segments arrive. Inventing anything on top
   * of it — a maximum lifetime, say — puts the caption on screen past the last
   * moment anything said it was there, which is how it ends up outliving the
   * broadcast by whatever number was invented.
   */
  at(ptsMs: number): AribCue | null {
    let best: AribCue | null = null;
    for (const cue of this.cues.values()) {
      if (cue.start_ms > ptsMs) continue;
      if (best && cue.start_ms <= best.start_ms) continue;
      best = cue;
    }
    return best && ptsMs < best.end_ms ? best : null;
  }
}

// ── drawing ─────────────────────────────────────────────────────────

/** A cell's advance: the character box plus its spacing, scaled. */
function sectionWidth(ch: AribChar) {
  return Math.floor((ch.char_width + (ch.char_horizontal_spacing ?? 0)) * hScale(ch));
}

function sectionHeight(ch: AribChar) {
  return Math.floor((ch.char_height + (ch.char_vertical_spacing ?? 0)) * vScale(ch));
}

function hScale(ch: AribChar) {
  return ch.char_horizontal_scale ?? 1;
}

function vScale(ch: AribChar) {
  return ch.char_vertical_scale ?? 1;
}

function isTransparent(color: string | undefined): boolean {
  if (!color) return true;
  // "#RRGGBBAA" — the decoder writes straight alpha, so FF is opaque.
  return color.length >= 9 && parseInt(color.slice(7, 9), 16) === 0;
}

function fontOf(ch: AribChar) {
  const em = ch.char_height * vScale(ch);
  const weight = ch.style?.bold ? "bold " : "";
  const slant = ch.style?.italic ? "italic " : "";
  return { em, font: `${slant}${weight}${em}px ${CAPTION_FONT}` };
}

// The font's own line box, per font string. Two numbers that do not change
// while the page lives, and measuring them per cell per caption would be the
// only expensive thing here.
const metricsCache = new Map<string, { ascent: number; descent: number }>();

function lineBox(ctx: CanvasRenderingContext2D, font: string) {
  const cached = metricsCache.get(font);
  if (cached) return cached;
  ctx.font = font;
  const m = ctx.measureText("あ");
  const box = {
    ascent: m.fontBoundingBoxAscent,
    descent: m.fontBoundingBoxDescent,
  };
  // A browser that reports neither is not one this can place text in; fall
  // back to the ratio the shipped font measures at (1.395, split 1.075/0.320).
  if (!(box.ascent > 0)) {
    const size = parseFloat(font.match(/([\d.]+)px/)?.[1] ?? "36");
    box.ascent = size * 1.075;
    box.descent = size * 0.32;
  }
  metricsCache.set(font, box);
  return box;
}

/** Forget the measurements, for when the caption font finishes loading. */
export function resetFontMetrics() {
  metricsCache.clear();
  glyphCache.clear();
}

/**
 * Draw one caption into a context already transformed so that one unit is one
 * caption-plane unit — that is, `setTransform(w / plane_width, 0, 0, h /
 * plane_height, …)` against the *picture*, not the element's box.
 *
 * Which picture is the whole of it. The player's box is 16:9 but wider than the
 * 16:9 video inside it, because the height is capped; laying the plane across
 * the letterbox bars is what put every caption of the recordings player ~120px
 * left of where the broadcast had it, and it looks like a rendering that is
 * merely "not quite right" rather than like a bug.
 */
export function drawCaption(ctx: CanvasRenderingContext2D, caption: AribCaption) {
  ctx.textAlign = "left";
  ctx.textBaseline = "alphabetic";
  ctx.lineJoin = "round";

  // Backgrounds first, all of them: ARIB fills the whole cell behind the text,
  // and a neighbouring region's box must not be painted over the glyphs of the
  // one before it. Same reason the ASS renderer puts them on a lower layer.
  for (const region of caption.regions) drawBackgrounds(ctx, region);
  for (const region of caption.regions) drawText(ctx, region, caption.drcs);
}

/** One rectangle per run of cells sharing a background colour. */
function drawBackgrounds(ctx: CanvasRenderingContext2D, region: AribRegion) {
  let i = 0;
  while (i < region.chars.length) {
    const first = region.chars[i];
    let j = i + 1;
    while (j < region.chars.length && region.chars[j].back_color === first.back_color) j++;
    const run = region.chars.slice(i, j);
    i = j;
    if (isTransparent(first.back_color)) continue;
    const w = run.reduce((sum, ch) => sum + sectionWidth(ch), 0);
    const h = run.reduce((max, ch) => Math.max(max, sectionHeight(ch)), 0);
    ctx.fillStyle = first.back_color!;
    ctx.fillRect(first.x, first.y, w, h);
  }
}

function drawText(
  ctx: CanvasRenderingContext2D,
  region: AribRegion,
  drcs: Record<string, AribDrcs> | undefined,
) {
  for (const ch of region.chars) {
    const glyph = ch.drcs !== undefined ? drcs?.[String(ch.drcs)] : undefined;
    if (glyph && glyph.width > 0 && glyph.height > 0) {
      drawDrcs(ctx, ch, glyph);
    } else {
      // 〓 is what every ARIB decoder shows for a character it cannot draw. It
      // should be rare: a glyph that *was* transmitted is drawn above.
      drawGlyph(ctx, ch, ch.text ?? "〓");
    }
    if (ch.enclosure) drawEnclosure(ctx, ch);
  }
}

function drawGlyph(ctx: CanvasRenderingContext2D, ch: AribChar, text: string) {
  if (!text.trim()) return;
  const { em, font } = fontOf(ch);
  if (em <= 0) return;
  const { ascent, descent } = lineBox(ctx, font);
  ctx.font = font;

  // The character box is centred in the section, with the line spacing half
  // above and half below — and then the glyph is centred in *that*, against the
  // font's line box rather than its em. Both are what libass does for `\an4`,
  // and getting the second one wrong leaves the text riding above the box that
  // was drawn for it.
  const baseline = ch.y + sectionHeight(ch) / 2 + (ascent - descent) / 2;

  // Squeeze, never stretch. A fullwidth glyph in a half-width cell (MSZ — how a
  // Japanese caption fits ~34 characters on a line) is what a television
  // squeezes to 50%, and measuring rather than assuming is what keeps that right
  // when the decoder has already substituted a halfwidth character, or when the
  // named font is not the one that resolved.
  const width = ctx.measureText(text).width;
  const target = ch.char_width * hScale(ch);
  const squeeze = width > 0 ? Math.min(1, target / width) : 1;

  ctx.save();
  ctx.translate(ch.x, baseline);
  if (squeeze !== 1) ctx.scale(squeeze, 1);
  // ARIB's outlined text. Under it the cell's background is what makes the
  // caption readable, so nothing is drawn when the flag is off.
  if (ch.style?.stroke && !isTransparent(ch.stroke_color)) {
    ctx.strokeStyle = ch.stroke_color!;
    // Centred on the outline, so twice the 2 plane units `\bord2` puts outside
    // it in the ASS the same caption is written to for a recording.
    ctx.lineWidth = 4;
    ctx.strokeText(text, 0, 0);
  }
  ctx.fillStyle = ch.text_color;
  ctx.fillText(text, 0, 0);
  // Inside the squeeze, so the rule is the glyph's own width however it was
  // scaled to fit.
  if (ch.style?.underline) {
    ctx.fillRect(0, descent / 2, width, Math.max(1, em / 18));
  }
  ctx.restore();
}

/** Rules on the sides of the cell the broadcast asked for. */
function drawEnclosure(ctx: CanvasRenderingContext2D, ch: AribChar) {
  const w = sectionWidth(ch);
  const h = sectionHeight(ch);
  const t = Math.max(1, ch.char_height * vScale(ch) / 18);
  ctx.fillStyle = ch.text_color;
  if (ch.enclosure?.top) ctx.fillRect(ch.x, ch.y, w, t);
  if (ch.enclosure?.bottom) ctx.fillRect(ch.x, ch.y + h - t, w, t);
  if (ch.enclosure?.left) ctx.fillRect(ch.x, ch.y, t, h);
  if (ch.enclosure?.right) ctx.fillRect(ch.x + w - t, ch.y, t, h);
}

// Unpacked glyphs, keyed by their bitmap and the colour they were tinted. A
// caption redraws only when it changes, but a channel reuses the same handful
// of glyphs all evening.
const glyphCache = new Map<string, HTMLCanvasElement>();

/**
 * A DRCS glyph: the bitmap the broadcast transmitted, tinted and scaled into
 * the cell.
 *
 * This is the thing WebVTT cannot do at all. A DRCS character is a bitmap
 * defined on the fly for something no code set has, so there is no character to
 * name and no font to have it — but the pixels were sent, so there is nothing to
 * look up, only something to draw.
 */
function drawDrcs(ctx: CanvasRenderingContext2D, ch: AribChar, glyph: AribDrcs) {
  const key = `${glyph.md5}:${ch.text_color}`;
  let bitmap = glyphCache.get(key);
  if (!bitmap) {
    const rendered = renderDrcs(glyph, ch.text_color);
    if (!rendered) return;
    bitmap = rendered;
    glyphCache.set(key, bitmap);
  }
  const cellW = ch.char_width * hScale(ch);
  const cellH = ch.char_height * vScale(ch);
  // The same seat a text cell gets: the character box centred in the section.
  // Drawing at the section's own top instead leaves a DRCS character riding
  // above the words either side of it, by half the line spacing.
  const y = ch.y + (sectionHeight(ch) - cellH) / 2;
  const smoothing = ctx.imageSmoothingEnabled;
  // A stencil scaled up is meant to have hard edges — it is what the ASS path
  // draws as vector rectangles.
  ctx.imageSmoothingEnabled = false;
  ctx.drawImage(bitmap, ch.x, y, cellW, cellH);
  ctx.imageSmoothingEnabled = smoothing;
}

/** The packed bitmap as an RGBA canvas in the character's own colour. */
function renderDrcs(glyph: AribDrcs, color: string): HTMLCanvasElement | null {
  const bytes = decodeBase64(glyph.pixels);
  if (!bytes) return null;
  const canvas = document.createElement("canvas");
  canvas.width = glyph.width;
  canvas.height = glyph.height;
  const ctx = canvas.getContext("2d");
  if (!ctx) return null;

  const r = parseInt(color.slice(1, 3), 16);
  const g = parseInt(color.slice(3, 5), 16);
  const b = parseInt(color.slice(5, 7), 16);
  const a = color.length >= 9 ? parseInt(color.slice(7, 9), 16) : 255;

  const image = ctx.createImageData(glyph.width, glyph.height);
  // Most Japanese DRCS is two-level and this is a stencil. A deeper glyph uses
  // its extra levels as coverage, which is partial alpha here — where the ASS
  // renderer, which can only fill a path with one colour, needs a drawing per
  // level.
  const levels = Math.max(1, glyph.depth - 1);
  const bits = glyph.depth_bits;
  for (let y = 0; y < glyph.height; y++) {
    for (let x = 0; x < glyph.width; x++) {
      let value = 0;
      const at = (y * glyph.width + x) * bits;
      for (let i = 0; i < bits; i++) {
        const bit = at + i;
        const byte = bytes[bit >> 3] ?? 0;
        value = (value << 1) | ((byte >> (7 - (bit & 7))) & 1);
      }
      if (value === 0) continue;
      const p = (y * glyph.width + x) * 4;
      image.data[p] = r;
      image.data[p + 1] = g;
      image.data[p + 2] = b;
      image.data[p + 3] = Math.round((a * Math.min(value, levels)) / levels);
    }
  }
  ctx.putImageData(image, 0, 0);
  return canvas;
}

function decodeBase64(text: string): Uint8Array | null {
  try {
    const binary = atob(text);
    const bytes = new Uint8Array(binary.length);
    for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i);
    return bytes;
  } catch {
    return null;
  }
}

/**
 * Where the picture is inside the element, which is not where the element is.
 *
 * A `<video>` letterboxes (`object-fit: contain`), and the live player's box is
 * capped at 70vh so past that cap it is wider than the 16:9 picture in it. The
 * bars are the same black as the page, so nothing about it looks wrong until the
 * captions are laid out across them.
 */
export function pictureRect(video: HTMLVideoElement) {
  const boxW = video.clientWidth;
  const boxH = video.clientHeight;
  const { videoWidth, videoHeight } = video;
  if (!boxW || !boxH || !videoWidth || !videoHeight) return null;
  const scale = Math.min(boxW / videoWidth, boxH / videoHeight);
  const width = videoWidth * scale;
  const height = videoHeight * scale;
  return {
    left: (boxW - width) / 2,
    top: (boxH - height) / 2,
    width,
    height,
  };
}
