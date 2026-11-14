import { useEffect, useState } from "preact/hooks";

// Peers panel (#7): LAN clients with their IPs, polled from
// /api/peers (discovery beacons). Shows the system is alive and
// makes debugging ("why don't they see each other?") obvious.

import { KEY_HEADER } from "./api";
import { loadGroupKey } from "./storage";

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

export function PeersPanel() {
  const [data, setData] = useState<PeersResp | null>(null);
  const [open, setOpen] = useState(true);

  useEffect(() => {
    let alive = true;
    const load = async () => {
      try {
        const key = loadGroupKey();
        const r = await fetch("/api/peers", {
          headers: key ? { [KEY_HEADER]: key } : {},
        });
        if (!r.ok) return;
        const resp = (await r.json()) as PeersResp;
        if (alive) setData(resp);
      } catch {
        /* engine gone; keep last state */
      }
    };
    void load();
    const t = setInterval(load, 3000);
    return () => {
      alive = false;
      clearInterval(t);
    };
  }, []);

  const rows = data
    ? [{ ...data.self, self: true }, ...data.peers].sort((a, b) =>
        a.self ? -1 : b.self ? 1 : (a.name || "").localeCompare(b.name || ""),
      )
    : [];

  return (
    <div class="card">
      <h3 style="margin-top:0">
        LAN clients{" "}
        <button class="secondary" style="padding:2px 10px" onClick={() => setOpen(!open)}>
          {open ? "hide" : "show"}
        </button>
      </h3>
      {open && (
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
                  {"self" in p && p.self ? "★ " : ""}
                  {p.name || p.id.slice(0, 8)}
                </td>
                <td>{p.ip}</td>
                <td class="num">{p.port}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {!data && <p class="muted">discovering LAN clients…</p>}
      {data && data.peers.length === 0 && (
        <p class="muted">no other engines found — is the other machine running?</p>
      )}
      <p class="muted">
        auto-sync: ~/Downloads/chunk-drop-files ↔ other engines (same
        group key), every {5}s.
      </p>
    </div>
  );
}