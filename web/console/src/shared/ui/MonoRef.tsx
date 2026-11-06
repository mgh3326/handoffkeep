// A shortened id in Mono (a SHA or digest cut to its first characters) with a
// copy button. The full value stays in the title and is what the button
// copies. A value that needed no shortening gets no button. The result of a
// copy is spoken through the shell's one polite region (hk-console-announce);
// the button's name never changes.

import { useEffect, useRef, useState } from "react";
import { IconButton } from "../Icon";
import { Mono } from "./Mono";
import "./MonoRef.css";

const COPIED_MS = 1600;

function announce(text: string) {
  window.dispatchEvent(new CustomEvent("hk-console-announce", { detail: text }));
}

export function MonoRef({ value, short, label = "전체 값 복사" }: { value: string; short: string; label?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      announce("복사하지 못함");
      return;
    }
    setCopied(true);
    announce("복사함");
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setCopied(false), COPIED_MS);
  };

  return (
    <span className="hk-monoref">
      <span title={value}>
        <Mono>{short}</Mono>
      </span>
      {short !== value ? (
        <IconButton icon={copied ? "check" : "copy"} label={label} className="hk-monoref-copy" onClick={copy} data-copied={copied} />
      ) : null}
    </span>
  );
}
