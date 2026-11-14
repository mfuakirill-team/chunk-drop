import type { ManifestFile } from "./api";

// Save layer (M0): desktop Chromium → File System Access API writes the
// real tree; everything else → per-file <a download> (PROTOCOL.md §4).

export interface Saver {
  /** Called once per file before chunks arrive (create parent dirs). */
  openFile: (f: ManifestFile) => Promise<void>;
  /** Called per verified chunk, in order. */
  writeChunk: (f: ManifestFile, chunk: number, bytes: Uint8Array) => Promise<void>;
  /** Called when all chunks of a file have been written. */
  closeFile: (f: ManifestFile) => Promise<void>;
}

export function supportsDirectorySave(): boolean {
  return typeof (window as unknown as { showDirectoryPicker?: unknown }).showDirectoryPicker === "function";
}

interface DirHandleLike {
  getFileHandle: (name: string, opts?: { create?: boolean }) => Promise<{ createWritable: () => Promise<{ write: (b: Uint8Array) => Promise<void>; close: () => Promise<void> }> }>;
  getDirectoryHandle: (name: string, opts?: { create?: boolean }) => Promise<DirHandleLike>;
}

/** Ask the user for a real directory, then save the manifest's tree into it. */
export async function newDirectorySaver(rootName: string): Promise<Saver | null> {
  if (!supportsDirectorySave()) return null;
  const w = window as unknown as {
    showDirectoryPicker: (o?: { mode?: string; startIn?: string }) => Promise<DirHandleLike>;
  };
  let root: DirHandleLike;
  try {
    root = await w.showDirectoryPicker({ mode: "readwrite", startIn: "downloads" });
  } catch {
    return null; // user dismissed the picker → fall back to <a download>
  }
  const dirs = new Map<string, DirHandleLike>([[rootName, root]]);

  const dirFor = async (rel: string): Promise<DirHandleLike> => {
    const parts = rel.split("/").slice(0, -1); // all but filename
    let cur = root;
    let prefix = "";
    for (const part of parts) {
      prefix = prefix ? `${prefix}/${part}` : part;
      let next = dirs.get(prefix);
      if (!next) {
        next = await cur.getDirectoryHandle(part, { create: true });
        dirs.set(prefix, next);
      }
      cur = next;
    }
    return cur;
  };

  const writers = new Map<number, { write: (b: Uint8Array) => Promise<void>; close: () => Promise<void> }>();

  return {
    async openFile(f) {
      const dir = await dirFor(`${rootName ? rootName + "/" : ""}${f.path}`);
      const name = f.path.split("/").pop() || `file-${f.index}`;
      const fh = await dir.getFileHandle(name, { create: true });
      const writable = await (fh as unknown as { createWritable: () => Promise<{ write: (b: Uint8Array) => Promise<void>; close: () => Promise<void> }> }).createWritable();
      writers.set(f.index, writable);
    },
    async writeChunk(f, _chunk, bytes) {
      const w = writers.get(f.index);
      if (!w) throw new Error(`saver: file ${f.path} not open`);
      await w.write(bytes);
    },
    async closeFile(f) {
      const w = writers.get(f.index);
      if (!w) return;
      writers.delete(f.index);
      await w.close();
    },
  };
}

/** Fallback saver: per-file <a download> for browsers without FSA. */
export function downloadSaver(): Saver {
  const blobs = new Map<number, Blob[]>();
  return {
    async openFile(f) {
      blobs.set(f.index, []);
    },
    async writeChunk(f, _chunk, bytes) {
      const parts = blobs.get(f.index);
      if (!parts) throw new Error(`saver: file ${f.path} not open`);
      parts.push(new Blob([bytes as unknown as BlobPart]));
    },
    async closeFile(f) {
      const parts = blobs.get(f.index);
      blobs.delete(f.index);
      if (!parts) return;
      const blob = new Blob(parts as BlobPart[], { type: "application/octet-stream" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = f.path.split("/").pop() || `file-${f.index}`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 60_000);
    },
  };
}