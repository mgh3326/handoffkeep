// The read-only document viewer behind /ui/doc/{key}. This module is a lazy
// chunk of the shell (see routes.tsx), and it is the only place the markdown
// renderer is reached from — Home and Work never load it.

import { lazy, Suspense, useEffect, useState } from "react";
import { HttpError } from "../shared/http";
import { fetchDoc, type DocPayload } from "../shared/doc/docApi";
import { RendererBoundary } from "../shared/doc/RendererBoundary";
import { formatMinuteUTC } from "../shared/time";
import { fetchDocumentAttachments } from "../shared/boardApi";
import type { BoardAttachment } from "../shared/boardTypes";

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

// A tbls document (`<prefix>/doc`) sits beside its `<prefix>/schema.json`.
function siblingSchemaKey(key: string): string | null {
  return key.endsWith("/doc") ? `${key.slice(0, -"doc".length)}schema.json` : null;
}

type Schema = { status: "none" } | { status: "ready"; doc: DocPayload } | { status: "error" };

function SchemaLink({ doc, schema, schemaKey }: { doc: DocPayload; schema: DocPayload; schemaKey: string }) {
  const same = doc.sha256 === schema.sha256;
  return (
    <section className="hk-doc-schema" aria-label="schema.json">
      <p>
        Schema: <a href={`/ui/doc/${schemaKey}`}>{schemaKey}</a>
      </p>
      <p className="muted">
        This document · updated {formatMinuteUTC(doc.updated_at)} · sha256 <code>{doc.sha256}</code>
      </p>
      <p className="muted">
        schema.json · updated {formatMinuteUTC(schema.updated_at)} · sha256 <code>{schema.sha256}</code>
      </p>
      <p className="muted" role="status">
        {same ? "Same version: the two sha256 values match." : "Different versions: the two sha256 values differ."}
      </p>
    </section>
  );
}

export default function DocView({ docKey }: { docKey: string }) {
  const [state, setState] = useState<State>({ status: "loading" });
  const [schema, setSchema] = useState<Schema>({ status: "none" });
  const [attachments, setAttachments] = useState<BoardAttachment[]>([]);
  const [attachmentsTruncated, setAttachmentsTruncated] = useState(false);
  const [attachmentsError, setAttachmentsError] = useState(false);
  useEffect(() => {
    const schemaKey = siblingSchemaKey(docKey);
    setSchema({ status: "none" });
    if (schemaKey === null) return;
    let cancelled = false;
    fetchDoc(schemaKey).then(
      (doc) => {
        if (!cancelled) setSchema({ status: "ready", doc });
      },
      (err: unknown) => {
        if (cancelled) return;
        // A missing sibling is simply no schema link; any other failure says so.
        setSchema(err instanceof HttpError && err.status === 404 ? { status: "none" } : { status: "error" });
      },
    );
    return () => {
      cancelled = true;
    };
  }, [docKey]);
  useEffect(() => {
    let cancelled = false;
    setState({ status: "loading" });
    setAttachments([]);
    setAttachmentsTruncated(false);
    setAttachmentsError(false);
    fetchDoc(docKey).then(
      (doc) => {
        if (!cancelled) setState({ status: "ready", doc });
        void fetchDocumentAttachments(docKey).then(result => { if (!cancelled) { setAttachments(result.attachments); setAttachmentsTruncated(result.truncated); } }).catch(() => { if (!cancelled) setAttachmentsError(true); });
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
          {doc.reason === "json" && (
            <p>
              <a className="hk-doc-download" href={`/ui/api/board/doc/download?key=${encodeURIComponent(doc.key)}`} download>
                Download JSON
              </a>
            </p>
          )}
          <pre className="hk-doc-raw">{doc.body}</pre>
        </>
      )}
      {schema.status === "ready" && <SchemaLink doc={doc} schema={schema.doc} schemaKey={schema.doc.key} />}
      {schema.status === "error" && <p className="muted" role="note">Could not check schema.json for this document.</p>}
      {(attachments.length > 0 || attachmentsError) && <section><h2>첨부 {attachmentsTruncated && <small>100+</small>}</h2>{attachmentsError ? <p role="alert">첨부를 불러오지 못했습니다.</p> : attachments.map(a => <p key={a.sha256}><a href={`/ui/files/${a.sha256}`}>{a.original_name}</a> · {a.size_bytes} bytes · {a.mime} · {formatMinuteUTC(a.created_at)} · {a.created_by}</p>)}</section>}
    </article>
  );
}
