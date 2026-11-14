import { useEffect, useMemo, useRef, useState } from "preact/hooks";

// Receive page (M0-TASKS.md step 7): manifest view (tree, sizes,
// per-file progress), sequential per-file chunk pipeline with 2–3 in
// flight, hash-wasm verify, 3 retries with backoff, per-chunk error
// surface, progress POST every ~2 s, File System Access save on desktop
// Chromium / per-file <a download> elsewhere.

import {
  download,
  fetchManifest,
  postCancel,
  postProgress,
  type DownloadHandle,
  type Manifest,
  type ManifestFile,
} from "./api";
import type { Saver } from "./saver";
import { downloadSaver, newDirectorySaver } from "./saver";
import { humanBytes, fmtPct } from "./util";

interface Props {
  sessionKey: string;
  // Absent on the standalone receive link (/r/): there is no reachable
  // dashboard on the receiving device, so no "back" affordance.
  onBack?: () => void;
}

type Phase = "idle" | "saving" | "done" | "error" | "cancelled";

export function ReceivePage({ sessionKey, onBack }: Props) {
  const [manifest, setManifest] = useState<Manifest | null>(null);
  const [phase, setPhase] = useState<Phase>("idle");
  const [err, setErr] = useState("");
  const [fileProgress, setFileProgress] = useState<Record<number, number>>({});
  const [fileDone, setFileDone] = useState<Record<number, boolean>>({});
  const [log, setLog] = useState<string[]>([]);
  const handleRef = useRef<DownloadHandle | null>(null);
  const saverRef = useRef<Saver | null>(null);

  useEffect(() => {
    let alive = true;
    (async () => {
      if (!sessionKey) {
        setErr("No session key in the link (#k=… missing).");
        return;
      }
      try {
        const m = await fetchManifest(sessionKey);
        if (alive) setManifest(m);
      } catch (e) {
        if (alive) {
          const he = e as { status?: number; message?: string };
          setErr(
            he.status === 401
              ? "Session key rejected (401). Ask the sender for a fresh QR."
              : he.status === 410
                ? "Session expired (410). Ask the sender to restart it."
                : `Could not load the manifest: ${e}`,
          );
        }
      }
    })();
    return () => {
      alive = false;
    };
  }, [sessionKey]);

  const appendLog = (line: string) =>
    setLog((l) => [...l.slice(-200), line]);

  const start = async () => {
    if (!manifest) return;
    setPhase("saving");
    setErr("");
    // Save layer: real tree on desktop Chromium, per-file download otherwise.
    let saver: Saver | null = null;
    try {
      if (!manifest.packedAsZip) {
        saver = await newDirectorySaver(rootName(manifest));
      }
    } catch {
      saver = null;
    }
    if (!saver) saver = downloadSaver();
    saverRef.current = saver;

    const progressState: { have: Record<string, number[]> } = { have: {} };

    const handle = download(
      manifest,
      sessionKey,
      // sink: ordered writes through the saver
      async (file, chunk, bytes) => {
        if (chunk === 0) await saver.openFile(file);
        await saver.writeChunk(file, chunk, bytes);
      },
      {
        onFileStart: (f) => {
          appendLog(`▶ ${f.path}`);
          setFileProgress((p) => ({ ...p, [f.index]: 0 }));
        },
        onFileDone: (f) => {
          appendLog(`✓ ${f.path}`);
          setFileDone((d) => ({ ...d, [f.index]: true }));
          void saver.closeFile(f);
        },
        onChunk: (f, _c, done) => {
          setFileProgress((p) => ({ ...p, [f.index]: done }));
        },
        onChunkError: (f, c, e, attempt) => {
          appendLog(`⚠ ${f.path}#${c} attempt ${attempt}: ${e.message}`);
        },
        onProgress: (have) => {
          progressState.have = have;
        },
      },
    );
    handleRef.current = handle;

    // progress POST every ~2 s (best-effort)
    const poster = setInterval(() => {
      const have = { ...progressState.have };
      void postProgress(sessionKey, have, receiverLabel()).catch(() => {
        /* best-effort */
      });
    }, 2000);

    try {
      await handle.done;
      setPhase("done");
      appendLog("all files done");
    } catch (e) {
      setPhase("error");
      setErr(String(e));
    } finally {
      clearInterval(poster);
    }
  };

  const cancel = async () => {
    handleRef.current?.cancel();
    setPhase("cancelled");
    try {
      await postCancel(sessionKey);
    } catch {
      /* engine may be gone; fine */
    }
  };

  const total = manifest?.files.reduce((a, f) => a + f.chunkCount, 0) ?? 0;
  const done = useMemo(
    () => Object.values(fileProgress).reduce((a, n) => a + n, 0),
    [fileProgress],
  );

  if (err && !manifest) {
    return (
      <div class="card">
        <h2>Cannot receive</h2>
        <p class="error">{err}</p>
        {onBack && (
          <button class="secondary" onClick={onBack}>
            ← back
          </button>
        )}
      </div>
    );
  }

  if (!manifest) return <div class="card muted">Loading manifest…</div>;

  return (
    <div>
      <div class="card">
        <h2 style="margin-top:0">Receive</h2>
        <p>
          {manifest.files.length} file{manifest.files.length === 1 ? "" : "s"}
          {manifest.packedAsZip ? " (packed as ZIP)" : ""} ·{" "}
          {humanBytes(manifest.files.reduce((a: number, f) => a + f.size, 0))}
        </p>
        <div class="bar">
          <div style={{ width: `${total ? (done / total) * 100 : 0}%` }} />
        </div>
        <p class="muted">
          {fmtPct(done, total)} · chunk size {humanBytes(manifest.chunkSize)}
        </p>
        {phase === "idle" && <button onClick={start}>Save all…</button>}
        {phase === "saving" && (
          <button class="secondary" onClick={cancel}>
            Cancel transfer
          </button>
        )}
        {phase === "done" && <p class="ok">Done — files saved.</p>}
        {phase === "cancelled" && <p class="muted">Cancelled. Nothing corrupted.</p>}
        {phase === "error" && <p class="error">{err}</p>}
      </div>

      <div class="card">
        <h3>Files</h3>
        <table>
          <thead>
            <tr>
              <th>path</th>
              <th class="num">size</th>
              <th class="num">chunks</th>
              <th class="num">progress</th>
            </tr>
          </thead>
          <tbody>
            {manifest.files.map((f) => (
              <FileRow
                f={f}
                done={fileDone[f.index]}
                progress={fileProgress[f.index] ?? 0}
              />
            ))}
          </tbody>
        </table>
      </div>

      {log.length > 0 && (
        <div class="card">
          <h3>Log</h3>
          <pre>{log.join("\n")}</pre>
        </div>
      )}
    </div>
  );
}

function FileRow({
  f,
  done,
  progress,
}: {
  f: ManifestFile;
  done?: boolean;
  progress: number;
}) {
  return (
    <tr style={{ opacity: done ? 0.7 : 1 }}>
      <td>
        {done ? "✓ " : ""}
        {f.path}
      </td>
      <td class="num">{humanBytes(f.size)}</td>
      <td class="num">{f.chunkCount}</td>
      <td class="num">{fmtPct(progress, f.chunkCount)}</td>
    </tr>
  );
}

function rootName(m: Manifest): string {
  return m.packedAsZip ? "" : "chunkdrop";
}

function receiverLabel(): string {
  const ua = navigator.userAgent;
  const isPhone = /iPhone|Android|Mobile/i.test(ua);
  const browser = /iPhone|iPad/.test(ua)
    ? "iOS Safari"
    : /Android/.test(ua)
      ? "Android Chrome"
      : /Firefox/.test(ua)
        ? "Firefox"
        : /Chrome/.test(ua)
          ? "Chrome"
          : "Browser";
  return `${browser}${isPhone ? " (phone)" : ""}`;
}