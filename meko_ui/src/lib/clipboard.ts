export async function copyToClipboard(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
      await navigator.clipboard.writeText(text);
      return true;
    }

    const helper = document.createElement("textarea");
    helper.value = text;
    helper.setAttribute("readonly", "true");
    helper.style.position = "fixed";
    helper.style.opacity = "0";
    helper.style.pointerEvents = "none";
    helper.style.left = "-9999px";
    document.body.appendChild(helper);

    helper.focus();
    helper.select();
    helper.setSelectionRange(0, helper.value.length);

    const copied = document.execCommand("copy");
    document.body.removeChild(helper);
    return copied;
  } catch {
    return false;
  }
}
