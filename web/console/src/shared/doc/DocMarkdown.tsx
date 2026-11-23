// Lazily loaded markdown renderer for a task's body document. This module is
// the only place the renderer libraries are imported; DocInline reaches it
// through React.lazy, so the queue list's static import graph never contains
// it (hk:doc advice/2026-09-21/console-design-system-astra Q4).
//
// Trust boundary — the document is data, never markup:
//  - raw HTML is never rendered (no rehype-raw); rehype-sanitize with a
//    narrowed schema is the last hast transform;
//  - every URL goes through safeDocUrl: https, same-origin paths and
//    in-document #fragments only;
//  - images are never fetched: an image renders as its alt text plus a link;
//  - links carry rel="noopener noreferrer" and never open by script;
//  - a `mermaid` fenced block is drawn by MermaidBlock (mermaidRender.ts),
//    loaded only when such a block exists, and shown as an image, never as
//    markup.

import { Children, isValidElement, useEffect, useRef, type ReactNode } from "react";
import Markdown, { type Components } from "react-markdown";
import rehypeSanitize, { defaultSchema, type Options as SanitizeSchema } from "rehype-sanitize";
import remarkGfm from "remark-gfm";
import { findSectionHeading, safeDocUrl } from "./docrender";
import MermaidBlock from "./MermaidBlock";
import { remarkTableLinks } from "./tableLinks";
import "./doc.css";

const schema: SanitizeSchema = {
  ...defaultSchema,
  protocols: {
    ...defaultSchema.protocols,
    href: ["https"],
    src: ["https"],
    cite: ["https"],
  },
};

function urlTransform(url: string): string | undefined {
  return safeDocUrl(url);
}

// The source of a fenced `mermaid` block, or null for any other <pre>.
function mermaidSource(children: ReactNode): string | null {
  const code = Children.toArray(children).find(isValidElement);
  if (!isValidElement<{ className?: string; children?: ReactNode }>(code)) {
    return null;
  }
  if (!/(^|\s)language-mermaid(\s|$)/.test(code.props.className ?? "")) {
    return null;
  }
  return typeof code.props.children === "string" ? code.props.children.replace(/\n$/, "") : null;
}

const components: Components = {
  pre({ children }) {
    const source = mermaidSource(children);
    if (source !== null) {
      return <MermaidBlock source={source} />;
    }
    return <pre>{children}</pre>;
  },
  a({ href, title, id, children }) {
    if (!href) {
      return <span className="hk-doc-deadlink">{children}</span>;
    }
    return (
      <a href={href} title={title} id={id} rel="noopener noreferrer">
        {children}
      </a>
    );
  },
  img({ src, alt }) {
    // v1 never fetches images: no <img>, no background request.
    const href = typeof src === "string" ? src : undefined;
    return (
      <span className="hk-doc-img">
        [이미지: {alt ? alt : "대체 텍스트 없음"}]
        {href ? (
          <>
            {" "}
            <a href={href} rel="noopener noreferrer">
              이미지 링크
            </a>
          </>
        ) : null}
      </span>
    );
  },
};

export default function DocMarkdown({ body, section, onSection }: { body: string; section: string | null; onSection?: (found: boolean) => void }) {
  const root = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (section === null || root.current === null) {
      return;
    }
    const heading = findSectionHeading(root.current, section);
    if (heading) {
      heading.classList.add("hk-doc-section-target");
      heading.scrollIntoView?.({ block: "start" });
    }
    onSection?.(heading !== null);
  }, [body, section, onSection]);
  return (
    <div className="hk-doc-body" ref={root} data-doc-renderer="markdown">
      <Markdown remarkPlugins={[remarkGfm, remarkTableLinks]} rehypePlugins={[[rehypeSanitize, schema]]} components={components} urlTransform={urlTransform}>
        {body}
      </Markdown>
    </div>
  );
}
