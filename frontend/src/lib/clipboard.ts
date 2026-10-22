import { t as translate } from "./i18n";
export async function copyText(value: string): Promise<void> {
  if (!value) throw new Error(translate("没有可复制的内容。"));
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value);
      return;
    } catch {}
  }
  const previous =
    document.activeElement instanceof HTMLElement
      ? document.activeElement
      : undefined;
  const selection = document.getSelection();
  const range = selection?.rangeCount
    ? selection.getRangeAt(0).cloneRange()
    : undefined;
  const input = document.createElement("textarea");
  input.value = value;
  input.readOnly = true;
  input.setAttribute("aria-hidden", "true");
  input.style.cssText = "position:fixed;left:-9999px;top:0;opacity:0";
  document.body.appendChild(input);
  try {
    input.select();
    if (!document.execCommand("copy"))
      throw new Error(translate("浏览器未允许自动复制，请手动选择复制。"));
  } finally {
    input.remove();
    previous?.focus({ preventScroll: true });
    if (selection && range) {
      selection.removeAllRanges();
      selection.addRange(range);
    }
  }
}
