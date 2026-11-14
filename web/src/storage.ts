// Group-key persistence (#2): localStorage was wiped because the key
// state was initialized before hydration and overwritten on save with
// the empty value; persist explicitly on every change and hydrate on
// module load.

const KEY_STORAGE = "chunkdrop.group_key";

export function loadGroupKey(): string {
  try {
    return localStorage.getItem(KEY_STORAGE) ?? "";
  } catch {
    return ""; // private mode / disabled storage: degrade gracefully
  }
}

export function saveGroupKey(key: string): void {
  try {
    if (key) {
      localStorage.setItem(KEY_STORAGE, key);
    } else {
      localStorage.removeItem(KEY_STORAGE);
    }
  } catch {
    /* storage unavailable: the dashboard still works per-tab */
  }
}