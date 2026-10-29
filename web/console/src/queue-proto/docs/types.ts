// Wire types for the console document browser (GET /ui/api/docs and
// /ui/api/docs/prefixes, hk 2245). Listing rows never carry bodies — the
// reader fetches the selected body through /ui/api/board/doc.

export type DocSummary = {
  key: string;
  title: string;
  kind: string;
  session: string;
  size: number;
  updated_at: string;
};

export type DocsResponse = {
  prefix: string;
  limit: number;
  documents: DocSummary[];
  has_more: boolean;
  next_before?: string;
};

export type DocPrefix = {
  prefix: string;
  count: number;
};

export type DocsPrefixesResponse = {
  prefixes: DocPrefix[];
};
