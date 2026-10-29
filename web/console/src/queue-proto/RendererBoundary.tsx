// Shared renderer-failure fallback: a failed markdown chunk load or a render
// error must leave the document readable as raw text, with the reason stated.
// Kept separate from DocInline so the document browser can reuse it without
// importing DocInline into the queue's entry graph.

import { Component, type ReactNode } from "react";

export class RendererBoundary extends Component<{ body: string; children: ReactNode }, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() {
    return { failed: true };
  }
  render() {
    if (this.state.failed) {
      return (
        <>
          <p className="qp-unknown" role="note">
            오류 — 렌더러를 불러오지 못해 원문 텍스트로 보입니다.
          </p>
          <pre className="qp-doc-raw">{this.props.body}</pre>
        </>
      );
    }
    return this.props.children;
  }
}
