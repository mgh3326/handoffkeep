// Keyboard helpers shared by the console's views. The task search client
// (/ui/api/search) was removed with the server-rendered console; only the
// typing-target guard that global shortcuts check is still in use.

const TEXT_INPUT_TYPES = new Set(["", "text", "search", "email", "url", "tel", "password", "number", "date", "datetime-local", "month", "time", "week"]);

/** True when a key press belongs to whatever the viewer is typing into:
 * text inputs, textareas, selects and contenteditable regions. Global
 * shortcuts never fire there. */
export function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) {
    return false;
  }
  if (target.isContentEditable || target.closest("[contenteditable]:not([contenteditable='false'])") !== null) {
    return true;
  }
  if (target instanceof HTMLTextAreaElement || target instanceof HTMLSelectElement) {
    return true;
  }
  if (target instanceof HTMLInputElement) {
    return TEXT_INPUT_TYPES.has(target.type.toLowerCase());
  }
  return false;
}
