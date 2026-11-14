// Receive + sender pages (docs/M0-TASKS.md steps 7–8).

export function humanBytes(n: number): string {
  const k = 1024;
  if (n >= k * k * k * k) return `${(n / (k * k * k * k)).toFixed(1)} TiB`;
  if (n >= k * k * k) return `${(n / (k * k * k)).toFixed(1)} GiB`;
  if (n >= k * k) return `${(n / (k * k)).toFixed(1)} MiB`;
  if (n >= k) return `${(n / k).toFixed(1)} KiB`;
  return `${n} B`;
}

export function fmtPct(done: number, total: number): string {
  if (total <= 0) return "100%";
  return `${Math.floor((done / total) * 100)}%`;
}

export const sleep = (ms: number) => new Promise((res) => setTimeout(res, ms));