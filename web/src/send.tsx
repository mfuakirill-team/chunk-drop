import { useEffect, useRef, useState } from "preact/hooks";

// Sender dashboard (M0-TASKS.md step 8): file/folder picker, QR with
// the receive URL, copy-link fallback, live receiver progress polled
// from /api/session. Pick runs a native dialog on the engine (#1);
// the group key persists in localStorage (#2). M0.2: selected paths
// sit in their own frame with a Reset button; Cancel sits next to
// Start; activity/progress live in the side panel (PeersPanel).

import {
  KEY_HEADER,
  UI_HEADER,
  postUploadClose,
  postUploadNew,
  type SessionInfo,
} from "./api";
import { fetchActivity } from "./peers";
import { buildReceiveUrl, buildUploadUrl } from "./link";
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
  // M2: the key is not just a browser note anymore — the engine
  // applies it the moment it is submitted (mesh rekeyed live) and
  // writes engine.toml ~20 s later.
  const [keyStatus, setKeyStatus] = useState("");
  const keyTimer = useRef<number | null>(null);
  const [started, setStarted] = useState<StartResponse | null>(null);
  const [sessionInfo, setSessionInfo] = useState<SessionInfo | null>(null);
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const [pickBusy, setPickBusy] = useState(false);
  const [manualPaths, setManualPaths] = useState("");
  const [copied, setCopied] = useState(false);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);

  const setKeyAndSave = (k: string) => {
    setGroupKey(k);
    setKeyStatus("");
    // The engine saves to engine.toml ~20 s after the last change;
    // here a shorter debounce just avoids a POST per keystroke.
    if (keyTimer.current !== null) clearTimeout(keyTimer.current);
    keyTimer.current = window.setTimeout(async () => {
      const prev = loadGroupKey();
      if (!k || k === prev) return;
      try {
        // The engine trusts the dashboard's own stamp (custom header +
        // Origin), not an old key: the owner replaces the key freely,
        // and a browser that lost its saved copy just types it again.
        const r = await fetch("/api/key", {
          method: "POST",
          headers: { [UI_HEADER]: "dashboard" },
          body: JSON.stringify({ key: k }),
        });
        if (r.ok) {
          saveGroupKey(k);
          setKeyStatus("key applied — mesh rekeyed; engine.toml updated in ~20 s");
        } else if (r.status === 401) {
          setKeyStatus("engine refused the change — open this dashboard on the engine itself");
        } else if (r.status === 400) {
          setKeyStatus(
            "invalid key — printable characters only (no quotes, backslash or control characters)",
          );
        } else {
          setKeyStatus(`engine did not accept the key (HTTP ${r.status})`);
        }
      } catch {
        setKeyStatus("engine unreachable — key kept in this browser only");
      }
    }, 800);
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
    <div class="maincol">
      <div class="card">
        <h2>Send</h2>
        <p class="muted">
          Pick files on this machine, or paste paths. The receiver scans the
          QR and saves straight from the browser.
        </p>
        <div class="row">
          <button disabled={pickBusy} onClick={() => void pickNative(false)}>
            {pickBusy ? "Opening dialog…" : "Pick files…"}
          </button>
          <button class="secondary" disabled={pickBusy} onClick={() => void pickNative(true)}>
            Pick folder…
          </button>
          <label class="check">
            <input
              type="checkbox"
              checked={zip}
              onChange={(e) => setZip((e.target as HTMLInputElement).checked)}
            />
            pack files as ZIP
          </label>
        </div>
        <div class="row">
          <input
            type="text"
            placeholder="…or paste absolute paths, one per line (/home/me/big.iso, D:\movie.mkv)"
            value={manualPaths}
            onInput={(e) => setManualPaths((e.target as HTMLInputElement).value)}
          />
          <button class="secondary" onClick={applyManual} disabled={!manualPaths.trim()}>
            Use pasted paths
          </button>
        </div>

        {/* selected paths get their own frame + Reset */}
        <div class={"selbox" + (paths.length > 0 ? " selbox-on" : "")}>
          {paths.length === 0 ? (
            <p class="muted">Nothing selected.</p>
          ) : (
            <div>
              <p class="selhead">
                <strong>
                  {paths.length} path{paths.length === 1 ? "" : "s"} selected
                </strong>
                {zip && <span class="muted">→ bundle.zip</span>}
                <span class="grow" />
                <button class="ghost" onClick={reset}>
                  Reset
                </button>
              </p>
              <ul class="sellist">
                {paths.slice(0, 12).map((p) => (
                  <li>{p}</li>
                ))}
                {paths.length > 12 ? <li class="muted">… +{paths.length - 12} more</li> : ""}
              </ul>
            </div>
          )}
        </div>

        <label class="field">
          <span class="lbl">Group key</span>
          <input
            type="text"
            placeholder="applied instantly · saved to engine.toml"
            value={groupKey}
            onInput={(e) => setKeyAndSave((e.target as HTMLInputElement).value)}
          />
        </label>
        {keyStatus && <p class="muted">{keyStatus}</p>}
        <div class="row">
          <button disabled={busy || !!started} onClick={start}>
            {busy ? "Hashing…" : "Start transfer"}
          </button>
          {started && (
            <button class="secondary" onClick={cancel}>
              Cancel
            </button>
          )}
        </div>
        {err && <p class="error">{err}</p>}
      </div>

      {started && (
        <div class="card">
          <h3>Scan to receive</h3>
          <div class="qrbox">
            <canvas class="qr" ref={canvasRef} width={320} height={320} />
          </div>
          <div class="row">
            <button
              class="secondary"
              onClick={() => {
                void navigator.clipboard?.writeText(receiveUrl);
                setCopied(true);
                setTimeout(() => setCopied(false), 1500);
              }}
            >
              {copied ? "Copied!" : "Copy link"}
            </button>
            <button class="secondary" onClick={() => onStart(started.key)}>
              Open on this device
            </button>
          </div>
          <p class="url">{receiveUrl}</p>
        </div>
      )}

      <PhoneUploadCard />
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

// M1 "Receive from a phone": mints an upload slot (RAM-only, 24 h)
// and shows its /u/ QR for the phone to scan. The slot carries no
// files until the phone pushes them; Close-link kills it.
export function PhoneUploadCard() {
  const [slot, setSlot] = useState<{ id: string; key: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState("");
  const [copied, setCopied] = useState(false);
  const [renewed, setRenewed] = useState(false); // auto re-minted after a spent link
  const [closedNote, setClosedNote] = useState(false); // last link closed by hand
  const manuallyClosedRef = useRef(false);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);

  const url = slot ? buildUploadUrl(location.origin, slot.key) : "";

  useEffect(() => {
    if (!slot) return;
    const canvas = canvasRef.current;
    if (canvas) void renderQr(canvas, url).catch(() => {});
  }, [slot]);

  // Watch the open link: when a *successful* activity poll no longer
  // lists the slot (phone finished the batch, 24 h passed, engine
  // restarted), re-mint so the QR is always scannable — the phone must
  // never end up erroring on a dead link (DEVICE-TESTS §C). A manual
  // "Close link" is terminal: no re-mint after it (the key must stay
  // dead until the user explicitly opens a new link).
  useEffect(() => {
    if (!slot) return;
    let alive = true;
    const t = setInterval(async () => {
      const act = await fetchActivity();
      // act === null (engine unreachable, transient) means "unknown" —
      // never interpret that as "slot gone", and never re-mint after a
      // manual close.
      if (!alive || manuallyClosedRef.current || !act) return;
      if (!act.incoming?.some((u) => u.id === slot.id)) {
        try {
          setSlot(await postUploadNew(loadGroupKey()));
          setRenewed(true);
        } catch {
          // engine unreachable — retry on the next tick
        }
      }
    }, 3000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, [slot]);

  const open = async () => {
    setBusy(true);
    setErr("");
    setRenewed(false);
    setClosedNote(false);
    manuallyClosedRef.current = false;
    try {
      setSlot(await postUploadNew(loadGroupKey()));
    } catch (e) {
      const st = (e as Error & { status?: number }).status;
      setErr(
        st === 401
          ? "Group key rejected — set it above first."
          : `Could not open an upload link (HTTP ${st ?? "?"})`,
      );
    } finally {
      setBusy(false);
    }
  };

  const close = async () => {
    if (!slot) return;
    setBusy(true);
    // Mark the close BEFORE the request: no watcher tick may re-mint
    // around it — a closed link must stay closed.
    manuallyClosedRef.current = true;
    try {
      await postUploadClose(loadGroupKey(), slot.key);
      setSlot(null);
      setRenewed(false);
      setClosedNote(true); // "the phone's key no longer works"
    } catch (e) {
      manuallyClosedRef.current = false;
      setErr(`Closing the link failed (${e}) — it expires on its own within 24 h.`);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="card">
      <h3>Receive from a phone</h3>
      {!slot ? (
        <div>
          {closedNote && (
            <p class="muted">
              Link closed — the phone's key no longer works. Open a new link
              for the next transfer.
            </p>
          )}
          <p class="muted">
            Let a phone push photos/files into this PC's sync folder.
          </p>
          <div class="row">
            <button disabled={busy || !loadGroupKey()} onClick={() => void open()}>
              {busy ? "Opening…" : "Open upload link"}
            </button>
          </div>
          {!loadGroupKey() && <p class="muted">Set the group key above first.</p>}
          {err && <p class="error">{err}</p>}
        </div>
      ) : (
        <div>
          {renewed && (
            <p class="muted">The previous link ended — a fresh QR is shown below.</p>
          )}
          <div class="qrbox">
            <canvas class="qr" ref={canvasRef} width={320} height={320} />
          </div>
          <div class="row">
            <button
              class="secondary"
              onClick={() => {
                void navigator.clipboard?.writeText(url);
                setCopied(true);
                setTimeout(() => setCopied(false), 1500);
              }}
            >
              {copied ? "Copied!" : "Copy link"}
            </button>
            <button class="secondary" disabled={busy} onClick={() => void close()}>
              Close link
            </button>
          </div>
          <p class="url">{url}</p>
          <p class="muted">
            Anyone with this link can upload until you close it (or 24 h).
            The phone's "Send more" reuses the same QR.
          </p>
        </div>
      )}
    </div>
  );
}