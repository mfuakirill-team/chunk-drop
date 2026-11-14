import { useState } from "preact/hooks";
import type { ComponentChild } from "preact";

// App shell: receive mode when #k= is present, sender dashboard at /,
// mode switch for manual key entry (M0-TASKS.md steps 7–8).

import { parseFragmentKey } from "./link";
import { ReceivePage } from "./receive";
import { SendPage } from "./send";
import { setFragment } from "./link";

export function App() {
  const [key, setKey] = useState(parseFragmentKey(location.hash));
  const [mode, setMode] = useState<"receive" | "send">(
    parseFragmentKey(location.hash) ? "receive" : "send",
  );

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
        <ReceivePage sessionKey={key} onBack={goSend} />
      ) : (
        <SendPage onStart={goReceive} />
      )}
      <footer class="muted">
        <p>
          {mode === "receive" ? (
            <button class="secondary" onClick={goSend}>
              ← sender dashboard
            </button>
          ) : (
            <span>
              Receiving on this device? Open the QR link or paste the session key
              below.
            </span>
          )}
        </p>
      </footer>
    </div>
  );
}

export type { ComponentChild };