import type { ReactNode } from "react";
import { Icon } from "../shared/Icon";
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

// fileURL is the one place the page names a stored file. inline=1 asks for the
// object with an inline disposition, so the browser renders it instead of
// downloading it (hk 4787; the console's CSP admits the object store's origin
// for exactly these two types).
function fileURL(sha256: string) {
  return `/ui/files/${sha256}?inline=1`;
}

function captionOf(attachment: OptionAttachment) {
  return (
    attachment.caption ||
    attachment.original_name ||
    `${attachment.sha256.slice(0, 12)}…`
  );
}

// OptionMedia shows each attachment in place: a picture renders where it
// sits, a video renders with controls. A type the page cannot show, and a
// file the server no longer holds, stay a plain link.
export function OptionMedia({
  attachments,
}: {
  attachments?: OptionAttachment[];
}) {
  if (!attachments || attachments.length === 0) return null;
  return (
    <div className="decision-option-media">
      {attachments.map((attachment) => {
        const url = fileURL(attachment.sha256);
        const caption = captionOf(attachment);
        if (attachment.mime?.startsWith("image/"))
          return (
            <figure key={attachment.sha256}>
              <a href={url} target="_blank" rel="noreferrer">
                <img src={url} alt={caption} loading="lazy" />
              </a>
              {attachment.caption && <figcaption>{attachment.caption}</figcaption>}
            </figure>
          );
        if (attachment.mime?.startsWith("video/"))
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
