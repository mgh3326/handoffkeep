import { useEffect, useRef } from "react";
export function ConfirmSheet({
  option,
  text,
  revision,
  onEdit,
  onCancel,
  onConfirm,
}: {
  option: string;
  text: string;
  revision: number;
  onEdit: () => void;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const edit = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    edit.current?.focus();
  }, []);
  const close = () => {
    if (!text || window.confirm("작성한 메모를 닫으시겠습니까?")) onCancel();
  };
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        close();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });
  return (
    <div className="decision-scrim">
      <section
        className="decision-sheet"
        role="dialog"
        aria-modal="true"
        aria-label="답변 확인"
      >
        <h2>답변 확인</h2>
        <p>선택: {option || "직접 입력"}</p>
        <p>메모: {text || "없음"}</p>
        <p>revision {revision}</p>
        <p>직접 답(operator_direct)으로 기록됩니다</p>
        <div className="decision-actions">
          <button ref={edit} onClick={onEdit}>
            수정
          </button>
          <button onClick={close}>취소</button>
          <button onClick={onConfirm}>답변 보내기</button>
        </div>
      </section>
    </div>
  );
}
