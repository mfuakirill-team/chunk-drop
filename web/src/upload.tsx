import { useEffect, useRef, useState } from "preact/hooks";

// Upload page (M1, /u/ mount): phone→PC push. Pick files or a folder,
// build the manifest in the browser (hash-wasm SHA-256 per 4 MiB
// chunk), then stream chunks to the engine — 2 in flight, 3 retries
// with backoff — and finish so the engine lands the files in its
// destination (the sync folder by default). No dashboard-back
// affordance: this page is the phone's whole world (PROTOCOL.md §4.1).

import {
  postUploadStart,
  sha256Hex,
  UPLOAD_CHUNK_SIZE,
  uploadPipeline,
  type UploadHandle,
  type UploadSpec,
} from "./api";
import { humanBytes, fmtPct } from "./util";

interface Props {
  uploadKey: string;
}

type Phase =
  | "idle" // pick files
  | "hashing" // building the manifest
  | "uploading"
  | "done"
  | "error";

// One picked file + its manifest-relative path (folder picks carry
// webkitRelativePath, e.g. "photos/2026/p1.jpg").
interface Picked {
  file: File;
  path: string;
}

function relPath(f: File): string {
  const rel = (f as File & { webkitRelativePath?: string }).webkitRelativePath;
  return rel && rel.includes("/") ? rel : f.name;
}

function chunkCountFor(size: number): number {
  return size === 0 ? 0 : Math.ceil(size / UPLOAD_CHUNK_SIZE);
}

// "a.jpg, b.jpg, c.jpg +4 more" — cap at 3 names, count the rest.
function namesLabel(names: string[]): string {
  const shown = names.slice(0, 3).join(", ");
  return names.length > 3 ? `${shown} +${names.length - 3} more` : shown;
}

// Map engine errors to phone-actionable text: a 401 on an upload means
// the link ended (closed, expired, engine restarted) — no file picking
// retry can fix that, only a fresh QR from the dashboard.
function friendlyError(e: unknown): string {
  const st = (e as Error & { status?: number }).status;
  if (st === 401) {
    return "This upload link has ended (expired or closed). Rescan the QR code on the computer's dashboard to get a fresh one.";
  }
  return String(e);
}

export function UploadPage({ uploadKey }: Props) {
  const [phase, setPhase] = useState<Phase>("idle");
  const [err, setErr] = useState("");
  const [hashDone, setHashDone] = useState(0); // hashed bytes while hashing
  const [hashTotal, setHashTotal] = useState(0);
  const [upDone, setUpDone] = useState(0); // uploaded chunks/bytes
  const [upTotal, setUpTotal] = useState(0);
  const [upBytes, setUpBytes] = useState(0);
  const [fileCount, setFileCount] = useState(0);
  const [fileNames, setFileNames] = useState<string[]>([]); // chosen, for the display
  const [curFile, setCurFile] = useState(""); // file being sent right now
  const [note, setNote] = useState("");
  const handleRef = useRef<UploadHandle | null>(null);

  useEffect(() => () => handleRef.current?.cancel(), []);

  if (!uploadKey) {
    return (
      <div class="card">
        <h2 style="margin-top:0">Send to the computer</h2>
        <p>
          This link is missing its key. Open it by scanning the QR code
          shown on the computer's dashboard ("Receive from a phone").
        </p>
      </div>
    );
  }

  const startWith = async (picked: Picked[]) => {
    setErr("");
    if (picked.length === 0) return;
    setFileCount(picked.length);
    setFileNames(picked.map((p) => relPath(p.file)));
    setCurFile("");
    setPhase("hashing");
    setHashTotal(picked.reduce((n, p) => n + p.file.size, 0));
    setHashDone(0);

    // Build the manifest: hash every 4 MiB chunk. The engine re-verifies
    // both, but hashing here means a corrupted read fails fast, before
    // the engine ever sees the bytes.
    let hashesDone = 0;
    const specs: UploadSpec[] = [];
    try {
      for (const p of picked) {
        const n = chunkCountFor(p.file.size);
        const hashes: string[] = [];
        for (let c = 0; c < n; c++) {
          const slice = p.file.slice(c * UPLOAD_CHUNK_SIZE, (c + 1) * UPLOAD_CHUNK_SIZE);
          const bytes = new Uint8Array(await slice.arrayBuffer());
          hashes.push(await sha256Hex(bytes));
          hashesDone += bytes.length;
          setHashDone(hashesDone);
        }
        specs.push({
          path: relPath(p.file),
          size: p.file.size,
          chunkCount: n,
          hashes,
          mtime: p.file.lastModified,
        });
      }
    } catch (e) {
      setErr(`Reading the files failed: ${e}`);
      setPhase("error");
      return;
    }

    setPhase("uploading");
    setUpTotal(specs.reduce((n, s) => n + s.chunkCount, 0));
    setUpDone(0);
    setUpBytes(0);

    // Start: the engine validates every manifest claim (paths, chunk
    // math, caps) and echoes the slot id the chunk PUTs are addressed
    // with (bound server-side to this key).
    let id: string;
    try {
      ({ id } = await postUploadStart(uploadKey, specs));
    } catch (e) {
      const st = (e as Error & { status?: number }).status;
      // A 401 already reads as a full sentence — don't bury it under a
      // "rejected" prefix.
      setErr(st === 401 ? friendlyError(e) : `The computer rejected the transfer: ${friendlyError(e)}`);
      setPhase("error");
      return;
    }

    const h = uploadPipeline(
      uploadKey,
      id,
      specs,
      (specIdx, chunk) =>
        picked[specIdx].file
          .slice(chunk * UPLOAD_CHUNK_SIZE, (chunk + 1) * UPLOAD_CHUNK_SIZE)
          .arrayBuffer()
          .then((b) => new Uint8Array(b)),
      {
        onProgress: (done, total, bytesDone) => {
          setUpDone(done);
          setUpBytes(bytesDone);
          setUpTotal(total);
        },
        onFileStart: (path) => setCurFile(path),
        onChunkError: (path, _c, e, attempt) => {
          // The pipeline owns the give-up decision; until `done` rejects
          // this stays a retry note.
          setNote(`${path}: retrying (attempt ${attempt} — ${e.message})`);
        },
      },
    );
    handleRef.current = h;
    try {
      await h.done;
      setPhase("done");
      setNote("");
    } catch (e) {
      setErr(friendlyError(e));
      setPhase("error");
    }
  };

  // ... startWith continues below via the pickers

  const onPick = async (e: Event) => {    const input = e.target as HTMLInputElement;
    const files = Array.from(input.files ?? []);
    await startWith(files.map((f) => ({ file: f, path: relPath(f) })));
  };

  return (
    <div class="card">
      <h2 style="margin-top:0">Send to the computer</h2>
      {phase === "idle" && (
        <div>
          <p class="muted">Choose what to push. Everything is verified end-to-end.</p>
          <p>
            <label class="bigbtn">
              Choose files
              <input type="file" multiple style="display:none" onChange={onPick} />
            </label>
          </p>
          <p>
            <label class="bigbtn secondary">
              Choose a folder
              <input
                type="file"
                style="display:none"
                onChange={onPick}
                // Non-standard but supported by Chrome/Edge/Safari 11.1+
                // (Firefox Android ignores it — files fall back to flat).
                {...({ webkitdirectory: "", directory: "" } as any)}
              />
            </label>
          </p>
        </div>
      )}
      {phase === "hashing" && (
        <div>
          <p>
            Reading {fileCount} file(s)… {humanBytes(hashDone)} / {humanBytes(hashTotal)}
          </p>
          <p class="muted" style="margin:4px 0; overflow-wrap:anywhere">
            {namesLabel(fileNames)}
          </p>
          <progress max={hashTotal} value={hashDone} style="width:100%" />
        </div>
      )}
      {phase === "uploading" && (
        <div>
          <p>
            Uploading {fileCount} file(s) — {humanBytes(upBytes)} ·{" "}
            {fmtPct(upDone, upTotal)}
          </p>
          <p style="margin:4px 0; overflow-wrap:anywhere">
            {curFile ? <>Sending: <strong>{curFile}</strong></> : "\u00a0"}
          </p>
          <p class="muted" style="margin:4px 0; overflow-wrap:anywhere">
            {namesLabel(fileNames)}
          </p>
          <progress max={upTotal} value={upDone} style="width:100%" />
          {note && <p class="muted">{note}</p>}
          <p>
            <button
              class="secondary"
              onClick={() => {
                handleRef.current?.cancel();
                setPhase("idle");
              }}
            >
              Cancel
            </button>
          </p>
        </div>
      )}
      {phase === "done" && (
        <div>
          <p>✓ Sent {fileCount} file(s) — check the computer's dashboard.</p>
          <p class="muted" style="margin:4px 0; overflow-wrap:anywhere">
            {namesLabel(fileNames)}
          </p>
          <p>
            <button
              class="secondary"
              onClick={() => {
                setPhase("idle");
                setFileCount(0);
                setFileNames([]);
                setCurFile("");
              }}
            >
              Send more
            </button>
          </p>
        </div>
      )}
      {phase === "error" && (
        <div>
          <p style="color:#b00">{err}</p>
          <button
            onClick={() => {
              setPhase("idle");
              setErr("");
              setNote("");
              setCurFile("");
            }}
          >
            Try again
          </button>
        </div>
      )}
    </div>
  );
}
