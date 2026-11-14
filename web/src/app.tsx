import { useState } from "preact/hooks";
import type { ComponentChild } from "preact";

// App shell: receive mode when #k= is present, sender dashboard at /,
// mode switch for manual key entry (M0-TASKS.md steps 7–8). The
// dashboard passes its transfer state up so the side panel (#1.3)
// can show live progress without polling the sender card.

import { parseFragmentKey } from "./link";
import { ActivityPanel, PeersPanel } from "./peers";
import { ReceivePage } from "./receive";
import { SendPage, type SendState } from "./send";
import { setFragment } from "./link";

export function App() {
  // The QR link is served at /r/ and opened on other devices, where the
  // dashboard is unreachable by design (local-only, PROTOCOL §4.1) — so
  // a /r/ page always starts in receive mode (no key → clear error from
  // ReceivePage, never a dashboard that cannot work), and it carries no
  // "back to sender dashboard" affordance. On the engine's own page (/)
  // switching into receive mode keeps the back button.
  const standaloneReceiveLink = location.pathname.startsWith("/r/");
  const [key, setKey] = useState(parseFragmentKey(location.hash));
  const [mode, setMode] = useState<"receive" | "send">(
    parseFragmentKey(location.hash) || standaloneReceiveLink ? "receive" : "send",
  );
  const [send, setSend] = useState<SendState | null>(null);

  const goReceive = (k: string) => {
    setKey(k);
    setMode("receive");
    setFragment(k);
  };
  const goSend = () => {
    setMode("send");
    setFragment("");
  };

  return (
    <div>
      <header class="site">
        <h1>ChunkDrop</h1>
        <span class="muted">chunked · resumable · private</span>
      </header>
      {mode === "receive" ? (
        <ReceivePage
          sessionKey={key}
          onBack={standaloneReceiveLink ? undefined : goSend}
        />
      ) : (
        <div class="columns">
          <SendPage onStart={goReceive} onChange={setSend} />
          <div class="sidecol">
            <PeersPanel />
            <ActivityPanel send={send} />
          </div>
        </div>
      )}
      {!(mode === "receive" && standaloneReceiveLink) && (
        <footer class="muted">
          <p>
            {mode === "receive" ? (
              <button class="secondary" onClick={goSend}>
                ← sender dashboard
              </button>
            ) : (
              <span>
                Receiving on this device? Open the QR link or paste the session
                key below.
              </span>
            )}
          </p>
        </footer>
      )}
    </div>
  );
}

export type { ComponentChild };