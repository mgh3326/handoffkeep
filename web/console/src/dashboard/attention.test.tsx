import { render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { Attention } from "./Attention";
afterEach(()=>vi.unstubAllGlobals());
it("labels task/lane counts partial even at zero because escalations are excluded",async()=>{
 vi.stubGlobal("fetch",vi.fn(async()=>new Response(JSON.stringify({tasks:{decisions_pending:0,dispositions_open:2}}))));
 render(<Attention />);
 expect(await screen.findByRole("link",{name:"0 task/lane decisions · 2 dispositions"})).toBeTruthy();
 expect(screen.getByText(/Partial count: answerable escalations are not included/)).toBeTruthy();
 expect(screen.getByRole("link",{name:/0 task\/lane decisions/}).getAttribute("href")).toBe("/ui/decisions");
});
it("keeps an unavailable count distinct from zero",async()=>{
 vi.stubGlobal("fetch",vi.fn(async()=>new Response("{}")));
 render(<Attention />);
 expect(await screen.findByRole("alert")).toBeTruthy();
 expect(screen.getByText("Decision counts unavailable")).toBeTruthy();
 expect(screen.queryByRole("link",{name:/0 task\/lane/})).toBeNull();
});
