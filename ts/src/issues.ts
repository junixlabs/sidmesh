export interface Issue {
  code: string;
  /** Path relative to the project root, with forward slashes. */
  file: string;
  /** JSON Pointer into the file; empty string for the whole document. */
  pointer: string;
  message: string;
}

export class IssueList {
  readonly items: Issue[] = [];

  add(code: string, file: string, pointer: string, message: string): void {
    this.items.push({ code, file, pointer, message });
  }
}

/** Escape one JSON Pointer reference token (RFC 6901). */
function token(key: string | number): string {
  return String(key).replace(/~/g, "~0").replace(/\//g, "~1");
}

export function pointer(...keys: (string | number)[]): string {
  return keys.map((k) => "/" + token(k)).join("");
}

export function formatIssue(issue: Issue): string {
  const where = issue.pointer ? `${issue.file}#${issue.pointer}` : issue.file;
  return `${issue.code}  ${where}  ${issue.message}`;
}
