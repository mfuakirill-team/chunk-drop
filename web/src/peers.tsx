import { useEffect, useState } from "preact/hooks";

// Side panel (#7 + #1.3): LAN clients with their IPs (discovery), the
// concise activity feed (what's uploading / downloading right now),
// and the live receiver progress of the running transfer — all in one
// place so "is the system working?" is answerable at a glance.

import { KEY_HEADER } from "./api";
import { loadGroupKey } from "./storage";
import type { SendState } from "./send";

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
}

const KIND_MARK: Record<string, string> = {
  up: "▲",
  down: "▼",
  info: "•",
  error: "!",
};

export function PeersPanel({ send }: { send: SendState | null }) {
  const [peers, setPeers] = useState<PeersResp | null>(null);
  const [act, setAct] = useState<ActivityResp | null>(null);
  const [open, setOpen] = useState(true);

  useEffect(() => {
    let alive = true;
    const load = async () => {
      const key = loadGroupKey();
      const headers: Record<string, string> = {};
      if (key) headers[KEY_HEADER] = key;
      try {
        const r = await fetch("/api/peers", { headers });
        if (r.ok) {
          const resp = (await r.json()) as PeersResp;
          if (alive) setPeers(resp);
        }
      } catch {
        /* engine gone; keep last state */
      }
      try {
        const r = await fetch("/api/activity", { headers });
        if (r.ok) {
          const resp = (await r.json()) as ActivityResp;
          if (alive) setAct(resp);
        }
      } catch {
        /* same */
      }
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

  const events = act?.events ?? [];
  const latest = events.slice(-12).reverse(); // newest on top, concise

  return (
    <div class="card">
      <h3 style="margin-top:0">
        LAN clients{" "}
        <button class="secondary" style="padding:2px 10px" onClick={() => setOpen(!open)}>
          {open ? "hide" : "show"}
        </button>
      </h3>
      {open && (
        <div>
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

          {/* #1.3: activity — what's uploading / downloading, concise. */}
          <h4 style="margin-bottom:4px">Activity</h4>
          {send?.sessionInfo?.receiver ? (
            <p class="muted" style="margin:4px 0">
              ▲ receiving now: {send.sessionInfo.receiver.receiver}
              {" "}
              ({Object.keys(send.sessionInfo.receiver.have).length} file(s) in progress)
            </p>
          ) : (
            <p class="muted" style="margin:4px 0">
              {act && act.uploads.length > 0
                ? `${act.uploads.length} live session(s) waiting for a receiver`
                : "idle — no transfers in flight"}
            </p>
          )}
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
      <p class="muted">
        auto-sync: ~/Downloads/chunk-drop-files ↔ other engines (same
        group key), every 5s.
      </p>
    </div>
  );
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}