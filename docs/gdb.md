# GDB Remote (68000 debugging)

The core speaks the GDB Remote Serial Protocol for the main 68000. The base
implementation comes from the ulalume fork; the akiyan branch adds Ctrl-C
interruption of a running `continue` and keeps the control socket usable while
GDB holds the CPU.

## Starting

```sh
# Listen on localhost TCP
BLASTEM_GDB_PORT=2345 ./blastem rom.bin -D
m68k-elf-gdb build/rom.elf -ex 'target remote localhost:2345'

# Without a port, GDB talks over stdin/stdout
m68k-elf-gdb build/rom.elf -ex 'target remote | ./blastem rom.bin -D'
```

Through MCP, `blastem_start {"debug": true}` does the same and exposes
`cpu_registers`, `memory_read`/`memory_write`, `breakpoint_set`/`remove`,
`watchpoint_set`/`remove`, `cpu_step` and `cpu_continue {timeout_ms}`
(see [mcp/AGENT_GUIDE.md](../mcp/AGENT_GUIDE.md)).

## Supported packets

| Feature | Packets |
|---|---|
| Registers | `g` (read all), `p`/`P` (one register: D0–D7, A0–A7, SR, PC). No `G`. |
| Memory | `m`, `M` (24-bit address space). Binary `X` is answered empty, so gdb falls back to `M`. |
| Breakpoints | `Z0`/`Z1`, `z0`/`z1` |
| Watchpoints | `Z2`/`z2`, write only. Read/access watchpoints (`Z3`/`Z4`) are answered empty. A hit replies `T05watch:ADDR`. |
| Run control | `c`, `s`, `vCont;c/s`, `?`. `k` exits BlastEM. |
| Interrupt | Ctrl-C (0x03) while `c` runs stops the CPU before the next instruction, within about one frame, and replies `S02` (TCP connections only). |
| Queries | `qSupported`, `qAttached`, `qOffsets`, `qC`, `qfThreadInfo`/`qsThreadInfo`, `qSymbol`, `qTStatus` |

## Caveats

- **An unsupported packet terminates BlastEM** ("Command ... is not implemented,
  exiting"). Stock `gdb` stays within the list above; check other clients first.
- One GDB client per process.
- While GDB holds the CPU, the [control socket](control-socket.md) keeps working:
  `status` reports `boundary: instruction`, and `ram_read`, `vdp_dump`,
  `z80_state`/`z80_read`, `snapshot` and `save_flush` are allowed. The CPU may be in
  the middle of a frame; `snapshot` returns the last completed frame.
- Frame-based commands (`pause`, `advance`, `trace`, `line_trace`, states, reset,
  speed, audio capture) answer `unsupported` in `-D` runs. Use either GDB stops or
  frame stops in one session, not both.
- In `-D` runs, ROM debug output (`KDEBUG ...`) and `KIT ...` lines go to stderr
  instead of stdout.
- Only the main 68000 is debuggable. Observe the Z80 with `z80_state`/`z80_read`.
