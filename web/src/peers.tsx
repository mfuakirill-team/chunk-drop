import { useEffect, useState } from "preact/hooks";

// Side panel (#7 + #1.3): two separate frames — "LAN clients" (the
// discovery table with peers' IPs) and, below it, "Activity" (what's
// uploading / downloading right now + live receiver progress of the
// running transfer).

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
}

interface PeersResp {
  self: PeerInfo;
  peers: PeerInfo[];
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

function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
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

  return (
    <div class="card">
      <h3 style="margin-top:0">LAN clients</h3>
      <table>
        <thead>
          <tr>
            <th>name</th>
            <th>ip</th>
            <th class="num">port</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((p) => (
            <tr>
              <td>
                {p.self ? "★ " : ""}
                {p.name || p.id.slice(0, 8)}
              </td>
              <td>{p.ip}</td>
              <td class="num">{p.port}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {peers && peers.peers.length === 0 && (
        <p class="muted">no other engines found — is the other machine running?</p>
      )}
      {!peers && <p class="muted">discovering LAN clients…</p>}
      <p class="muted" style="margin-bottom:0">
        auto-sync: ~/Downloads/chunk-drop-files ↔ other engines (same
        group key), every 5s. Deletions are signalled to every engine
        (replayed for 24h to ones that were offline).
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
            <p class="muted" style="margin:4px 0">
              ▲ receiving now: {liveReceiver.receiver} (
              {Object.keys(liveReceiver.have).length} file(s) in progress)
            </p>
          ) : (
            <p class="muted" style="margin:4px 0">
              {act && act.uploads.length > 0
                ? `${act.uploads.length} live session(s) waiting for a receiver`
                : "idle — no transfers in flight"}
            </p>
          )}
          {/* phone→PC pushes in progress (M1) */}
          {(act?.incoming ?? []).map((u) => (
            <p class="muted" style="margin:4px 0">
              ▼ phone pushing {u.files} file(s) — {humanBytes(u.bytesDone)}
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
            {latest.length === 0 && (
              <li class="muted">nothing yet — start a transfer or drop a file in the sync folder</li>
            )}
          </ul>
        </div>
      )}
    </div>
  );
}