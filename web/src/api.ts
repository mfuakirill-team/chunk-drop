import { useEffect } from "preact/hooks";
import { createSHA256 } from "hash-wasm";

// Module: api.ts — typed client for the engine HTTP API
// (docs/PROTOCOL.md §4). Every authenticated call carries the session
// key from the #k= URL fragment as the X-ChunkDrop-Key header — the key
// itself never appears in any request URL (fragments aren't sent).

export interface ManifestFile {
  index: number;
  path: string;
  size: number;
  chunkCount: number;
  hashes: string[];
}

export interface Manifest {
  version: number;
  sessionId: string;
  createdAt: string;
  chunkSize: number;
  packedAsZip: boolean;
  files: ManifestFile[];
}

export interface SessionInfo {
  session: {
    id: string;
    createdAt: string;
    expiresAt: string;
    fileCount: number;
    totalBytes: number;
    packedAsZip: boolean;
    paths: string[];
  };
  receiver?: {
    receiver: string;
    updatedAt: string;
    have: Record<string, number[]>;
  };
  progressAgoMs?: number;
}

export const KEY_HEADER = "X-ChunkDrop-Key";

/** Read the session key from the URL fragment (#k=…). */
export function readKeyFromFragment(): string {
  const h = location.hash;
  if (h.startsWith("#k=")) return decodeURIComponent(h.slice(3));
  return "";
}

function headers(key: string): HeadersInit {
  return { [KEY_HEADER]: key };
}

export async function fetchManifest(key: string): Promise<Manifest> {
  const r = await fetch("/api/manifest", { headers: headers(key) });
  if (!r.ok) throw httpError(r);
  return r.json();
}

export async function fetchSession(key: string): Promise<SessionInfo> {
  const r = await fetch("/api/session", { headers: headers(key) });
  if (!r.ok) throw httpError(r);
  return r.json();
}

export async function postProgress(
  key: string,
  have: Record<string, number[]>,
  receiver: string,
): Promise<void> {
  const r = await fetch("/api/progress", {
    method: "POST",
    headers: { ...headers(key), "Content-Type": "application/json" },
    body: JSON.stringify({ receiver, have }),
  });
  if (!r.ok) throw httpError(r);
}

export async function postCancel(key: string): Promise<void> {
  const r = await fetch("/api/cancel", {
    method: "POST",
    headers: headers(key),
  });
  if (!r.ok) throw httpError(r);
}

export function httpError(r: Response): Error & { status?: number } {
  const err = new Error(`HTTP ${r.status}`) as Error & { status?: number };
  err.status = r.status;
  return err;
}

// ---------------------------------------------------------------------------
// Chunk pipeline (docs/PROTOCOL.md §4 receiver behavior, normative):
// sequential per file, 2–3 in flight, hash-wasm SHA-256 verify,
// 3 retries with backoff, per-chunk error surface — a bad moment on
// WiFi never restarts the transfer.
// ---------------------------------------------------------------------------

export type ChunkSink = (file: ManifestFile, chunk: number, bytes: Uint8Array) => Promise<void>;

export interface TransferCallbacks {
  onFileStart: (file: ManifestFile) => void;
  onFileDone: (file: ManifestFile) => void;
  onChunk: (file: ManifestFile, chunk: number, done: number, total: number) => void;
  onChunkError: (file: ManifestFile, chunk: number, err: Error, attempt: number) => void;
  onProgress: (have: Record<string, number[]>) => void;
}

let hasher: Awaited<ReturnType<typeof createSHA256>> | null = null;
async function getHasher() {
  if (!hasher) hasher = await createSHA256();
  return hasher;
}

export async function sha256Hex(data: Uint8Array): Promise<string> {
  const h = await getHasher();
  await h.init();
  await h.update(data);
  return h.digest("hex");
}

const RETRIES = 3;
const IN_FLIGHT = 3;

async function fetchChunk(
  key: string,
  fileIndex: number,
  chunk: number,
): Promise<{ bytes: Uint8Array; hash: string | null }> {
  const r = await fetch(`/api/chunk/${fileIndex}/${chunk}`, {
    headers: headers(key),
  });
  if (!r.ok) throw httpError(r);
  const hash = r.headers.get("X-Chunk-Hash");
  const bytes = new Uint8Array(await r.arrayBuffer());
  return { bytes, hash };
}

const sleep = (ms: number) => new Promise((res) => setTimeout(res, ms));

export interface DownloadHandle {
  cancel: () => void;
  done: Promise<void>;
}

/** Run the full download pipeline for a manifest. */
export function download(
  manifest: Manifest,
  key: string,
  sink: ChunkSink,
  cb: TransferCallbacks,
): DownloadHandle {
  let cancelled = false;
  const have: Record<string, number[]> = {};

  const done = (async () => {
    for (const file of manifest.files) {
      if (cancelled) return;
      if (file.chunkCount === 0) {
        // empty file: create it via the sink, mark done
        await sink(file, 0, new Uint8Array(0));
        have[String(file.index)] = [];
        cb.onFileStart(file);
        cb.onFileDone(file);
        continue;
      }
      cb.onFileStart(file);
      const fileHave: number[] = [];
      let next = 0;
      let doneCount = 0;

      const worker = async () => {
        while (!cancelled) {
          const c = next++;
          if (c >= file.chunkCount) return;
          let ok = false;
          for (let attempt = 1; attempt <= RETRIES && !cancelled; attempt++) {
            try {
              const { bytes, hash } = await fetchChunk(key, file.index, c);
              const digest = await sha256Hex(bytes);
              if (hash && hash !== digest) {
                throw new Error(
                  `hash mismatch on ${file.path}#${c} (server ${hash} != local ${digest})`,
                );
              }
              if (digest !== file.hashes[c]) {
                throw new Error(`hash mismatch on ${file.path}#${c} (manifest)`);
              }
              await sink(file, c, bytes);
              fileHave.push(c);
              doneCount++;
              cb.onChunk(file, c, doneCount, file.chunkCount);
              ok = true;
              break;
            } catch (err) {
              cb.onChunkError(file, c, err as Error, attempt);
              if (attempt < RETRIES) await sleep(250 * 2 ** (attempt - 1));
            }
          }
          if (!ok && !cancelled) {
            // per-chunk error surface: give up on this file, keep others
            return;
          }
        }
      };

      const workers: Promise<void>[] = [];
      for (let i = 0; i < IN_FLIGHT; i++) workers.push(worker());
      await Promise.all(workers);
      if (cancelled) return;
      have[String(file.index)] = fileHave;
      if (fileHave.length === file.chunkCount) {
        cb.onFileDone(file);
        cb.onProgress({ ...have });
      }
    }
  })();

  return {
    cancel: () => {
      cancelled = true;
    },
    done,
  };
}

// Progress POST timer — receiver posts its bitmap every ~2 s
// (PROTOCOL.md §4).

export function startProgressPoster(
  key: string,
  state: { have: Record<string, number[]> },
  receiver: string,
  everyMs = 2000,
): () => void {
  const t = setInterval(() => {
    void postProgress(key, { ...state.have }, receiver).catch(() => {
      // progress posts are best-effort; failures must not kill a transfer
    });
  }, everyMs);
  return () => clearInterval(t);
}

// ---------------------------------------------------------------------------
// Save layer (M0): desktop Chromium → File System Access API real tree;
// everything else → per-file <a download>.
// ---------------------------------------------------------------------------

export function supportsDirectorySave(): boolean {
  return typeof (window as any).showDirectoryPicker === "function";
}

export async function saveViaDownload(file: ManifestFile, bytes: Uint8Array): Promise<void> {
  const blob = new Blob([bytes as unknown as BlobPart], { type: "application/octet-stream" });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = file.path.split("/").pop() || "chunkdrop-file";
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30_000);
}

// Re-export a tiny hook used by the app shell.
export function usePolling(fn: () => void, ms: number): void {
  useEffect(() => {
    fn();
    const t = setInterval(fn, ms);
    return () => clearInterval(t);
  }, [fn, ms]);
}

// ---------------------------------------------------------------------------
// Upload direction (M1, docs/PROTOCOL.md §4 upload table): phone→PC
// push. The browser builds the manifest (hash-wasm SHA-256 per 4 MiB
// chunk), the engine validates every claim and re-verifies every chunk
// server-side. Possession of the upload key (the #k= fragment) is the
// only authorization the phone needs.
// ---------------------------------------------------------------------------

export interface UploadSpec {
  path: string;
  size: number;
  chunkCount: number;
  hashes: string[];
  mtime: number;
}

export async function postUploadNew(
  groupKey: string,
): Promise<{ id: string; key: string }> {
  const r = await fetch("/api/upload/new", {
    method: "POST",
    headers: { [KEY_HEADER]: groupKey },
  });
  if (!r.ok) throw httpError(r);
  return r.json();
}

export async function postUploadClose(groupKey: string, key: string): Promise<void> {
  const r = await fetch("/api/upload/close", {
    method: "POST",
    headers: { [KEY_HEADER]: groupKey, "Content-Type": "application/json" },
    body: JSON.stringify({ key }),
  });
  if (!r.ok) throw httpError(r);
}

export async function postUploadStart(
  key: string,
  files: UploadSpec[],
): Promise<{ id: string }> {
  const r = await fetch("/api/upload/start", {
    method: "POST",
    headers: { ...headers(key), "Content-Type": "application/json" },
    body: JSON.stringify({ files }),
  });
  if (!r.ok) throw httpError(r);
  return r.json();
}

async function putUploadChunk(
  key: string,
  id: string,
  fileIdx: number,
  chunk: number,
  bytes: Uint8Array,
  hash: string,
): Promise<void> {
  const r = await fetch(`/api/upload/${id}/chunk/${fileIdx}/${chunk}`, {
    method: "PUT",
    headers: { ...headers(key), "Content-Type": "application/octet-stream", "X-Chunk-Hash": hash },
    body: bytes as unknown as BlobPart,
  });
  if (!r.ok) throw httpError(r);
}

export async function postUploadFinish(key: string, id: string): Promise<void> {
  const r = await fetch(`/api/upload/${id}/finish`, {
    method: "POST",
    headers: headers(key),
  });
  if (!r.ok) throw httpError(r);
}

export interface UploadCallbacks {
  onProgress: (done: number, total: number, bytesDone: number, bytesTotal: number) => void;
  onChunkError: (path: string, chunk: number, err: Error, attempt: number) => void;
}

export interface UploadHandle {
  cancel: () => void;
  done: Promise<void>;
}

const UP_IN_FLIGHT = 2;

/** Run the upload pipeline: sequential per file, 2 chunks in flight,
 * 3 retries with backoff — the receive pipeline mirrored. */
export function uploadPipeline(
  key: string,
  id: string,
  specs: UploadSpec[],
  source: (specIdx: number, chunk: number) => Promise<Uint8Array>,
  cb: UploadCallbacks,
): UploadHandle {
  let cancelled = false;
  const bytesTotal = specs.reduce((n, s) => n + s.size, 0);
  let bytesDone = 0;
  let chunksDone = 0;
  const chunksTotal = specs.reduce((n, s) => n + s.chunkCount, 0);

  const done = (async () => {
    for (let i = 0; i < specs.length && !cancelled; i++) {
      const spec = specs[i];
      if (spec.chunkCount === 0) continue; // empty file: finish creates it
      let next = 0;
      const worker = async () => {
        while (!cancelled) {
          const c = next++;
          if (c >= spec.chunkCount) return;
          let ok = false;
          for (let attempt = 1; attempt <= RETRIES && !cancelled; attempt++) {
            try {
              const bytes = await source(i, c);
              const digest = await sha256Hex(bytes);
              await putUploadChunk(key, id, i, c, bytes, digest);
              chunksDone++;
              bytesDone += bytes.length;
              cb.onProgress(chunksDone, chunksTotal, bytesDone, bytesTotal);
              ok = true;
              break;
            } catch (err) {
              cb.onChunkError(spec.path, c, err as Error, attempt);
              if (attempt < RETRIES) await sleep(250 * 2 ** (attempt - 1));
            }
          }
          if (!ok && !cancelled) return; // give up on this file, keep others
        }
      };
      const workers: Promise<void>[] = [];
      for (let w = 0; w < UP_IN_FLIGHT; w++) workers.push(worker());
      await Promise.all(workers);
    }
    if (cancelled) return;
    await postUploadFinish(key, id);
  })();

  return {
    cancel: () => {
      cancelled = true;
    },
    done,
  };
}

/** Chunk size used both for hashing and PUTs (PROTOCOL.md §3). */
export const UPLOAD_CHUNK_SIZE = 4 * 1024 * 1024;