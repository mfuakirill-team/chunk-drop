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
import { UploadPage } from "./upload";
import { setFragment } from "./link";

export function App() {
  // The QR link is served at /r/ and opened on other devices, where the
  // dashboard is unreachable by design (local-only, PROTOCOL §4.1) — so
  // a /r/ page always starts in receive mode (no key → clear error from
  // ReceivePage, never a dashboard that cannot work), and it carries no
  // "back to sender dashboard" affordance. On the engine's own page (/)
  // switching into receive mode keeps the back button.
  // /u/ (M1) is the phone→PC push page: same standalone treatment —
  // the upload key lives in the fragment, the dashboard is not
  // reachable from the phone, so there is no back affordance either.
  const standaloneReceiveLink = location.pathname.startsWith("/r/");
  const uploadLink = location.pathname.startsWith("/u/");
  const [key, setKey] = useState(parseFragmentKey(location.hash));
  const [mode, setMode] = useState<"receive" | "send" | "upload">(
    uploadLink ? "upload"
      : parseFragmentKey(location.hash) || standaloneReceiveLink ? "receive"
      : "send",
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
      {mode === "upload" ? (
        <UploadPage uploadKey={key} />
      ) : mode === "receive" ? (
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
      {!(mode === "receive" && standaloneReceiveLink) && mode !== "upload" && (
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