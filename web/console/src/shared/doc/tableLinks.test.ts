import { describe, expect, it } from "vitest";
import { markerFileName, resolveTableLink } from "./tableLinks";

describe("markerFileName", () => {
  it("reads the file a tbls marker names", () => {
    expect(markerFileName("<!-- file: tasks.md -->")).toBe("tasks.md");
    expect(markerFileName("<!--file:a_b-1.md-->")).toBe("a_b-1.md");
  });

  it("ignores other comments and paths that are not plain file names", () => {
    expect(markerFileName("<!-- note -->")).toBeNull();
    expect(markerFileName("<!-- file: ../secret.md -->")).toBeNull();
    expect(markerFileName("<!-- file: tasks.txt -->")).toBeNull();
  });
});

describe("resolveTableLink", () => {
  const files = new Set(["tasks.md", "decisions.md"]);

  it("turns a link to a file in the document into a fragment to its anchor", () => {
    expect(resolveTableLink("tasks.md", files)).toEqual({ href: "#user-content-file-tasks.md" });
    expect(resolveTableLink("decisions.md#columns", files)).toEqual({ href: "#user-content-file-decisions.md" });
  });

  it("makes a link to a file the document does not hold dead", () => {
    expect(resolveTableLink("missing.md", files)).toEqual({ dead: true });
    expect(resolveTableLink("missing.md#x", files)).toEqual({ dead: true });
  });

  it("leaves every other url to the existing URL policy", () => {
    for (const url of ["https://example.com/tasks.md", "/tasks.md", "//host/tasks.md", "#intro", "tasks.txt", "../tasks.md"]) {
      expect(resolveTableLink(url, files), url).toBeNull();
    }
  });
});
