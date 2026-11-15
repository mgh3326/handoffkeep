// The task page's 첨부 section: images and videos shown in place (through
// /ui/files/<sha>?inline=1, which serves only an allowlisted media type
// inline), a side-by-side compare view for two or more chosen items, and a
// full-viewport enlarge view. Everything else stays a download link.
import { useCallback, useEffect, useRef, useState } from "react";
import type { BoardAttachment } from "../shared/boardTypes";
import { inlineSrc, MAX_INLINE_BYTES, mediaKind } from "../shared/media";
import type { MediaKind } from "../shared/media";
import { isTypingTarget } from "../shared/search";
import { formatMinuteKST } from "../shared/time";

type MediaItem = { attachment: BoardAttachment; kind: MediaKind };

function Media({ item }: { item: MediaItem }) {
  if (item.kind === "image") {
    return <img src={inlineSrc(item.attachment.sha256)} alt={item.attachment.original_name} loading="lazy" />;
  }
  return <video src={inlineSrc(item.attachment.sha256)} aria-label={item.attachment.original_name} controls preload="metadata" />;
}

function CompareView({ items, onEnlarge }: { items: MediaItem[]; onEnlarge: (index: number) => void }) {
  return (
    <section className="work-compare" aria-label="첨부 비교">
      <header className="work-compare-head"><h3>비교 · {items.length}개</h3></header>
      <div className="work-compare-grid">
        {items.map((item, index) => (
          <figure key={item.attachment.sha256} className="work-compare-item">
            <div className="work-compare-media"><Media item={item} /></div>
            <figcaption>
              <span>{item.attachment.original_name}</span>
              <button type="button" className="hk-button" aria-label={`${item.attachment.original_name} 크게 보기`} onClick={() => onEnlarge(index)}>크게 보기</button>
            </figcaption>
          </figure>
        ))}
      </div>
    </section>
  );
}

function EnlargeView({ items, index, onClose, onStep }: { items: MediaItem[]; index: number; onClose: () => void; onStep: (delta: number) => void }) {
  const item = items[index];
  const closeRef = useRef<HTMLButtonElement>(null);
  useEffect(() => { closeRef.current?.focus(); }, []);
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (isTypingTarget(event.target)) return;
      if (event.key === "Escape") {
        event.preventDefault();
        onClose();
      } else if (event.key === "ArrowLeft") {
        event.preventDefault();
        onStep(-1);
      } else if (event.key === "ArrowRight") {
        event.preventDefault();
        onStep(1);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose, onStep]);
  return (
    <div
      className="work-enlarge"
      role="dialog"
      aria-modal="true"
      aria-label={`${item.attachment.original_name} 크게 보기`}
      onClick={event => { if (event.target === event.currentTarget) onClose(); }}
    >
      <div className="work-enlarge-bar">
        <button type="button" className="hk-button" aria-label="이전" onClick={() => onStep(-1)}>←</button>
        <span>{item.attachment.original_name} · {index + 1}/{items.length}</span>
        <button type="button" className="hk-button" aria-label="다음" onClick={() => onStep(1)}>→</button>
        <button ref={closeRef} type="button" className="hk-button" onClick={onClose}>닫기</button>
      </div>
      <Media item={item} />
    </div>
  );
}

export function TaskAttachments({ attachments }: { attachments: BoardAttachment[] }) {
  const [selected, setSelected] = useState<string[]>([]);
  const [enlarged, setEnlarged] = useState<number | null>(null);
  const opener = useRef<HTMLElement | null>(null);
  // An archived file has no hot copy, so it is never rendered in place: the
  // line below names where the archive holds it.
  const archived = attachments.filter(attachment => attachment.archive_location !== "");
  const live = attachments.filter(attachment => attachment.archive_location === "");
  // Above the threshold an image or video keeps the plain link line below.
  const items: MediaItem[] = live.flatMap(attachment => {
    const kind = mediaKind(attachment.mime);
    return kind !== null && attachment.size_bytes <= MAX_INLINE_BYTES ? [{ attachment, kind }] : [];
  });
  // Derived, so a list that reloads under a different task drops stale picks.
  const chosen = items.filter(item => selected.includes(item.attachment.sha256));
  // Too large to show in place: the link and its size, nothing else.
  const oversized = live.filter(attachment => mediaKind(attachment.mime) !== null && attachment.size_bytes > MAX_INLINE_BYTES);
  const links = live.filter(attachment => mediaKind(attachment.mime) === null);
  const toggle = (sha: string) => setSelected(old => old.includes(sha) ? old.filter(value => value !== sha) : [...old, sha]);
  const close = useCallback(() => {
    setEnlarged(null);
    opener.current?.focus();
    opener.current = null;
  }, []);
  const step = useCallback((delta: number) => {
    setEnlarged(index => index === null ? null : (index + delta + chosen.length) % chosen.length);
  }, [chosen.length]);
  if (!attachments.length) return <p>첨부 없음</p>;
  return (
    <>
      {items.length > 0 && (
        <div className="work-media">
          <p className="work-media-hint">이미지·동영상 {items.length}개 · 두 개 이상 고르면 나란히 비교합니다</p>
          <ul className="work-media-grid" aria-label="이미지·동영상 첨부">
            {items.map(item => {
              const sha = item.attachment.sha256;
              const on = selected.includes(sha);
              return (
                <li key={sha} className="work-media-item" data-selected={on ? "true" : undefined}>
                  <figure>
                    <Media item={item} />
                    <figcaption><a href={`/ui/files/${sha}`}>{item.attachment.original_name}</a> · {item.attachment.size_bytes} bytes</figcaption>
                  </figure>
                  <button type="button" className="hk-button work-media-select" aria-pressed={on} onClick={() => toggle(sha)}>
                    {on ? "비교 해제" : "비교 선택"}
                  </button>
                </li>
              );
            })}
          </ul>
        </div>
      )}
      {chosen.length >= 2 && <CompareView items={chosen} onEnlarge={index => { opener.current = document.activeElement as HTMLElement | null; setEnlarged(index); }} />}
      {oversized.map(attachment => <p key={attachment.sha256}><a href={`/ui/files/${attachment.sha256}`}>{attachment.original_name}</a> · {attachment.size_bytes} bytes</p>)}
      {links.map(attachment => <p key={attachment.sha256}><a href={`/ui/files/${attachment.sha256}`}>{attachment.original_name}</a> · {attachment.size_bytes} bytes · {attachment.mime} · {formatMinuteKST(attachment.created_at)} · {attachment.created_by}</p>)}
      {archived.map(attachment => <p key={attachment.sha256}><a href={`/ui/files/${attachment.sha256}`}>{attachment.original_name}</a> · {attachment.size_bytes} bytes · 보관됨 · {attachment.archive_location}</p>)}
      {enlarged !== null && chosen[enlarged] && <EnlargeView items={chosen} index={enlarged} onClose={close} onStep={step} />}
    </>
  );
}
