// #k= fragment key parsing + receive URL building (docs/PROTOCOL.md §2).

export function parseFragmentKey(hash: string): string {
  if (!hash.startsWith("#k=")) return "";
  try {
    return decodeURIComponent(hash.slice(3));
  } catch {
    return "";
  }
}

export function buildReceiveUrl(origin: string, key: string): string {
  return `${origin}/r/#k=${encodeURIComponent(key)}`;
}

export function setFragment(key: string): void {
  const h = key ? `#k=${encodeURIComponent(key)}` : "";
  if (location.hash !== h) history.replaceState(null, "", h);
}