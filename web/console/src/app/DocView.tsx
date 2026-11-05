// The read-only document viewer behind /ui/doc/{key}. This module is a lazy
// chunk of the shell (see routes.tsx), and it is the only place the markdown
// renderer is reached from — Home and Work never load it.

import { lazy, Suspense, useEffect, useState } from "react";
import { HttpError } from "../shared/http";
import { fetchDoc, type DocPayload } from "../shared/doc/docApi";
import { RendererBoundary } from "../shared/doc/RendererBoundary";
import { formatMinuteUTC } from "../shared/time";
import "../shared/doc/doc.css";

const DocMarkdown = lazy(() => import("../shared/doc/DocMarkdown"));

type State =
  | { status: "loading" }
  | { status: "ready"; doc: DocPayload }
  | { status: "missing" }
  | { status: "error" };

const REASON: Record<string, string> = {
  json: "JSON document, shown as text.",
  too_large: "Too large to render, shown as text.",
  not_text: "Not valid text, shown as text.",
};

function sectionFromHash(): string | null {
  const hash = window.location.hash.replace(/^#/, "");
  return hash === "" ? null : hash;
}

export default function DocView({ docKey }: { docKey: string }) {
  const [state, setState] = useState<State>({ status: "loading" });
  useEffect(() => {
    let cancelled = false;
    setState({ status: "loading" });
    fetchDoc(docKey).then(
      (doc) => {
        if (!cancelled) setState({ status: "ready", doc });
      },
      (err: unknown) => {
        if (cancelled) return;
        setState(err instanceof HttpError && err.status === 404 ? { status: "missing" } : { status: "error" });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [docKey]);

  if (state.status === "loading") return <p role="status">Loading document…</p>;
  if (state.status === "missing") return <p role="alert">Document not found.</p>;
  if (state.status === "error") return <p role="alert">Could not load this document.</p>;
  const { doc } = state;
  const updated = formatMinuteUTC(doc.updated_at);
  return (
    <article className="hk-view hk-doc">
      <header className="hk-doc-head">
        <h1>{doc.key}</h1>
        <p className="muted">
          {doc.kind}
          {updated ? ` · updated ${updated}` : ""}
        </p>
      </header>
      {doc.format === "markdown" ? (
        <RendererBoundary body={doc.body}>
          <Suspense fallback={<p role="status">Loading renderer…</p>}>
            <DocMarkdown body={doc.body} section={sectionFromHash()} />
          </Suspense>
        </RendererBoundary>
      ) : (
        <>
          <p className="muted">{REASON[doc.reason ?? ""] ?? "Shown as text."}</p>
          <pre className="hk-doc-raw">{doc.body}</pre>
        </>
      )}
    </article>
  );
}
