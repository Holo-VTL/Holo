export function formatBytes(value: number): string {
  if (!Number.isFinite(value) || value < 0) {
    return "0 B";
  }
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let size = value;
  let idx = 0;
  while (size >= 1024 && idx < units.length - 1) {
    size /= 1024;
    idx += 1;
  }
  const digits = size >= 100 ? 0 : size >= 10 ? 1 : 2;
  return `${size.toFixed(digits)} ${units[idx]}`;
}

export function isoNow(): string {
  return new Date().toISOString();
}

export function toLocalDatetimeValue(date = new Date()): string {
  const pad = (n: number) => `${n}`.padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

export function parseRulesJson(input: string): unknown {
  return JSON.parse(input);
}

export function safeString(value: unknown): string {
  if (value === null || value === undefined) {
    return "";
  }
  return String(value);
}
