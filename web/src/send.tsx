import { useEffect, useRef, useState } from "preact/hooks";

// Sender dashboard (M0-TASKS.md step 8): file/folder picker, QR with
// the receive URL, copy-link fallback, live receiver progress polled
// from /api/session. Pick runs a native dialog on the engine (#1);
// the group key persists in localStorage (#2). M0.2: selected paths
// sit in their own frame with a Reset button; Cancel sits next to
// Start; activity/progress live in the side panel (PeersPanel).

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

export interface SendState {
  started: StartResponse | null;
  sessionInfo: SessionInfo | null;
  cancel: () => void;
}

interface Props {
  onStart: (sessionKey: string) => void;
  onChange: (st: SendState | null) => void;
}

export function SendPage({ onStart, onChange }: Props) {
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

  // Poll receiver progress (live % for the sender) → activity panel.
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

  // Expose started/progress/cancel to the side panel (#1.3).
  const cancel = () => {
    if (!started) return;
    void fetch("/api/cancel", {
      method: "POST",
      headers: { [KEY_HEADER]: started.key },
    }).catch(() => {});
    setStarted(null);
    setSessionInfo(null);
  };
  useEffect(() => {
    onChange(started ? { started, sessionInfo, cancel } : null);
  }, [started, sessionInfo]);

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

  const reset = () => {
    setPaths([]);
    setZip(false);
    setErr("");
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

        {/* #1.1: selected paths get their own frame + Reset. */}
        <div class={"selbox" + (paths.length > 0 ? " selbox-on" : "")}>
          {paths.length === 0 ? (
            <p class="muted" style="margin:0">
              Nothing selected yet.
            </p>
          ) : (
            <div>
              <p style="margin:0 0 6px">
                <strong>
                  {paths.length} path{paths.length === 1 ? "" : "s"} selected
                </strong>
                {zip ? " → bundle.zip (store mode)" : ""}
                {" "}
                <button class="secondary" style="padding:2px 10px" onClick={reset}>
                  Reset selection
                </button>
              </p>
              <ul class="sellist">
                {paths.slice(0, 12).map((p) => (
                  <li style={{ overflowWrap: "anywhere" }}>{p}</li>
                ))}
                {paths.length > 12 ? <li class="muted">… +{paths.length - 12} more</li> : ""}
              </ul>
            </div>
          )}
        </div>

        <p>
          <input
            type="text"
            placeholder="group key (engine keygen) — saved in this browser"
            value={groupKey}
            onInput={(e) => setKeyAndSave((e.target as HTMLInputElement).value)}
          />
        </p>
        <p>
          <button disabled={busy || !!started} onClick={start}>
            {busy ? "Hashing…" : "Start transfer"}
          </button>{" "}
          {/* #1.2: Cancel alongside Start (right side). */}
          {started && (
            <button class="secondary" onClick={cancel}>
              Cancel
            </button>
          )}
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