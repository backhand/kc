/**
 * kc — spike skeleton.
 *
 * Sole purpose of this file: smoke-test OpenTUI + React under Bun and, after
 * `bun build --compile`, prove the resulting standalone binary renders, takes
 * input, and exits cleanly with the terminal restored. It is NOT a feature —
 * see SPEC.md. Keep it minimal.
 *
 * Exercises exactly what kc will depend on:
 *   - layout      : a bordered box with a title + a footer hint
 *   - render      : a vertical list of dummy items
 *   - input       : ↑/↓ and j/k move the highlighted selection
 *   - clean exit  : `q` and Ctrl+C restore the terminal and leave no residue
 */

import { createCliRenderer, type CliRenderer, type KeyEvent } from "@opentui/core"
import { createRoot, useKeyboard, useRenderer } from "@opentui/react"
import { useState } from "react"

const ITEMS = [
  "Pods",
  "Deployments",
  "Services",
  "Ingresses",
  "ConfigMaps",
] as const

/** Restore the terminal (raw mode off, alt-screen exited) and quit. */
function quit(renderer: CliRenderer): void {
  // destroy() runs OpenTUI's native teardown: leaves the alternate screen,
  // disables raw mode, shows the cursor. Without it the terminal stays wedged.
  renderer.destroy()
  process.exit(0)
}

function App() {
  const renderer = useRenderer()
  const [selected, setSelected] = useState(0)

  useKeyboard((key: KeyEvent) => {
    switch (key.name) {
      case "q":
        quit(renderer)
        return
      case "c":
        // Belt-and-suspenders: createCliRenderer({ exitOnCtrlC: true }) already
        // handles this, but make the contract explicit and version-proof.
        if (key.ctrl) quit(renderer)
        return
      case "up":
      case "k":
        setSelected((i) => (i - 1 + ITEMS.length) % ITEMS.length)
        return
      case "down":
      case "j":
        setSelected((i) => (i + 1) % ITEMS.length)
        return
      default:
        return
    }
  })

  return (
    <box
      style={{ flexDirection: "column", width: 44, padding: 1 }}
      border
      borderStyle="rounded"
      borderColor="#5FAFFF"
      title=" kc · spike "
      titleAlignment="center"
    >
      <box style={{ flexDirection: "column", gap: 0 }}>
        {ITEMS.map((label, i) => {
          const active = i === selected
          return (
            <text key={label} fg={active ? "#000000" : "#C0C0C0"} bg={active ? "#5FAFFF" : undefined}>
              {`${active ? "›" : " "} ${label}`}
            </text>
          )
        })}
      </box>

      <box style={{ marginTop: 1 }}>
        <text fg="#6C6C6C">↑/↓ or j/k to move · q / Ctrl+C to quit</text>
      </box>
    </box>
  )
}

const renderer = await createCliRenderer({
  // Native, signal-safe clean exit on Ctrl+C / SIGINT / SIGTERM.
  exitOnCtrlC: true,
  // Clear the alternate screen on shutdown so quitting leaves nothing behind.
  clearOnShutdown: true,
})

createRoot(renderer).render(<App />)
