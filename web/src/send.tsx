import { useEffect, useRef, useState } from "preact/hooks";

// Sender dashboard (M0-TASKS.md step 8): file/folder picker, QR with
// the receive URL, copy-link fallback, live receiver progress polled
// from /api/session. Pick runs a native dialog on the engine (#1);
// the group key persists in localStorage (#2).

import { KEY_HEADER, type SessionInfo } from "./api";
import { buildReceiveUrl } from "./link";
import { renderQr } from "./qr";
import { loadGroupKey, saveGroupKey } from "./storage";

interface StartResponse {
  sessionId: string;
  key: string;
  manifest: {
    packedAsZip: boolean;
    files: { path: string; size: number }[];
  };
}

interface Props {
  onStart: (sessionKey: string) => void;
}

export function SendPage({ onStart }: Props) {
  const [paths, setPaths] = useState<string[]>([]);
  const [zip, setZip] = useState(false);
  // #2: persisted group key; #3: plain input, not a password field.
  const [groupKey, setGroupKey] = useState(loadGroupKey);
  const [started, setStarted] = useState<StartResponse | null>(null);
  const [sessionInfo, setSessionInfo] = useState<SessionInfo | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [pickBusy, setPickBusy] = useState(false);
  const [manualPaths, setManualPaths] = useState("");
  const canvasRef = useRef<HTMLCanvasElement | null>(null);

  const setKeyAndSave = (k: string) => {
    setGroupKey(k);
    saveGroupKey(k);
  };

  useEffect(() => {
    if (!started) return;
    const url = buildReceiveUrl(location.origin, started.key);
    const canvas = canvasRef.current;
    if (canvas) void renderQr(canvas, url).catch(() => {});
  }, [started]);

  // Poll receiver progress (live % for the sender).
  useEffect(() => {
    if (!started) return;
    const t = setInterval(async () => {
      try {
        const info = await fetchSessionWithId(started.sessionId, started.key);
        setSessionInfo(info);
      } catch {
        /* session may be gone; keep last state */
      }
    }, 2000);
    return () => clearInterval(t);
  }, [started]);

  // #1: native dialog on the engine machine — browsers cannot hand
  // the server real paths (that was the "stat README.md" bug: the
  // dashboard sent bare filenames).
  const pickNative = async (folder: boolean) => {
    setPickBusy(true);
    setErr("");
    try {
      const r = await fetch(folder ? "/api/pick/folder" : "/api/pick/files", {
        method: "POST",
        headers: { [KEY_HEADER]: groupKey },
      });
      if (!r.ok) throw new Error(`picker → HTTP ${r.status}`);
      const resp = (await r.json()) as { paths: string[] };
      if (resp.paths && resp.paths.length > 0) setPaths(resp.paths);
    } catch (e) {
      setErr(
        `Native picker failed (${e}) — paste absolute paths below instead.`,
      );
    } finally {
      setPickBusy(false);
    }
  };

  const applyManual = () => {
    const list = manualPaths
      .split(/[\n,]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    if (list.length > 0) {
      setPaths(list);
      setManualPaths("");
    }
  };

  const start = async () => {
    if (paths.length === 0 || !groupKey) {
      setErr("Pick files and set the group key first.");
      return;
    }
    setBusy(true);
    setErr("");
    try {
      const r = await fetch("/api/start", {
        method: "POST",
        headers: { [KEY_HEADER]: groupKey, "Content-Type": "application/json" },
        body: JSON.stringify({ paths, zip }),
      });
      if (!r.ok) {
        throw new Error(
          r.status === 401 ? "Group key rejected (401)" : `start failed: HTTP ${r.status}`,
        );
      }
      const resp = (await r.json()) as StartResponse;
      setStarted(resp);
    } catch (e) {
      setErr(String(e));
    } finally {
      setBusy(false);
    }
  };

  const receiveUrl = started ? buildReceiveUrl(location.origin, started.key) : "";

  return (
    <div>
      <div class="card">
        <h2 style="margin-top:0">Send</h2>
        <p class="muted">
          Pick files or a folder (a dialog opens on the engine machine),
          or paste absolute paths. The receiver scans the QR (or opens
          the link) and saves straight from the browser.
        </p>
        <p>
          <button disabled={pickBusy} onClick={() => void pickNative(false)}>
            {pickBusy ? "Opening dialog…" : "Pick files…"}
          </button>{" "}
          <button class="secondary" disabled={pickBusy} onClick={() => void pickNative(true)}>
            Pick folder…
          </button>{" "}
          <label style="margin-left:12px">
            <input
              type="checkbox"
              checked={zip}
              onChange={(e) => setZip((e.target as HTMLInputElement).checked)}
            />{" "}
            pack many small files as store-mode ZIP
          </label>
        </p>
        <p>
          <input
            type="text"
            placeholder={"or paste paths, one per line\n/home/me/big.iso, D:\\movie.mkv"}
            value={manualPaths}
            onInput={(e) => setManualPaths((e.target as HTMLInputElement).value)}
          />
        </p>
        <p>
          <button class="secondary" onClick={applyManual} disabled={!manualPaths.trim()}>
            Use pasted paths
          </button>
        </p>
        {paths.length > 0 && (
          <p class="muted">
            {paths.length} path{paths.length === 1 ? "" : "s"} selected
            {zip ? " → will be packed into bundle.zip (store mode)" : ""}
            <br />
            {paths.slice(0, 5).map((p) => (
              <div style={{ overflowWrap: "anywhere" }}>{p}</div>
            ))}
            {paths.length > 5 ? `… +${paths.length - 5} more` : ""}
          </p>
        )}
        <p>
          <input
            type="text"
            placeholder="group key (engine keygen) — saved in this browser"
            value={groupKey}
            onInput={(e) => setKeyAndSave((e.target as HTMLInputElement).value)}
          />
        </p>
        <p>
          <button disabled={busy} onClick={start}>
            {busy ? "Hashing…" : "Start transfer"}
          </button>
          {err && <span class="error"> {err}</span>}
        </p>
      </div>

      {started && (
        <div class="card">
          <h3 style="margin-top:0">Scan to receive</h3>
          <canvas class="qr" ref={canvasRef} width={320} height={320} />
          <p>
            <button
              class="secondary"
              onClick={() => void navigator.clipboard?.writeText(receiveUrl)}
            >
              Copy link
            </button>{" "}
            <button class="secondary" onClick={() => onStart(started.key)}>
              Open on this device
            </button>
          </p>
          <p class="muted" style={{ overflowWrap: "anywhere" }}>
            {receiveUrl}
          </p>
        </div>
      )}

      {started && (
        <div class="card">
          <h3 style="margin-top:0">Receiver progress</h3>
          {sessionInfo?.receiver ? (
            <div>
              <p class="muted">{sessionInfo.receiver.receiver} — live</p>
              <table>
                <thead>
                  <tr>
                    <th>file</th>
                    <th class="num">have</th>
                  </tr>
                </thead>
                <tbody>
                  {Object.entries(sessionInfo.receiver.have).map(([f, chunks]) => (
                    <tr>
                      <td>{started.manifest.files[Number(f)]?.path ?? f}</td>
                      <td class="num">
                        {(chunks as number[]).length}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p class="muted">Waiting for the first receiver…</p>
          )}
        </div>
      )}
    </div>
  );
}

async function fetchSessionWithId(id: string, key: string): Promise<SessionInfo> {
  const r = await fetch("/api/session", {
    headers: { [KEY_HEADER]: key, "X-ChunkDrop-Session": id },
  });
  if (!r.ok) throw new Error(`HTTP ${r.status}`);
  return r.json();
}