import type { ReactNode } from "react";
import { Icon } from "../shared/Icon";
import { inlineSrc, MAX_INLINE_BYTES, mediaKind } from "../shared/media";
import { DecisionChip } from "./DecisionChip";
import type { Option, OptionAttachment } from "./types";

// One option of a decision, as a card: its key, its label and chips, and the
// pictures and videos the operator compares it by (hk 4788). The card is the
// layout unit so media sits inside it without nesting interactive content in
// the radio control.
export function OptionCard({
  option,
  index,
  label,
  answerable,
  selected,
  editable,
  defaultOption,
  note,
  onSelect,
}: {
  option: Option;
  index: number;
  label: string;
  answerable: boolean;
  selected: boolean;
  editable: boolean;
  defaultOption?: string;
  note?: ReactNode;
  onSelect?: () => void;
}) {
  return (
    <div
      className="decision-option-card"
      data-option-key={option.key}
      data-selected={selected ? "true" : undefined}
      data-answerable={answerable ? "true" : undefined}
    >
      <span className="decision-option-key" aria-hidden="true">
        {option.key}
      </span>
      <button
        type="button"
        role={answerable ? "radio" : undefined}
        aria-checked={answerable ? selected : undefined}
        disabled={answerable ? !editable : undefined}
        className="decision-option"
        onClick={onSelect}
      >
        <span className="decision-option-mark" aria-hidden="true">
          {selected && <Icon name="check" />}
        </span>
        <span className="decision-option-body">
          <strong>
            {index + 1}. {label}
          </strong>
          <span className="hk-chips">
            {note}
            {option.recommended && (
              <DecisionChip group="done" label="권장" icon="check" />
            )}
            {defaultOption === option.key && (
              <DecisionChip group="neutral" label="미응답 시 기본값" />
            )}
          </span>
        </span>
      </button>
      <OptionMedia attachments={option.attachments} />
    </div>
  );
}

// OptionMedia shows each attachment in place: a picture renders where it
// sits, a video renders with controls. The server's allowlist and the size
// cap are the task page's (shared/media.ts), so a type it will not serve
// inline, a file above the cap, and a file the server no longer holds stay a
// plain link rather than a picture that cannot decode.
export function OptionMedia({
  attachments,
}: {
  attachments?: OptionAttachment[];
}) {
  if (!attachments || attachments.length === 0) return null;
  return (
    <div className="decision-option-media">
      {attachments.map((attachment) => {
        const url = inlineSrc(attachment.sha256);
        const caption =
          attachment.caption ||
          attachment.original_name ||
          `${attachment.sha256.slice(0, 12)}…`;
        const kind =
          attachment.size_bytes !== undefined &&
          attachment.size_bytes > MAX_INLINE_BYTES
            ? null
            : mediaKind(attachment.mime);
        if (kind === "image")
          return (
            <figure key={attachment.sha256}>
              <a href={url} target="_blank" rel="noreferrer">
                <img src={url} alt={caption} loading="lazy" />
              </a>
              {attachment.caption && <figcaption>{attachment.caption}</figcaption>}
            </figure>
          );
        if (kind === "video")
          return (
            <figure key={attachment.sha256}>
              <video controls preload="metadata" src={url} aria-label={caption} />
              <figcaption>
                <a href={url} target="_blank" rel="noreferrer">
                  {caption}
                </a>
              </figcaption>
            </figure>
          );
        return (
          <a key={attachment.sha256} href={url} target="_blank" rel="noreferrer">
            {caption}
          </a>
        );
      })}
    </div>
  );
}
