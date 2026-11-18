# Control socket (Unix-domain)

Set `BLASTEM_CTRL_SOCK=/path/ctrl.sock` and the core listens on a Unix-domain
stream socket for external commands: controller input, captures and observation
without window focus or synthetic key events. The MCP server is built on this
socket plus [GDB Remote](gdb.md); scripts can also use it directly.

- The socket is polled once per frame, and also while paused or stopped in GDB.
- One client at a time. The next client is accepted after the previous connection closes.
- The first non-space byte of a connection selects the protocol: `{` means
  acknowledged JSON (v2), anything else means legacy line commands. Do not mix
  them on one connection.

## Acknowledged JSON (v2, akiyan branch)

One JSON object per line; each request gets exactly one reply with the same `id`.
IDs must increase within a connection. Send `hello` first and gate features on the
returned `capabilities`.

```text
→ {"id":1,"version":2,"command":"hello","params":{"client":"me"}}
← {"id":1,"version":2,"ok":true,"result":{"server":"blastem","protocol":2,"capabilities":[...]}}
→ {"id":2,"version":2,"command":"pause","params":{}}
→ {"id":3,"version":2,"command":"advance","params":{"frames":60}}
← {"id":3,"version":2,"ok":true,"result":{"frame":125,"advanced":60,"paused":true,"interrupted":false,"boundary":"frame_sync"}}
→ {"id":4,"version":2,"command":"ram_read","params":{"address":16711680,"size":4}}
```

Errors look like `{"id":5,"version":2,"ok":false,"error":{"code":"invalid_state",...}}`.
Codes: `invalid_request`, `invalid_params`, `unknown_command`, `unsupported`,
`invalid_state`, `not_found`, `io_error`, `busy`, `internal`.

| Command | Params | Result / notes | Capability |
|---|---|---|---|
| `status` | — | `system`, `rom_sha1`, `frame`, `paused`, `boundary` (`frame_sync`, `paused_loop`, `instruction`, `event_poll`) | — |
| `pause` | — | Stop at the next frame boundary | `frame_control` |
| `advance` | `frames` 1..600 | Run N frames and stop; reply when done | `frame_control` |
| `interrupt` | — | Ask a running `advance`/`trace`/`line_trace` to stop | `frame_control` |
| `trace` | `frames` 1..36000, `path`, `watches[]`, optional `until` | JSONL of frames where a watched value changed; `records`, `stop` | `frame_trace` |
| `line_trace` | `frames` 1..600, `path`, optional `kinds` (`cram`,`vsram`,`reg`,`int`) | JSONL of VDP writes and interrupt/RTE with `line`/`slot`; `events` | `line_trace` |
| `ram_read` | `address` FF0000..FFFFFF, `size` 1..256 | `hex`, `frame` (also allowed in a GDB stop) | `paused_work_ram` |
| `speed` | `percent` 25..1000 | Host pacing only; frame counts unchanged | `speed_control` |
| `vdp_dump` | `path` | KITVDMP1 file (VRAM, CRAM, VSRAM, registers) written immediately | `paused_vdp_dump` |
| `z80_state` | — | Z80 registers (no PC), reset/busreq/busack, cycles last frame and total | `z80_observation` |
| `z80_read` | `address` 0..1FFF, `size` 1..256 | `hex`, `frame` | `z80_observation` |
| `snapshot` | `path` | PNG of the last completed frame without running the CPU | `cached_screenshot` |
| `save_flush` | — | SRAM written, fsynced and renamed before the reply | `sram_flush` |
| `save_state` / `load_state` | `name` (1..64 of `[A-Za-z0-9][A-Za-z0-9._-]*`) | Files in `BLASTEM_STATE_DIR`, bound to the ROM's SHA-1 | `state` |
| `reset` | optional `kind`: `soft` | Deterministic reset at the first cycle of the next run (`hard` is `unsupported`) | `reset` |
| `audio_start` / `audio_stop` | `path`, `max_seconds` 1..30 / — | WAV of the host mixer | `host_audio_capture` |

Rules:

- `trace` watch: `{space, address, size}` with `space` = `ram` (default), `cram`,
  `vsram`, `vdp_reg`, `vram`, `z80_ram`, `z80_frame`; `size` 1, 2 or 4.
  `until`: `{watch, op: eq|ne|changed, value, mask}` (`watch` is an index).
- Long runs (`advance`, `trace`, `line_trace`) reply when finished. Meanwhile only
  `status` and `interrupt` are accepted; everything else answers `busy`.
- Closing the connection stops a running command at the next frame boundary.
- Output paths must be absolute; existing files are never overwritten.
- Frame-based commands require a frame stop (`pause` first) and answer
  `unsupported` in GDB (`-D`) runs, Sega CD and 32X sessions.

The full contract, including limits and parser rules, is in
[control-protocol-v2.md](control-protocol-v2.md).

## Legacy line commands (ulalume fork)

Newline-terminated, unacknowledged. Results appear as files or as `KIT ...` lines
on stdout.

| Command | Effect |
|---|---|
| `pad <n> down\|up <button>` | Press/release a button (the MCP button tools use this) |
| `screenshot <path>` | Save the next rendered frame (.png or .ppm) |
| `burst <count> <prefix>` | Save consecutive frames as `<prefix>00000.png`, …; `burst 0` cancels |
| `script <path>` | Load a frame-keyed input script (below) |
| `frame` | Print `KIT FRAME frame=<N>` |
| `logframes on\|off` | Append ` @f<N>` to KDEBUG messages |
| `vramdump <path>` | KITVDMP1 snapshot at the next frame boundary; prints `KIT VDUMP frame=<N> file=<path>` |
| `vramhash on\|off` | Per-frame `KIT VHASH frame=<N> hash=<fnv1a>` over VRAM, CRAM and VSRAM |
| `prof bracket <name> <start_hex> <end_hex>` | 68000 cycles spent between two PCs, per frame |
| `prof lap <name> <addr_hex>` | Cycles between consecutive passes of one PC (e.g. main-loop period) |
| `prof log on\|off`, `prof clear` | Emit/stop `KIT PROF frame=<N> total=<cycles> <name>=<cycles>:<hits> ...`; clear all brackets |
| `hud on\|off`, `hud gamefps <addr_hex>`, `hud idle <addr_hex\|auto>` | Game FPS and CPU% in the window title and `KIT HUD` lines |
| `watchlog add <addr_hex>` / `watchlog clear` | `KIT WATCH` write log (NEW_CORE interpreter builds only) |

Buttons: `up down left right a b c x y z start mode`.

### Input scripts (`script`)

Frames count video frames since reset, so the same script on the same ROM gives the
same images every run.

```text
# <frame> down|up <port> <buttons,comma,separated>
120 down 1 start
126 up 1 start
300 shot /abs/out/title.png
600 burst 30 /abs/out/fade-
650 log reached menu
anchor MENU READY        # following frames count from the frame whose KDEBUG output contains this text
10 down 1 a
```

An action whose frame has already passed when the script loads runs at once and
prints `KIT SCRIPT warn` (that run is no longer reproducible).

## Output: KIT lines and ROM debug messages

`KIT ...` lines and ROM debug output go to stdout (stderr in GDB `-D` runs). In an
MCP session they are in `blastem.stdout.log` inside the runtime directory reported
by `blastem_status`.

A ROM prints `KDEBUG MESSAGE: <text>` by writing characters to VDP register 30, and
measures code with register 31 (`KDEBUG TIMER: <cycles>`), following the Gens KMod
convention (e.g. SGDK `KLog` and `KDebug_Alert`).

## Environment variables

| Variable | Effect |
|---|---|
| `BLASTEM_CTRL_SOCK` | Control socket path |
| `BLASTEM_GDB_PORT` | With `-D`, listen for GDB on localhost TCP instead of stdin/stdout |
| `BLASTEM_SAVE_DIR` | Absolute SRAM directory independent of the ROM name (`save.sram`) |
| `BLASTEM_STATE_DIR` | Absolute directory for `save_state`/`load_state` |
| `BLASTEM_HIDDEN_WINDOW=1` | Create the window hidden |
| `BLASTEM_NO_GUI=1` | Skip the menu UI |
| `XDG_CONFIG_HOME`, `XDG_DATA_HOME` | Absolute paths isolate configuration and saves from the user's normal setup |
| `SDL_VIDEODRIVER=dummy`, `SDL_AUDIODRIVER=dummy` | Run without display or audio devices (what MCP `headless` sets) |
