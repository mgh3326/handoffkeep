// Task body front-matter — the optional read contract from #628. When the
// linked body_doc markdown opens with a `---` front-matter block that carries
// `schema: hk-task/v1`, the drawer may read `summary` and `display_title`
// from it. Every other shape — no fence, a different schema, missing fields,
// multi-line values — is ignored: absent metadata is never inferred.
//
// This module is plain text handling — it never imports the markdown
// renderer, and parsed values are rendered as text nodes only.

import { useEffect, useState } from "react";
import type { BoardDoc } from "../board/types";
import { parseBodyDoc } from "./bodydoc";
import type { FetchDoc } from "./DocInline";

export type TaskDocMeta = {
  summary: string | null;
  displayTitle: string | null;
};

const FENCE_RE = /^---[ \t]*\r?\n([\s\S]*?)\r?\n---[ \t]*\r?(?:\n|$)/;
const FIELD_RE = /^([A-Za-z_][A-Za-z0-9_-]*)[ \t]*:[ \t]*(.*)$/;
/** YAML block-scalar indicators ("|", ">", "|+", ">-2" …) are not values —
 * a folded multi-line field reads as absent, never as the indicator text. */
const BLOCK_SCALAR_RE = /^[|>][+-]?\d*$/;

function unquote(value: string): string {
  const t = value.trim();
  if (t.length >= 2 && ((t.startsWith('"') && t.endsWith('"')) || (t.startsWith("'") && t.endsWith("'")))) {
    return t.slice(1, -1);
  }
  return t;
}

function gateMeta(fieldsText: string): TaskDocMeta | null {
  const fields = new Map<string, string>();
  for (const line of fieldsText.split(/\r?\n/)) {
    const kv = FIELD_RE.exec(line);
    if (kv !== null) {
      fields.set(kv[1], unquote(kv[2]));
    }
  }
  if (fields.get("schema") !== "hk-task/v1") {
    return null;
  }
  const pick = (name: string): string | null => {
    const v = fields.get(name);
    return v === undefined || v === "" || BLOCK_SCALAR_RE.test(v) ? null : v;
  };
  return { summary: pick("summary"), displayTitle: pick("display_title") };
}

/** Reads the front-matter block at the top of a markdown document. Returns
 * null unless the block gates on `schema: hk-task/v1` — the new read
 * contract opts in explicitly so arbitrary docs never feed the header. */
export function parseTaskDocMeta(body: string): TaskDocMeta | null {
  const m = FENCE_RE.exec(body);
  return m === null ? null : gateMeta(m[1]);
}

/** Removes a gated hk-task/v1 front-matter block from the text handed to the
 * renderer — the contract fields are read by the drawer, not meant to paint
 * as a giant heading above the spec. Ungated bodies are returned unchanged. */
export function stripTaskFrontMatter(body: string): string {
  const m = FENCE_RE.exec(body);
  if (m === null || gateMeta(m[1]) === null) {
    return body;
  }
  return body.slice(m[0].length);
}

export type TaskDocMetaState =
  | { status: "none" } // no usable body_doc key — the metadata contract does not apply
  | { status: "loading" }
  | { status: "error" } // the document could not be read — distinct from "no metadata"
  | { status: "ready"; meta: TaskDocMeta | null };

/** Fetches the linked body_doc once and reads its hk-task/v1 front-matter.
 * The shared (cached) fetch keeps this from doubling the request DocInline
 * already makes for the same document. */
export function useTaskDocMeta(bodyDoc: string, fetchDoc: FetchDoc): TaskDocMetaState {
  const ref = parseBodyDoc(bodyDoc);
  const key = ref?.key ?? null;
  const [state, setState] = useState<{ key: string; value: TaskDocMetaState } | null>(null);

  useEffect(() => {
    if (key === null) {
      return;
    }
    let cancelled = false;
    setState({ key, value: { status: "loading" } });
    fetchDoc(key).then(
      (doc: BoardDoc) => {
        if (!cancelled) {
          setState({ key, value: { status: "ready", meta: doc.format === "markdown" ? parseTaskDocMeta(doc.body) : null } });
        }
      },
      () => {
        if (!cancelled) {
          setState({ key, value: { status: "error" } });
        }
      },
    );
    return () => {
      cancelled = true;
    };
  }, [key, fetchDoc]);

  if (key === null) {
    return { status: "none" };
  }
  return state !== null && state.key === key ? state.value : { status: "loading" };
}
