# BlastEM MCP: guide for AI agents

How to drive and verify Mega Drive / Genesis software through the `blastem-mcp`
server **without looking at images**. Prefer the text and number tools below;
use screenshots only when a human needs to see the result.

Everything here is title-independent. Game-specific meaning (which RAM variable
is the scene ID, which palette is "selected") belongs in your project, usually
via a symbol file.

## 1. Start a session

```json
blastem_start {
  "binary_path": "/abs/blastem/blastem",
  "rom_path": "/abs/build/rom.bin",
  "symbols_path": "/abs/build/symbol.txt",
  "save_dir": "/abs/profiles/my-game",
  "state_dir": "/abs/states/my-game",
  "headless": true
}
```

| Field | Use |
|---|---|
| `symbols_path` | `nm -n` style file (`ADDRESS TYPE NAME`). Enables `symbol` everywhere an address is accepted (`NAME`, `NAME+4`, `NAME-0x10`). |
| `save_dir` | Battery SRAM profile shared across ROM rebuilds (file name/hash may change). Use `save_flush` before relying on the file. |
| `state_dir` | Where named save states live. Omit for a private directory deleted at `blastem_stop`. |
| `headless` | No X server/Xvfb needed (SDL dummy drivers, software renderer). Use this on servers and in CI. |
| `debug` | Start the GDB stub. **Only for CPU debugging**: frame tools, traces, states, and reset are unavailable in debug sessions. |

Always finish with `blastem_stop` (it flushes the save profile and releases held buttons).

## 2. The core loop: pause, act, observe

1. `frame_pause` — stop at the next frame boundary. Every tool below assumes this.
2. Act: `frame_advance {frames}`, `button_press`, `input_sequence`, `reset`, `state_load`.
3. Observe without advancing: `ram_read`, `vdp_layout`, `z80_state`, `paused_screenshot`.

Frame numbers in results are the VDP frame counter. Compare frames, not wall time.

### Input

| Tool | Behaviour |
|---|---|
| `button_press {pad, button, frames}` | Hold for exactly N frames (1..600), release at the final boundary. Best for "press A". |
| `input_sequence {frames, events:[{frame, pad, button, action}]}` | One call runs up to 36000 frames, applying `down`/`up` at frame offsets (0 = before the first frame). Releases everything at the end unless `keep_held`. |
| `button_down` / `button_up` / `release_all_buttons` | Manual holding across several calls. |

Buttons: `up down left right a b c x y z start mode`. Pads 1 and 2.

### Reproduce a scene cheaply

```text
state_save {"name": "before-bug"}
input_sequence {...}            # try something
frame_trace {...}               # measure it
state_load {"name": "before-bug"}
input_sequence {...}            # try again: same state + same input = same result
```

States are tied to the ROM (SHA-1 checked) and BlastEM build. After rebuilding the
ROM, use the game's own SRAM save (`save_dir`) to resume instead.

`reset` releases buttons and performs a deterministic soft reset at the start of
the next advance. After `state_load`, advance one frame before `paused_screenshot`
(the cached image is from before the load).

## 3. Find *when* something happens: `frame_trace`

Runs up to 36000 frames and samples 1..16 values at every frame boundary; returns
only frames where a value changed (plus the start). Optional `until` stops at the
first matching frame. This replaces loops of `frame_advance` + `ram_read`.

```json
frame_trace {
  "frames": 3600,
  "watches": [
    {"symbol": "scene_id", "size": 2},
    {"name": "backdrop", "space": "vdp_reg", "address": 7},
    {"name": "colour15", "space": "cram", "address": 30},
    {"name": "z80_busy", "space": "z80_frame", "address": 0, "size": 4}
  ],
  "until": {"watch": "scene_id", "op": "eq", "value": 42}
}
```

- `space`: `ram` (default, FF0000..FFFFFF), `cram` (byte offset 0..127), `vsram`,
  `vdp_reg` (0..23), `vram`, `z80_ram` (0..1FFF), `z80_frame` (size 4; address 0 =
  Z80 cycles executed last frame, 1 = stopped by 68000 bus request, 2 = in reset).
- `size` 1, 2 or 4 (big-endian). Default 2 for cram/vsram, else 1.
- `until.op`: `eq`, `ne`, `changed`; `mask` applies before comparing.
- Result: `records[]` with `frame`, `values` by name, and `changed` names;
  `trace.stop` is `frames`, `until`, or `interrupted`. `max_records` limits the
  inline list; the full JSONL stays in `artifact.path`.

Typical uses: frame a fade starts/ends, when a flag flips, how many frames a wait
lasts, whether two runs diverge.

## 4. Read the screen as text: `vdp_layout`

Decodes the stopped VDP state; no image is rendered.

- `mode` (H32/H40, V28/V30, display enabled), `backdrop`, `scroll` runs.
- `planes[]` (`a`, `b`, `window`): `grid` has one string per screen row; each cell is
  `TILE/PALETTE` in hex with `*` priority, `h`/`v` flips, `.` empty, `-` hidden by the window.
  `non_empty` and `palettes` give counts. `cells: true` returns per-cell objects.
- `sprites`: link-chain order, position, size, tile, palette, visibility,
  `overflow_lines` (per-line limit exceeded), and `problems` (link loops).
- `patterns: true` adds CRC32 of tile pixel data, so you can compare content
  independent of tile numbers. `rect` limits to a cell rectangle.

Use it to check "text drawn in rows 2..6", "character sprite gone", "layer
contents identical before and after a change", or to diff two scenes.

## 5. Raster effects and interrupt timing: `line_trace`

Runs 1..600 frames and logs CRAM, VSRAM and VDP register writes plus interrupt
acknowledge/RTE with the VDP `line` (vcounter) and `slot` where each took effect.

```json
line_trace {"frames": 1, "kinds": ["cram", "int"], "lines": {"from": 30, "to": 80}}
```

`summary.interrupts["4"]` (horizontal) / `["6"]` (vertical) gives counts, first/last
line, and 68000 cycles from acknowledge to RTE (min/avg/max). One line is about
488 68000 cycles; a handler exceeding that misses the next line.

## 6. Sound

### Sound CPU: `z80_state`

Registers (no PC: the JIT does not keep it current), reset/bus-request state,
cycles executed / stopped in the last frame and in total, and optionally Z80 RAM
(`address`, `size`). For per-frame history, trace `z80_frame` with `frame_trace`.

### Quick listen: `audio_capture {frames}`

WAV from the host mixer for 1..600 frames. Good for "is there sound", not for
sample-exact comparisons.

### Measure levels and events (CLI, not MCP)

```sh
python3 tools/record_stems.py ROM out/run 3600 --record-input input.txt [--per-channel]
```

Deterministic offline recordings of the mix and each stem (FM, DAC, PSG, or 11
channels). Outputs `out/run.levels.jsonl` (per frame `[AC RMS, peak, DC]`),
`out/run.stems.json` (totals, active frame ranges, mix-vs-stems residual), and
`out/run.sound.jsonl` (YM2612/PSG register writes with frame and cycle; DAC sample
writes counted per frame). Judge activity by AC RMS: the YM2612 output has a DC offset.

Input file lines: `COMPLETED_FRAME PAD down|up BUTTON` (e.g. `180 1 down a`).

## 7. Video for humans (CLI)

```sh
blastem/blastem --record out/run 3600 --record-input input.txt ROM
python3 tools/mux_capture.py out/run out/run.mp4 --crop auto
```

Headless, faster than real time, game-time synchronised APNG + WAV. `run.json`
records `active_areas` (the real picture rectangle per display-mode run);
`--crop auto` uses it. `--record-no-video`, `--record-mute LIST`,
`--record-sound-log` are also available.

## 8. Symbols and CPU debugging

- `symbol_lookup {name}` searches names; `{address}` names an address
  (`func+0x1c`). `ram_read`, `memory_read/write`, `breakpoint_set/remove` and trace
  watches accept `symbol`.
- With `debug: true`: `cpu_registers` (adds `pc_location`), `memory_read/write`,
  `breakpoint_set/remove`, `watchpoint_set/remove {symbol|address, size}` (stops
  after a **write**; reply `T05watch:ADDR`), `cpu_step`, `cpu_continue {timeout_ms}`.
  If nothing stops the CPU within the timeout (default 10 s) it is interrupted and
  you get `interrupted: true` plus the stop location; nothing hangs.
- "Who writes this variable?": `watchpoint_set {"symbol": "scene_id", "size": 2}`,
  `cpu_continue`, then `cpu_registers` → `pc_location` is the writing code.
- In a GDB stop the CPU may be mid-frame: `paused_screenshot` shows the last
  completed frame, `ram_read`/`vdp_layout`/`z80_state` show the current state.

## 9. Images, when needed

`paused_screenshot` (last completed frame, no CPU progress), `screenshot` (next
frame), `vram_tiles` (four tile sheets, one per palette), `vdp_snapshot` (raw
KITVDMP1 dump). Prefer `vdp_layout` and traces for automated checks.

## 10. Errors and limits

| Error / situation | Meaning |
|---|---|
| `invalid_state` | Not paused at a frame boundary (call `frame_pause`), or a state from another ROM. |
| `unsupported` | Debug (GDB) session, Sega CD / 32X, or a feature not built in. |
| `busy` | A frame run is in progress; wait for it or use `frame_interrupt`. |
| `not_found` | Unknown state name. |
| `io_error` | File write/read failed, or a state file is corrupt. |

- Frame runs: `frame_advance` ≤ 600, `frame_trace`/`input_sequence` ≤ 36000,
  `line_trace` ≤ 600 per call. `frame_interrupt` stops a running one.
- `speed_set {percent: 25..1000}` changes host pacing only; frame counts and
  results stay the same. Set 100 before `audio_capture`.
- One session per MCP server process. Do not mix GDB stops and frame tools in one
  session.

## 11. Without MCP: GDB, the control socket, and environment variables

Everything above is also reachable directly:

- [docs/gdb.md](../docs/gdb.md) — `-D` with `BLASTEM_GDB_PORT`, supported packets
  (write watchpoints, Ctrl-C interrupt), and caveats (an unsupported packet
  terminates BlastEM; one client; frame commands are unavailable in `-D` runs).
- [docs/control-socket.md](../docs/control-socket.md) — `BLASTEM_CTRL_SOCK`, every
  acknowledged JSON (v2) command with params and capability, the legacy line
  commands inherited from the ulalume fork (`pad`, `screenshot`, `burst`, `script`,
  `vramhash`, `prof`, `hud`, ...), the input script format, `KIT`/`KDEBUG` output,
  and all environment variables.
- [docs/control-protocol-v2.md](../docs/control-protocol-v2.md) — the full v2 contract.
