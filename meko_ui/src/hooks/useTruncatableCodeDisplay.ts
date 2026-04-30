import { useCallback, useEffect, useState } from "react";

/**
 * Collapsible preview for long monospace / shell snippets. Copy handlers should still use the full `text`.
 * Collapses again when `collapseVersion` or `text` changes (e.g. dialog close, new provision, new step code).
 */
export function useTruncatableCodeDisplay(text: string, previewMaxChars: number, collapseVersion: string) {
  const [expanded, setExpanded] = useState(false);
  const needsToggle = text.length > previewMaxChars;
  const displayText =
    needsToggle && !expanded ? `${text.slice(0, previewMaxChars)}\u2026` : text;

  useEffect(() => {
    setExpanded(false);
  }, [text, collapseVersion]);

  const toggleExpanded = useCallback(() => {
    setExpanded((v) => !v);
  }, []);

  return { displayText, needsToggle, expanded, toggleExpanded };
}
