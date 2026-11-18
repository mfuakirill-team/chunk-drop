import { useEffect, useState } from "preact/hooks";

// Side panel: "Mesh mode" (the global mode switch — how this device
// talks to ALL clients: auto / local / internet), "Clients" (who is
// connected, with live download/upload rates) and "Activity" (what is
// moving right now).

import { KEY_HEADER } from "./api";
import { loadGroupKey } from "./storage";
import type { SendState } from "./send";
import { humanBytes } from "./util";

interface PeerInfo {
  id: string;
  name: string;
  ip: string;
  port: number;
  self?: boolean;
  hub?: boolean; // reachable over the internet via the hub
  down?: number; // bytes/s we pull FROM this client
  up?: number; // bytes/s we serve TO this client
}

interface PeersResp {
  self: PeerInfo;
  peers: PeerInfo[];
}

interface ModeResp {
  mode: string; // auto | local | internet
  modes: string[];
}

interface ActivityEvent {
  time: string;
  kind: string; // up | down | info | error
  msg: string;
}

interface UploadInfo {
  session: string;
  files: number;
  bytes: number;
  receiver?: string;
  have?: Record<string, number[]>;
  since: string;
}

interface ActivityResp {
  events: ActivityEvent[];
  uploads: UploadInfo[];
  // Phone→PC pushes in progress (M1, /api/activity "incoming").
  incoming?: IncomingUpload[];
}

interface IncomingUpload {
  id: string;
  files: number;
  bytesDone: number;
  bytesTotal: number;
  since: string;
  finished?: boolean; // batch done, key still live for "send more"
  names?: string[]; // first few paths (engine caps at 8)
}

const KIND_MARK: Record<string, string> = {
  up: "▲",
  down: "▼",
  info: "•",
  error: "!",
};

async function fetchJson<T>(path: string): Promise<T | null> {
  const key = loadGroupKey();
  const headers: Record<string, string> = {};
  if (key) headers[KEY_HEADER] = key;
  try {
    const r = await fetch(path, { headers });
    if (!r.ok) return null;
    return (await r.json()) as T;
  } catch {
    return null; // engine gone; keep last state
  }
}

async function postJson(path: string, body: unknown): Promise<boolean> {
  const key = loadGroupKey();
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (key) headers[KEY_HEADER] = key;
  try {
    const r = await fetch(path, { method: "POST", headers, body: JSON.stringify(body) });
    return r.ok;
  } catch {
    return false;
  }
}

/** Poll /api/activity once (group-key gated, dashboard-only route). */
export function fetchActivity(): Promise<ActivityResp | null> {
  return fetchJson<ActivityResp>("/api/activity");
}

export type { ActivityResp, IncomingUpload };

function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

// "1.2 MB/s"; 0/idle → "–" (no transfer, not zero speed).
function humanSpeed(bps: number | undefined): string {
  if (!bps || bps < 1) return "–";
  return humanBytes(bps) + "/s";
}

// Mesh mode card: three big segmented buttons. One switch for the
// whole device — it decides how THIS engine interacts with ALL
// clients; there is nothing per-peer to configure.
export function ModeCard() {
  const [mode, setMode] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let alive = true;
    const load = async () => {
      const resp = await fetchJson<ModeResp>("/api/mode");
      if (resp && alive && !busy) setMode(resp.mode);
    };
    void load();
    const t = setInterval(load, 3000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  const pick = async (m: string) => {
    if (m === mode || busy) return;
    setMode(m); // optimistic; the poll confirms or corrects
    setBusy(true);
    await postJson("/api/mode", { mode: m });
    setBusy(false);
  };

  const labels: Record<string, string> = { auto: "Auto", local: "Local", internet: "Internet" };
  return (
    <div class="card">
      <h3 style="margin-top:0">Mesh mode</h3>
      <div class="seg">
        {["auto", "local", "internet"].map((m) => (
          <button class={mode === m ? "on" : ""} disabled={busy} onClick={() => void pick(m)}>
            {labels[m]}
          </button>
        ))}
      </div>
      <p class="muted" style="margin:8px 0 0">
        {mode === "local"
          ? "local network only"
          : mode === "internet"
            ? "via the hub only"
            : "everything"}
      </p>
    </div>
  );
}

export function PeersPanel() {
  const [peers, setPeers] = useState<PeersResp | null>(null);

  useEffect(() => {
    let alive = true;
    const load = async () => {
      const resp = await fetchJson<PeersResp>("/api/peers");
      if (resp && alive) setPeers(resp);
    };
    void load();
    const t = setInterval(load, 3000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  const rows = peers
    ? [{ ...peers.self, self: true }, ...peers.peers].sort((a, b) =>
        a.self ? -1 : b.self ? 1 : (a.name || "").localeCompare(b.name || ""),
      )
    : [];

  // ip cell: a hub-only client has no LAN address — show the first
  // characters of its device ID (full ID in the tooltip).
  const addr = (p: PeerInfo): string => (p.ip ? p.ip : `${p.id.slice(0, 8)}…`);

  return (
    <div class="card">
      <h3 style="margin-top:0">Clients</h3>
      <table>
        <thead>
          <tr>
            <th>name</th>
            <th>ip</th>
            <th class="num">▼ down</th>
            <th class="num">▲ up</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((p) => (
            <tr>
              <td>
                {p.self ? "★ " : ""}
                {p.name || p.id.slice(0, 8)}
                {p.hub && !p.ip ? " 🌐" : ""}
              </td>
              <td title={p.id}>{addr(p)}</td>
              <td class="num" title={`downloading from ${p.name || p.id.slice(0, 8)}`}>
                {humanSpeed(p.down)}
              </td>
              <td class="num" title={`serving to ${p.name || p.id.slice(0, 8)}`}>
                {humanSpeed(p.up)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {peers && peers.peers.length === 0 && <p class="muted">no other clients yet</p>}
      {!peers && <p class="muted">discovering…</p>}
      <p class="muted" style="margin-bottom:0">
        sync folder ↔ clients with the key, every 5 s
      </p>
    </div>
  );
}

export function ActivityPanel({ send }: { send: SendState | null }) {
  const [act, setAct] = useState<ActivityResp | null>(null);
  const [open, setOpen] = useState(true);

  useEffect(() => {
    let alive = true;
    const load = async () => {
      const resp = await fetchJson<ActivityResp>("/api/activity");
      if (resp && alive) setAct(resp);
    };
    void load();
    const t = setInterval(load, 3000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  const events = act?.events ?? [];
  const latest = events.slice(-12).reverse(); // newest on top, concise
  const liveReceiver = send?.sessionInfo?.receiver;

  // Names of the files a receiver is pulling right now, mapped from
  // the session's have-bitmap indices into the started manifest.
  const receivingNames: string[] = [];
  if (liveReceiver && send?.started) {
    const files = send.started.manifest.files;
    for (const idx of Object.keys(liveReceiver.have)) {
      const f = files[Number(idx)];
      if (f) receivingNames.push(f.path);
    }
  }

  // "a.jpg, b.jpg +5 more" — cap at 3 names, count the rest of the total.
  const namesLabel = (names: string[], total: number): string => {
    const shown = names.slice(0, 3).join(", ");
    return total > 3 ? `${shown} +${total - 3} more` : shown;
  };

  return (
    <div class="card">
      <h3 style="margin-top:0">
        Activity{" "}
        <button class="secondary" style="padding:2px 10px" onClick={() => setOpen(!open)}>
          {open ? "hide" : "show"}
        </button>
      </h3>
      {open && (
        <div>
          {/* live receiver progress of the running transfer */}
          {liveReceiver ? (
            <p style="margin:4px 0; overflow-wrap:anywhere">
              ▲ {liveReceiver.receiver}: {Object.keys(liveReceiver.have).length} file(s)
              {receivingNames.length > 0 ? ` — ${namesLabel(receivingNames, receivingNames.length)}` : ""}
            </p>
          ) : null}
          {/* phone→PC pushes (M1) */}
          {(act?.incoming ?? []).map((u) => (
            <p style="margin:4px 0; overflow-wrap:anywhere">
              {u.finished ? "✓" : "▼"} {u.files} file(s)
              {u.names?.length ? ` — ${namesLabel(u.names, u.files)}` : ""} — {humanBytes(u.bytesDone)}
              {u.bytesTotal > 0 ? ` / ${humanBytes(u.bytesTotal)}` : ""}
            </p>
          ))}
          <ul class="act">
            {latest.map((e) => (
              <li>
                <span class={"actk actk-" + e.kind}>{KIND_MARK[e.kind] ?? "•"}</span>{" "}
                <span class="muted">{fmtTime(e.time)}</span> {e.msg}
              </li>
            ))}
            {latest.length === 0 && <li class="muted">nothing yet</li>}
          </ul>
        </div>
      )}
    </div>
  );
}
