// A task body or comment rendered with the console's shared markdown renderer
// (shared/doc/DocMarkdown: react-markdown + remark-gfm + rehype-sanitize). The
// renderer is a lazy chunk so the work page's entry graph stays small, and the
// raw text stays one toggle away — markdown can hide what its source says.
import { lazy, Suspense, useState } from "react";
import { RendererBoundary } from "../shared/doc/RendererBoundary";
import { translationAction } from "../shared/translation";

const DocMarkdown = lazy(() => import("../shared/doc/DocMarkdown"));

export function MarkdownBody({ original, korean, stale, compact, rawToggle = true }: { original: string; korean?: string; stale?: boolean; compact?: boolean; rawToggle?: boolean }) {
  const [raw, setRaw] = useState(false);
  // The console's translation preference picks the text; the renderer only
  // decides how it looks. A stale translation keeps the original.
  const text = translationAction.applicable(korean, stale) ? (korean ?? original) : original;
  return (
    <div className={compact ? "work-markdown work-markdown--compact" : "work-markdown"}>
      {(stale || rawToggle) && (
        <div className="work-markdown-bar">
          {stale && <small className="hk-translation-stale"> 갱신 필요</small>}
          {rawToggle && (
            <button type="button" className="work-markdown-toggle" aria-pressed={raw} onClick={() => setRaw(value => !value)}>
              {raw ? "렌더" : "원문"}
            </button>
          )}
        </div>
      )}
      {raw ? (
        <pre className="hk-doc-raw">{text}</pre>
      ) : (
        <RendererBoundary body={text}>
          <Suspense fallback={<pre className="hk-doc-raw">{text}</pre>}>
            <DocMarkdown body={text} section={null} />
          </Suspense>
        </RendererBoundary>
      )}
    </div>
  );
}
