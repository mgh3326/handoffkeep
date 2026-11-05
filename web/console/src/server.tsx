import { useLayoutEffect, useRef } from "react";
import { createRoot } from "react-dom/client";
import { bootTheme, PageShell, type NavKey } from "./shared/AppShell";

function ServerContent({ content }: { content: HTMLElement }) {
  const slot = useRef<HTMLDivElement>(null);
  useLayoutEffect(() => {
    slot.current?.append(content);
  }, [content]);
  return <div ref={slot} />;
}
bootTheme();

const edited = new WeakSet<HTMLFormElement>();
document.addEventListener("input", (e) => {
  if (e.target instanceof HTMLElement) {
    const form = e.target.closest("form");
    if (form) edited.add(form);
  }
});
const root = document.getElementById("server-root");
const content = document.getElementById("server-content");
if (root && content) {
  const keys: Record<string, NavKey> = {
    timeline: "timeline",
    decisions: "decisions",
    compose: "compose",
    document: "docs",
  };
  createRoot(root).render(
    <PageShell current={keys[root.dataset.page ?? ""] ?? "home"}>
      <ServerContent content={content} />
    </PageShell>,
  );
}
// Confirm the concrete answer in-page. No navigation shortcut can submit it.

document.addEventListener(
  "submit",
  (e) => {
    const form = e.target;
    if (
      !(form instanceof HTMLFormElement) ||
      !/\/ui\/(decisions|dispositions)\//.test(form.action)
    )
      return;
    e.preventDefault();
    e.stopImmediatePropagation();
    form.querySelector(".hk-confirm")?.remove();
    const submitter =
      e instanceof SubmitEvent && e.submitter instanceof HTMLButtonElement
        ? e.submitter
        : null;
    const box = document.createElement("div");
    box.className = "hk-confirm";
    box.setAttribute("role", "region");
    box.setAttribute("aria-label", "Confirm answer");
    const preview = document.createElement("p");
    const values = Array.from(new FormData(form).entries()).filter(([k]) =>
      /answer|custom|key|note/.test(k),
    );
    preview.textContent = `Confirm this answer: ${
      values
        .map(([, v]) => String(v))
        .filter(Boolean)
        .join(" · ") || "selected recommendation"
    }. Submission uses the existing write gate.`;
    const cancel = document.createElement("button");
    cancel.type = "button";
    cancel.textContent = "Keep editing";
    cancel.onclick = () => {
      box.remove();
      submitter?.focus();
    };
    const send = document.createElement("button");
    send.type = "button";
    send.textContent = "Confirm and submit";
    send.onclick = async () => {
      send.disabled = true;
      cancel.disabled = true;
      const payload = new URLSearchParams();
      for (const [key, value] of new FormData(form).entries())
        payload.append(key, String(value));
      if (submitter?.name) payload.append(submitter.name, submitter.value);
      try {
        const response = await fetch(form.action, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            "Content-Type": "application/x-www-form-urlencoded",
            "HX-Request": "true",
          },
          body: payload,
          signal: AbortSignal.timeout(30000),
        });
        const body = await response.text();
        if (!response.ok)
          throw new Error(
            body.replace(/<[^>]*>/g, " ").slice(0, 300) ||
              `Request failed (${response.status})`,
          );
        const next = document.createElement("template");
        next.innerHTML = body;
        const updated = next.content.querySelector("#ui-content");
        if (!updated)
          throw new Error(
            "Answer response could not be verified. Check request history before retrying.",
          );
        document.getElementById("ui-content")?.replaceWith(updated);
      } catch (err) {
        preview.textContent = `Answer not confirmed. ${err instanceof Error ? err.message : "Connection failed"}. Your selection is retained; check history before retrying.`;
        preview.setAttribute("role", "alert");
        send.disabled = false;
        cancel.disabled = false;
      }
    };
    box.append(preview, cancel, send);
    form.append(box);
    send.focus();
  },
  true,
);
// Keep fragment updates, but never replace an answer the operator is editing.
if (root && ["timeline", "decisions"].includes(root.dataset.page ?? "")) {
  const status = document.createElement("p");
  status.className = "hk-server-sync muted";
  status.setAttribute("role", "status");
  root.append(status);
  const events = new EventSource("/ui/events");
  events.onerror = () => {
    status.textContent = "Updates unavailable · showing last loaded data";
  };
  events.addEventListener("delta", async () => {
    const section = document.getElementById("ui-content");
    if (
      !section?.dataset.uiFragment ||
      section.contains(document.activeElement) ||
      section.querySelector(".hk-confirm") ||
      Array.from(section.querySelectorAll("form")).some((form) =>
        edited.has(form),
      )
    )
      return;
    try {
      const response = await fetch(
        `/ui/fragments/${section.dataset.uiFragment}${location.search}`,
        { credentials: "same-origin" },
      );
      if (response.ok) {
        const html = document.createElement("template");
        html.innerHTML = await response.text();
        section.replaceWith(html.content);
        status.textContent = "";
      } else {
        status.textContent = "Refresh failed · showing last loaded data";
      }
    } catch {
      status.textContent = "Refresh failed · showing last loaded data";
    }
  });
  window.addEventListener("pagehide", () => events.close(), { once: true });
}
