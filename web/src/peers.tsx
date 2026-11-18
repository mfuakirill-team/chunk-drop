import { useEffect, useState } from "preact/hooks";

// Side panel (#7 + #1.3): three separate frames — "Internet mesh"
// (hub connection status, M2 step ⑤), "Clients" (the merged mesh
// client list: LAN-discovered engines + hub devices, joined by the
// persistent device ID both sources carry, with per-peer route state
// and mode controls, M2 step ④) and, below it, "Activity" (what's
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
  hub?: boolean; // reachable over the internet via the hub
  route?: string; // lan | tunnel | relay ("" = internet peer not dialed yet)
  mode?: string; // auto | tunnel | relay | off (dashboard toggle)
}

interface PeersResp {
  self: PeerInfo;
  peers: PeerInfo[];
}

interface HubResp {
  enabled: boolean;
  url?: string;
  connected?: boolean;
  last_error?: string;
  devices?: { id: string; name: string }[];
  relay_only?: boolean; // CHUNKDROP_RELAY_ONLY=1 (step ③ diagnosis knob)
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

/** Poll /api/activity once (group-key gated, dashboard-only route). */
export function fetchActivity(): Promise<ActivityResp | null> {
  return fetchJson<ActivityResp>("/api/activity");
}

export type { ActivityResp, IncomingUpload };

/** POST /api/peer/mode (step ④): steer one peer's route / pause it. */
async function postPeerMode(id: string, mode: string): Promise<boolean> {
  const key = loadGroupKey();
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (key) headers[KEY_HEADER] = key;
  try {
    const r = await fetch("/api/peer/mode", {
      method: "POST",
      headers,
      body: JSON.stringify({ id, mode }),
    });
    return r.ok;
  } catch {
    return false;
  }
}

function fmtTime(iso: string): string {
  const d = new Date(iso);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

// Internet mesh card (M2 step ⑤): hub reachability, online group
// members, and the forced-relay note — so "sync is slow" has a face.
export function HubPanel() {
  const [hub, setHub] = useState<HubResp | null>(null);

  useEffect(() => {
    let alive = true;
    const load = async () => {
      const resp = await fetchJson<HubResp>("/api/hub");
      if (resp && alive) setHub(resp);
    };
    void load();
    const t = setInterval(load, 3000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  if (!hub) return null; // engine unreachable; keep the panel hidden

  if (!hub.enabled) {
    return (
      <div class="card">
        <h3 style="margin-top:0">Internet mesh</h3>
        <p class="muted" style="margin-bottom:0">
          off — no hub configured (engine.toml hub_url). LAN clients below work regardless.
        </p>
      </div>
    );
  }

  const devices = hub.devices ?? [];
  const names = devices.map((d) => d.name || d.id.slice(0, 8)).join(", ");
  return (
    <div class="card">
      <h3 style="margin-top:0">Internet mesh</h3>
      <p style="margin:4px 0">
        {hub.connected ? (
          <span>● connected</span>
        ) : (
          <span class="muted">○ connecting to the hub…</span>
        )}
        {devices.length > 0 ? ` — ${devices.length} online: ${names}` : hub.connected ? " — nobody else online" : ""}
      </p>
      {hub.last_error && <p class="muted" style="margin:4px 0; overflow-wrap:anywhere">last error: {hub.last_error}</p>}
      {hub.relay_only && (
        <p class="muted" style="margin:4px 0">
          ⚠ relay forced (CHUNKDROP_RELAY_ONLY) — every transfer goes through the hub.
        </p>
      )}
      <p class="muted" style="margin-bottom:0">
        Engines find each other here and sync like on the LAN; the hub never sees file bytes.
      </p>
    </div>
  );
}

// Route badge for a Clients row (step ④): what the peer actually used.
function routeMark(p: PeerInfo): string {
  if (p.mode === "off") return "⏸";
  if (p.hub) return p.route === "relay" ? "🌐 relay" : p.route === "tunnel" ? "🌐 tunnel" : "🌐";
  return "lan";
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

  // ip/id cell: a hub-only client has no LAN address — show the first
  // characters of its device ID instead (full ID in the tooltip). A
  // 🌐 marks internet reachability (LAN+hub and hub-only alike);
  // LAN-only clients get neither.
  const addr = (p: PeerInfo): string =>
    p.ip ? p.ip : `${p.id.slice(0, 8)}…`;

  // Per-peer mode control (step ④): internet peers choose the route
  // (auto/tunnel/relay) or pause sync; LAN peers can be paused. The
  // select binds to the engine's reported mode; a change POSTs and
  // the next poll (3 s) confirms.
  const modeCell = (p: PeerInfo) => {
    if (p.self) return <td class="num" />;
    const opts = p.hub ? ["auto", "tunnel", "relay", "off"] : ["auto", "off"];
    return (
      <td class="num">
        <select
          value={p.mode === "off" ? "off" : opts.includes(p.mode ?? "auto") ? p.mode : "auto"}
          onChange={(e) => void postPeerMode(p.id, (e.target as HTMLSelectElement).value)}
          title="route / pause: auto = tunnel first, relay fallback; off pauses sync with this peer"
          style="font-size:11px; padding:0 2px"
        >
          {opts.map((m) => (
            <option value={m}>{m}</option>
          ))}
        </select>
      </td>
    );
  };

  return (
    <div class="card">
      <h3 style="margin-top:0">Clients</h3>
      <table>
        <thead>
          <tr>
            <th>name</th>
            <th>ip/id</th>
            <th class="num">route</th>
            <th class="num">mode</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((p) => (
            <tr style={p.mode === "off" && !p.self ? "opacity:0.55" : ""}>
              <td>
                {p.self ? "★ " : ""}
                {p.name || p.id.slice(0, 8)}
              </td>
              <td title={p.id}>{addr(p)}</td>
              <td class="num" title={p.route === "relay" ? "syncs through the hub (tunnel failed or forced)" : p.route === "tunnel" ? "direct device-to-device tunnel" : ""}>
                {routeMark(p)}
              </td>
              {modeCell(p)}
            </tr>
          ))}
        </tbody>
      </table>
      {peers && peers.peers.length === 0 && (
        <p class="muted">no other clients found — is the other machine running?</p>
      )}
      {!peers && <p class="muted">discovering clients…</p>}
      <p class="muted" style="margin-bottom:0">
        lan = direct; 🌐 = over the internet (tunnel = P2P, relay = through the hub). mode:
        auto/tunnel/relay steer the route; ⏸ off pauses sync with that peer in both
        directions — this engine stops pulling from it AND refuses its pulls (a restart
        returns everyone to auto). auto-sync: Downloads ↔ other engines (same group key),
        every 5s. Deletions are signalled to every client (replayed for 24h to ones that
        were offline).
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
  // the session's have-bitmap indices into the started manifest — so
  // the sender can verify WHAT is going where, not just how much.
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
            <p class="muted" style="margin:4px 0; overflow-wrap:anywhere">
              ▲ receiving now: {liveReceiver.receiver} (
              {Object.keys(liveReceiver.have).length} file(s) in progress
              {receivingNames.length > 0 ? `: ${namesLabel(receivingNames, receivingNames.length)}` : ""})
            </p>
          ) : (act?.incoming?.length ?? 0) > 0 ? null : (
            <p class="muted" style="margin:4px 0">
              {act && act.uploads.length > 0
                ? `${act.uploads.length} live session(s) waiting for a receiver`
                : "idle — no transfers in flight"}
            </p>
          )}
          {/* phone→PC pushes (M1) — names included so the dashboard
              owner can verify the right files are arriving; finished
              batches show as done (key still live for "send more") */}
          {(act?.incoming ?? []).map((u) => (
            <p class="muted" style="margin:4px 0; overflow-wrap:anywhere">
              {u.finished ? "✓ phone sent" : "▼ phone pushing"} {u.files} file(s)
              {u.names?.length ? ` — ${namesLabel(u.names, u.files)}` : ""} —{" "}
              {humanBytes(u.bytesDone)}
              {u.bytesTotal > 0 ? ` / ${humanBytes(u.bytesTotal)}` : ""}
              {u.finished ? " — batch done, link still open" : ""}
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
