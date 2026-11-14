import { useEffect, useRef, useState } from "preact/hooks";

// Sender dashboard (M0-TASKS.md step 8): file/folder picker, QR with
// the receive URL, copy-link fallback, live receiver progress polled
// from /api/session.

import { KEY_HEADER, type SessionInfo } from "./api";
import { buildReceiveUrl } from "./link";
import { renderQr } from "./qr";

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
  const [groupKey, setGroupKey] = useState(
    localStorage.getItem("chunkdrop.group_key") ?? "",
  );
  const [started, setStarted] = useState<StartResponse | null>(null);
  const [sessionInfo, setSessionInfo] = useState<SessionInfo | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);

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

  const pickFiles = async (dir: boolean) => {
    try {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      const w = window as any;
      if (dir && w.showDirectoryPicker) {
        const handle = await w.showDirectoryPicker();
        const names: string[] = [];
        await collectFiles(handle, handle.name, names, 200);
        setPaths(names);
        return;
      }
      const input = document.createElement("input");
      input.type = "file";
      input.multiple = true;
      input.onchange = () => {
        setPaths(Array.from(input.files ?? []).map((f) => (f as File & { path?: string }).path ?? f.name));
      };
      input.click();
    } catch {
      /* picker dismissed */
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
      localStorage.setItem("chunkdrop.group_key", groupKey);
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
          Pick files or a folder. The receiver scans the QR (or opens the
          link) and saves straight from their browser.
        </p>
        <p>
          <button onClick={() => void pickFiles(false)}>Pick files…</button>{" "}
          <button class="secondary" onClick={() => void pickFiles(true)}>
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
        {paths.length > 0 && (
          <p class="muted">
            {paths.length} path{paths.length === 1 ? "" : "s"} selected
            {zip ? " → will be packed into bundle.zip (store mode)" : ""}
          </p>
        )}
        <p>
          <input
            type="password"
            placeholder="group key (engine keygen)"
            value={groupKey}
            onInput={(e) => setGroupKey((e.target as HTMLInputElement).value)}
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

async function collectFiles(
  dir: { values: () => AsyncIterable<{ kind: string; name: string; getFileHandle?: unknown }> },
  prefix: string,
  out: string[],
  cap: number,
): Promise<void> {
  // Collect up to `cap` paths from a directory handle (browser-side
  // picking; the engine re-walks by absolute path server-side).
  for await (const entry of dir.values()) {
    if (out.length >= cap) return;
    if (entry.kind === "file") out.push(`${prefix}/${entry.name}`);
  }
}