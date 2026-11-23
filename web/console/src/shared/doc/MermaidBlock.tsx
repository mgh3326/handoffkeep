// One `mermaid` fenced block. It shows the drawn SVG, or — when the block is
// too large or does not parse — the source as text with a one-line reason.

import { useEffect, useState } from "react";
import { DiagramTooLarge, renderMermaid } from "./mermaidRender";

type State = { status: "drawing" } | { status: "svg"; svg: string } | { status: "failed"; reason: string };

function reasonOf(err: unknown): string {
  if (err instanceof DiagramTooLarge) {
    return err.message;
  }
  const first = err instanceof Error ? err.message.split("\n")[0] : "";
  return first === "" ? "The diagram could not be drawn." : `The diagram could not be drawn: ${first}`;
}

export default function MermaidBlock({ source }: { source: string }) {
  const [state, setState] = useState<State>({ status: "drawing" });
  useEffect(() => {
    let cancelled = false;
    setState({ status: "drawing" });
    renderMermaid(source).then(
      (svg) => {
        if (!cancelled) setState({ status: "svg", svg });
      },
      (err: unknown) => {
        if (!cancelled) setState({ status: "failed", reason: reasonOf(err) });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [source]);

  if (state.status === "svg") {
    // The SVG goes in as an image, not as markup: the page's CSP (style-src
    // 'self') would block the <style> Mermaid puts inside it, and an image is
    // its own document, where scripts never run and nothing is fetched.
    const src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(state.svg)}`;
    return <img className="hk-doc-mermaid" data-doc-diagram="svg" src={src} alt="Diagram" />;
  }
  return (
    <>
      {state.status === "failed" ? (
        <p className="muted" role="note">
          {state.reason}
        </p>
      ) : (
        <p className="muted" role="status">
          Drawing diagram…
        </p>
      )}
      <pre className="hk-doc-raw" data-doc-diagram={state.status}>
        {source}
      </pre>
    </>
  );
}
