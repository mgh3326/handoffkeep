import { fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, expect, it } from "vitest";
import { OriginalPreference, TranslatedText } from "./translation";
import { STORAGE_KEYS } from "./storage";

beforeEach(() => localStorage.clear());
it("shows fresh Korean first and toggles each item and the global preference", () => {
  render(<><OriginalPreference /><TranslatedText original="Original" korean="번역" /><TranslatedText original="Second" korean="두 번째" /></>);
  expect(screen.getByText("번역")).toBeTruthy();
  fireEvent.click(screen.getAllByRole("button", { name: "원문" })[0]);
  expect(screen.getByText("Original")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "원문 전체 보기" }));
  expect(screen.getByText("Second")).toBeTruthy();
  expect(localStorage.getItem(STORAGE_KEYS.showOriginal)).toBe("true");
  fireEvent.click(screen.getByRole("button", { name: "원문 전체 보기" }));
  expect(screen.getByText("번역")).toBeTruthy();
  expect(screen.getByText("두 번째")).toBeTruthy();
});
it("marks stale translations and keeps the original visible", () => {
  render(<TranslatedText original="Updated" korean="오래된 번역" stale />);
  expect(screen.getByText("Updated")).toBeTruthy();
  expect(screen.getByText("갱신 필요")).toBeTruthy();
  expect(screen.queryByText("오래된 번역")).toBeNull();
});
