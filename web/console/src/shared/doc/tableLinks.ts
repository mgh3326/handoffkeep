// Table links inside a combined tbls document. The tbls combine step writes a
// `<!-- file: NAME.md -->` comment above each table's file, and a table links
// to its neighbours as `[name](name.md)`. The console turns each marker into
// an anchor and each such link into a fragment to that anchor; a link to a
// file the document does not hold renders as text. Pure helpers, plus the
// remark plugin DocMarkdown wires in. Only this lazy renderer chunk uses them.

/** rehype-sanitize's default clobber prefix: every `id` leaves the sanitizer
 * with it, so an in-document fragment must carry it too. */
export const SANITIZED_ID_PREFIX = "user-content-";

const MARKER = /^<!--\s*file:\s*([A-Za-z0-9_][A-Za-z0-9_.-]*\.md)\s*-->$/;
const FILE_LINK = /^([A-Za-z0-9_][A-Za-z0-9_.-]*\.md)(#.*)?$/;

/** The file name a marker comment names, or null for any other HTML. */
export function markerFileName(html: string): string | null {
  const match = MARKER.exec(html.trim());
  return match ? match[1] : null;
}

/** The id of the anchor emitted at a file's marker, before sanitizing. */
export function fileAnchorId(name: string): string {
  return `file-${name}`;
}

export type TableLink = { href: string } | { dead: true };

/** Resolves one link against the files a document holds. Returns null when
 * the link is not a relative `X.md` or `X.md#frag` (the caller keeps it as it
 * is). A file the document holds becomes a fragment to its marker's anchor;
 * the `#frag` part is dropped, since the document has no anchors below a
 * marker. A file it does not hold is dead: the link renders as text. */
export function resolveTableLink(url: string, files: ReadonlySet<string>): TableLink | null {
  const match = FILE_LINK.exec(url);
  if (!match) {
    return null;
  }
  if (!files.has(match[1])) {
    return { dead: true };
  }
  return { href: `#${SANITIZED_ID_PREFIX}${fileAnchorId(match[1])}` };
}

type MdNode = {
  type: string;
  value?: string;
  url?: string;
  children?: MdNode[];
  data?: Record<string, unknown>;
};

function walk(node: MdNode, visit: (node: MdNode) => void): void {
  visit(node);
  for (const child of node.children ?? []) {
    walk(child, visit);
  }
}

function anchorNode(name: string): MdNode {
  // An empty paragraph that the renderer emits as an empty <span id=...>.
  return { type: "paragraph", children: [], data: { hName: "span", hProperties: { id: fileAnchorId(name) } } };
}

/** remark plugin: top-level file markers become anchors, and the relative
 * table links they make resolvable are rewritten. A document with no marker
 * is left untouched, so its links behave as they always did. */
export function remarkTableLinks() {
  return (tree: MdNode): void => {
    const files = new Set<string>();
    for (const node of tree.children ?? []) {
      const name = node.type === "html" ? markerFileName(node.value ?? "") : null;
      if (name !== null) {
        files.add(name);
      }
    }
    if (files.size === 0) {
      return;
    }
    tree.children = (tree.children ?? []).map((node) => {
      const name = node.type === "html" ? markerFileName(node.value ?? "") : null;
      return name === null ? node : anchorNode(name);
    });
    walk(tree, (node) => {
      if (node.type !== "link") {
        return;
      }
      const link = resolveTableLink(node.url ?? "", files);
      if (link === null) {
        return;
      }
      // An empty url is the renderer's dead link (see DocMarkdown's `a`).
      node.url = "dead" in link ? "" : link.href;
    });
  };
}
