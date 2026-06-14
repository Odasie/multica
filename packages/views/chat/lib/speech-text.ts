/**
 * Flatten an assistant reply's markdown into plain text suitable for
 * text-to-speech. The goal isn't a perfect markdown parser — it's to stop the
 * voice from reading syntax aloud ("asterisk asterisk", "backtick", raw URLs),
 * while preserving the words a listener cares about.
 *
 * Used by Parley auto-speak before handing the text to `useTextToSpeech`.
 */
export function plainTextForSpeech(markdown: string): string {
  let text = markdown;

  // Fenced code blocks — drop entirely (reading code aloud is noise).
  text = text.replace(/```[\s\S]*?```/g, " ");
  // Inline code — keep the contents, drop the backticks.
  text = text.replace(/`([^`]+)`/g, "$1");
  // Images: ![alt](url) — read the alt text only.
  text = text.replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1");
  // Links: [label](url) — read the label only.
  text = text.replace(/\[([^\]]+)\]\([^)]*\)/g, "$1");
  // Headings / blockquote markers at line start.
  text = text.replace(/^\s{0,3}#{1,6}\s+/gm, "");
  text = text.replace(/^\s{0,3}>\s?/gm, "");
  // Unordered list bullets at line start.
  text = text.replace(/^\s*[-*+]\s+/gm, "");
  // Emphasis / bold / strikethrough markers.
  text = text.replace(/(\*\*|__|\*|_|~~)/g, "");
  // Collapse runs of whitespace/newlines into single spaces.
  text = text.replace(/\s+/g, " ").trim();

  return text;
}
