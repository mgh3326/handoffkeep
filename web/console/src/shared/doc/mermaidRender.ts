// Draws one Mermaid block of a document as sanitized SVG. Mermaid and
// DOMPurify are loaded with dynamic import() here, and only MermaidBlock
// reaches this module, so a document without a `mermaid` block never loads
// either library.
//
// Trust boundary — the block's text is data, never markup:
//  - Mermaid runs with securityLevel "strict" and htmlLabels false: no click
//    handlers, links or callbacks, and no HTML labels;
//  - the SVG Mermaid returns is passed through DOMPurify's SVG profile again,
//    with <a>, <foreignObject> and <script> forbidden, before it is inserted.

/** Mermaid's default maxTextSize. A larger block is refused with its reason,
 * never drawn by raising this limit. */
export const MERMAID_MAX_TEXT = 50_000;

export class DiagramTooLarge extends Error {
  constructor(readonly length: number) {
    super(`diagram source is ${length} characters; the limit is ${MERMAID_MAX_TEXT}`);
    this.name = "DiagramTooLarge";
  }
}

type Loaded = {
  mermaid: typeof import("mermaid").default;
  purify: typeof import("dompurify").default;
};

let loaded: Promise<Loaded> | null = null;

function load(): Promise<Loaded> {
  if (loaded === null) {
    loaded = Promise.all([import("mermaid"), import("dompurify")]).then(([m, d]) => {
      m.default.initialize({
        startOnLoad: false,
        securityLevel: "strict",
        htmlLabels: false,
        maxTextSize: MERMAID_MAX_TEXT,
      });
      return { mermaid: m.default, purify: d.default };
    });
  }
  return loaded;
}

// Mermaid keeps global render state, so blocks are drawn one at a time.
let queue: Promise<unknown> = Promise.resolve();
let sequence = 0;

/** Resolves to sanitized SVG markup, or rejects with the reason. */
export function renderMermaid(source: string): Promise<string> {
  const run = queue.then(() => draw(source));
  queue = run.catch(() => undefined);
  return run;
}

async function draw(source: string): Promise<string> {
  if (source.length > MERMAID_MAX_TEXT) {
    throw new DiagramTooLarge(source.length);
  }
  const { mermaid, purify } = await load();
  await mermaid.parse(source);
  sequence += 1;
  const { svg } = await mermaid.render(`hk-mermaid-${sequence}`, source);
  const clean = purify.sanitize(svg, {
    USE_PROFILES: { svg: true, svgFilters: true },
    FORBID_TAGS: ["a", "foreignObject", "script"],
  });
  if (clean === "") {
    throw new Error("the diagram produced no SVG");
  }
  return clean;
}
