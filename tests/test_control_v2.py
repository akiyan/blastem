"""Actual core tests; run with xvfb-run -a python3 -m unittest discover -s tests."""
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import time
import unittest
import wave

ROOT = Path(__file__).resolve().parents[1]
CORE = ROOT
FIXTURE = ROOT / "mcp/testdata/fixture/build/blastem-mcp-fixture.bin"


@unittest.skipUnless(FIXTURE.exists(), "requires the built fixture ROM")
class ControlV2(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="bm-v2-")
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        self.profile = self.work / "persistent"
        self.profile.mkdir()
        self.rom = self.work / "build-A.bin"
        shutil.copy2(FIXTURE, self.rom)
        self.proc = None
        self.connection = None
        self.addCleanup(self.stop)

    def start(self):
        self.log = (self.work / "core.log").open("a")
        (self.work / "states").mkdir(exist_ok=True)
        env = dict(os.environ, SDL_AUDIODRIVER="dummy", BLASTEM_NO_GUI="1", BLASTEM_STATE_DIR=str(self.work / "states"),
                   **({} if os.environ.get("DISPLAY") else {"SDL_VIDEODRIVER": "dummy"}),
                   BLASTEM_CTRL_SOCK=str(self.work / "control.sock"),
                   BLASTEM_SAVE_DIR=str(self.profile), XDG_CONFIG_HOME=str(self.work / "config"),
                   XDG_DATA_HOME=str(self.work / "data"))
        self.proc = subprocess.Popen([str(CORE / "blastem"), str(self.rom)], cwd=CORE,
                                     env=env, stdout=self.log, stderr=self.log)
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            c = socket.socket(socket.AF_UNIX)
            try:
                c.connect(str(self.work / "control.sock"))
                c.settimeout(3)
                self.connection = c
                self.reader = c.makefile("rb")
                self.addCleanup(self.reader.close)
                self.sequence = 0
                self.call("hello", {"client": "test"})
                return
            except (FileNotFoundError, ConnectionRefusedError):
                c.close()
                time.sleep(.02)
        self.fail("core did not accept control connection: " + (self.work / "core.log").read_text())

    def stop(self):
        if self.connection:
            self.reader.close()
            self.connection.close()
            self.connection = None
        if self.proc:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait()
            self.proc = None
            self.log.close()

    def raw(self, data):
        self.connection.sendall(data + b"\n")
        return json.loads(self.reader.readline())

    def call(self, command, params=None, ok=True):
        self.sequence += 1
        response = self.raw(json.dumps(dict(id=self.sequence, version=2, command=command,
                                            params=params or {})).encode())
        self.assertEqual(response["id"], self.sequence)
        self.assertEqual(response["ok"], ok, response)
        return response

    def test_flush_and_distinct_rom_names(self):
        # Preseed a battery image; changing ROM name/hash must still load it.
        original = bytes([0x37]) + bytes(32767)
        (self.profile / "save.sram").write_bytes(original)
        self.start()
        result = self.call("save_flush")["result"]
        self.assertEqual(result["bytes"], 32768)
        self.assertEqual((self.profile / "save.sram").read_bytes(), original)
        self.stop()
        self.rom = self.work / "build-B.bin"
        data = bytearray(FIXTURE.read_bytes())
        data[0x1D0] ^= 1  # different unused header text and ROM hash
        self.rom.write_bytes(data)
        self.start()
        self.call("save_flush")
        self.assertEqual((self.profile / "save.sram").read_bytes(), original)
        self.assertEqual(list(self.profile.glob("*.tmp.*")), [])

    def test_flush_io_failure_is_reported(self):
        self.start()
        # Renaming a regular temp file over a directory must fail, even as root.
        (self.profile / "save.sram").mkdir()
        self.assertEqual(self.call("save_flush", ok=False)["error"]["code"], "io_error")
        self.assertTrue((self.profile / "save.sram").is_dir())
        self.assertEqual(list(self.profile.glob("*.tmp.*")), [])

    def test_protocol_rejects_bad_requests_and_recovers(self):
        self.start()
        for bad in [b'{"id":2,"id":3,"version":2,"command":"status","params":{}}',
                    b'{"id":2,"version":2,"command":"status","params":{"x":1.5}}',
                    b'{"id":2,"version":2,"command":"status","params":{"x":"\xff"}}']:
            self.assertFalse(self.raw(bad)["ok"])
        self.call("status")
        self.assertEqual(self.call("unknown", ok=False)["error"]["code"], "unknown_command")

    def test_snapshot_requires_stopped_cpu_and_valid_path(self):
        self.start()
        self.assertEqual(self.call("snapshot", {"path": str(self.work / "shot.png")}, ok=False)["error"]["code"], "invalid_state")
        for path in ("relative.png", "/tmp/a\0b"):
            self.assertEqual(self.call("snapshot", {"path": path}, ok=False)["error"]["code"], "invalid_params")
        self.assertFalse((self.work / "shot.png").exists())

    def test_pause_and_exact_frame_counts(self):
        self.start()
        first = self.call("pause")["result"]
        self.assertTrue(first["paused"])
        previous = first["frame"]
        for frames in (1, 2, 17, 1):
            result = self.call("advance", {"frames": frames})["result"]
            self.assertEqual(result["advanced"], frames)
            self.assertEqual(result["frame"], previous + frames)
            self.assertFalse(result["interrupted"])
            previous = result["frame"]
            time.sleep(.04)
            status = self.call("status")["result"]
            self.assertEqual(status["frame"], previous)
            self.assertEqual(status["boundary"], "frame_sync")
            self.assertTrue(status["paused"])
        shot = self.call("snapshot", {"path": str(self.work / "paused.png")})["result"]
        self.assertEqual(shot["frame"], previous)
        self.assertTrue((self.work / "paused.png").read_bytes().startswith(b"\x89PNG"))
        for frames in (0, -1, 601, True):
            self.assertEqual(self.call("advance", {"frames": frames}, ok=False)["error"]["code"], "invalid_params")

    def test_advance_can_be_interrupted_and_rejects_busy_mutations(self):
        self.start()
        self.call("pause")
        self.sequence += 1
        advance_id = self.sequence
        self.connection.sendall(json.dumps(dict(id=advance_id, version=2, command="advance", params={"frames":600})).encode() + b"\n")
        busy = self.call("save_flush", ok=False)
        self.assertEqual(busy["error"]["code"], "busy")
        self.call("interrupt")
        done = json.loads(self.reader.readline())
        self.assertEqual(done["id"], advance_id)
        self.assertTrue(done["result"]["interrupted"])
        self.assertLess(done["result"]["advanced"], 600)
        self.assertTrue(done["result"]["paused"])
        self.assertEqual(self.call("advance", {"frames":1})["result"]["advanced"], 1)

    def test_disconnect_stops_bounded_run(self):
        self.start()
        self.call("pause")
        self.sequence += 1
        self.connection.sendall(json.dumps(dict(id=self.sequence, version=2, command="advance", params={"frames":600})).encode() + b"\n")
        time.sleep(.04)
        self.reader.close()
        self.connection.close()
        self.connection = socket.socket(socket.AF_UNIX)
        self.connection.settimeout(3)
        self.connection.connect(str(self.work / "control.sock"))
        self.reader = self.connection.makefile("rb")
        self.sequence = 0
        self.call("hello", {"client":"reconnected"})
        deadline = time.monotonic() + 2
        while time.monotonic() < deadline:
            state = self.call("status")["result"]
            if state["paused"]: break
            time.sleep(.02)
        self.assertTrue(state["paused"], state)
        self.assertEqual(self.call("advance", {"frames":1})["result"]["advanced"], 1)

    def test_bounded_audio_capture_and_errors(self):
        self.start()
        self.call("pause")
        path = self.work / "audio.wav"
        self.assertEqual(self.call("audio_stop", ok=False)["error"]["code"], "invalid_state")
        self.assertEqual(self.call("audio_start", {"path": str(self.work), "max_seconds":1}, ok=False)["error"]["code"], "io_error")
        self.call("audio_start", {"path":str(path), "max_seconds":1})
        self.assertEqual(self.call("audio_start", {"path":str(path), "max_seconds":1}, ok=False)["error"]["code"], "busy")
        self.call("advance", {"frames":120})
        recorded = self.call("audio_stop")["result"]
        self.assertTrue(recorded["limited"])
        with wave.open(str(path)) as audio:
            self.assertEqual(audio.getnframes(), recorded["samples"])
            self.assertEqual(audio.getnframes(), recorded["sample_rate"])
            self.assertEqual(audio.getnchannels(), 2)
            self.assertEqual(audio.getsampwidth(), 2)
        self.assertEqual(self.call("audio_stop", ok=False)["error"]["code"], "invalid_state")

    def test_version_mismatch_disconnects(self):
        self.start()
        response = self.raw(b'{"id":2,"version":99,"command":"status","params":{}}')
        self.assertEqual(response["error"]["code"], "unsupported_version")
        self.assertEqual(self.reader.readline(), b"")

    def test_profile_isolation(self):
        first_profile = self.profile
        original = bytes([0x37]) + bytes(32767)
        (first_profile / "save.sram").write_bytes(original)
        self.profile = self.work / "other-profile"
        self.profile.mkdir()
        different = bytes([0x6C]) + bytes(32767)
        (self.profile / "save.sram").write_bytes(different)
        self.start()
        self.call("save_flush")
        self.assertEqual((first_profile / "save.sram").read_bytes(), original)
        self.assertEqual((self.profile / "save.sram").read_bytes(), different)

    def test_speed_keeps_frame_contract_and_requires_pause(self):
        self.start()
        self.assertEqual(self.call("speed", {"percent": 500}, ok=False)["error"]["code"], "invalid_state")
        frame = self.call("pause")["result"]["frame"]
        for value in [0, 24, 1001, -1, "100"]:
            self.assertEqual(self.call("speed", {"percent": value}, ok=False)["error"]["code"], "invalid_params")
        for value in [25, 500, 1000, 100]:
            result = self.call("speed", {"percent": value})["result"]
            self.assertEqual(result, dict(percent=value, frame=frame, paused=True))
            advanced = self.call("advance", {"frames": 6})["result"]
            self.assertEqual(advanced["advanced"], 6)
            self.assertEqual(advanced["frame"], frame + 6)
            frame = advanced["frame"]

    def test_paused_ram_bounds_byte_order_and_no_execution(self):
        self.start()
        self.assertEqual(self.call("ram_read", {"address":0xFF0000,"size":4}, ok=False)["error"]["code"], "invalid_state")
        self.call("pause")
        stopped = self.call("advance", {"frames":6})["result"]["frame"]
        for address, size in [(0xFEFFFF,1),(0xFFFFFF,2),(0xFF0000,0),(0xFF0000,257)]:
            self.assertEqual(self.call("ram_read", {"address":address,"size":size}, ok=False)["error"]["code"], "invalid_params")
        for address, size, value in [(0xFF0000,4,"424d4350"),(0xFF0011,3,"223344")]:
            first = self.call("ram_read", {"address":address,"size":size})["result"]
            second = self.call("ram_read", {"address":address,"size":size})["result"]
            self.assertEqual(first, second)
            self.assertEqual(first["frame"], stopped)
            self.assertEqual(first["hex"], value)

    def reconnect(self):
        self.connection = socket.socket(socket.AF_UNIX)
        self.connection.settimeout(3)
        self.connection.connect(str(self.work / "control.sock"))
        self.reader = self.connection.makefile("rb")
        self.sequence = 0
        self.call("hello", {"client": "test"})

    def legacy(self, line):
        # The control socket serves one client at a time; legacy lines use their own connection.
        self.reader.close()
        self.connection.close()
        legacy = socket.socket(socket.AF_UNIX)
        legacy.connect(str(self.work / "control.sock"))
        legacy.sendall(line.encode() + b"\n")
        time.sleep(.1)
        legacy.close()
        self.reconnect()

    def trace_records(self, path):
        return [json.loads(line) for line in path.read_text().splitlines()]

    def test_trace_records_changes_and_stops_on_condition(self):
        self.start()
        self.call("pause")
        start = self.call("advance", {"frames": 60})["result"]["frame"]  # past fixture boot
        counter = {"address": 0xFF0004, "size": 4}
        path = self.work / "trace.jsonl"
        result = self.call("trace", {"frames": 30, "path": str(path), "watches": [
            counter, {"space": "vdp_reg", "address": 7}, {"space": "cram", "address": 2, "size": 2}]})["result"]
        self.assertEqual((result["advanced"], result["frame"], result["stop"]), (30, start + 30, "frames"))
        records = self.trace_records(path)
        self.assertEqual(result["records"], len(records))
        # The counter changes every frame; first record is the starting state.
        self.assertEqual([r["frame"] for r in records], list(range(start, start + 31)))
        self.assertEqual([r["values"][0] for r in records], list(range(records[0]["values"][0], records[0]["values"][0] + 31)))
        self.assertEqual({tuple(r["values"][1:]) for r in records}, {(1, 0x0EEE)})
        # Only changed frames are written: a constant watch yields one record.
        quiet = self.work / "quiet.jsonl"
        result = self.call("trace", {"frames": 20, "path": str(quiet), "watches": [{"space": "vdp_reg", "address": 7}]})["result"]
        self.assertEqual((result["records"], result["advanced"]), (1, 20))
        # Stop when the fixture reacts to A (backdrop register 7 becomes 2).
        self.legacy("pad 1 down a")
        until = self.work / "until.jsonl"
        result = self.call("trace", {"frames": 600, "path": str(until), "watches": [
            {"space": "vdp_reg", "address": 7}, {"address": 0xFF0008, "size": 2}],
            "until": {"watch": 0, "op": "eq", "value": 2}})["result"]
        self.assertEqual(result["stop"], "until")
        self.assertLess(result["advanced"], 10)
        last = self.trace_records(until)[-1]
        self.assertEqual((last["frame"], last["values"][0]), (result["frame"], 2))
        self.assertEqual(last["values"][1] & 0x40, 0x40)
        changed = self.work / "changed.jsonl"
        self.legacy("pad 1 up a")
        result = self.call("trace", {"frames": 600, "path": str(changed), "watches": [{"address": 0xFF0008, "size": 2}],
                                     "until": {"watch": 0, "op": "changed", "mask": 0x40}})["result"]
        self.assertEqual(result["stop"], "until")
        self.assertEqual(self.trace_records(changed)[-1]["values"][0] & 0x40, 0)
        self.assertEqual(self.call("status")["result"]["frame"], result["frame"])

    def test_trace_validation_interrupt_and_existing_file(self):
        self.start()
        watch = [{"address": 0xFF0004, "size": 4}]
        self.assertEqual(self.call("trace", {"frames": 1, "path": str(self.work / "a.jsonl"), "watches": watch}, ok=False)["error"]["code"], "invalid_state")
        self.call("pause")
        bad = [
            {"frames": 0, "path": str(self.work / "b.jsonl"), "watches": watch},
            {"frames": 36001, "path": str(self.work / "b.jsonl"), "watches": watch},
            {"frames": 1, "path": "relative.jsonl", "watches": watch},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": []},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": watch * 17},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": [{"address": 0xFEFFFF}]},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": [{"space": "cram", "address": 1, "size": 2}]},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": [{"space": "vdp_reg", "address": 24}]},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": [{"space": "rom", "address": 0}]},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": [{"address": 0xFF0000, "size": 3}]},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": watch, "until": {"watch": 1, "op": "eq", "value": 0}},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": watch, "until": {"watch": 0, "op": "gt", "value": 0}},
            {"frames": 1, "path": str(self.work / "b.jsonl"), "watches": watch, "until": {"watch": 0, "op": "eq"}},
        ]
        for params in bad:
            self.assertEqual(self.call("trace", params, ok=False)["error"]["code"], "invalid_params", params)
        self.assertFalse((self.work / "b.jsonl").exists())
        existing = self.work / "existing.jsonl"
        existing.write_text("keep\n")
        self.assertEqual(self.call("trace", {"frames": 1, "path": str(existing), "watches": watch}, ok=False)["error"]["code"], "io_error")
        self.assertEqual(existing.read_text(), "keep\n")
        self.sequence += 1
        trace_id = self.sequence
        path = self.work / "long.jsonl"
        self.connection.sendall(json.dumps(dict(id=trace_id, version=2, command="trace", params={
            "frames": 36000, "path": str(path), "watches": watch})).encode() + b"\n")
        self.assertEqual(self.call("advance", {"frames": 1}, ok=False)["error"]["code"], "busy")
        time.sleep(.1)
        self.call("interrupt")
        done = json.loads(self.reader.readline())
        self.assertEqual(done["id"], trace_id)
        self.assertEqual((done["result"]["stop"], done["result"]["interrupted"]), ("interrupted", True))
        self.assertEqual(done["result"]["records"], len(self.trace_records(path)))
        self.assertEqual(self.call("advance", {"frames": 1})["result"]["advanced"], 1)

    def test_vdp_dump_while_stopped(self):
        self.start()
        path = self.work / "vdp.kitvdmp"
        self.assertEqual(self.call("vdp_dump", {"path": str(path)}, ok=False)["error"]["code"], "invalid_state")
        frame = self.call("pause")["result"]["frame"]
        for params in ({"path": "relative"}, {}, {"path": str(path), "x": 1}):
            self.assertEqual(self.call("vdp_dump", params, ok=False)["error"]["code"], "invalid_params")
        result = self.call("vdp_dump", {"path": str(path)})["result"]
        self.assertEqual(result, {"frame": frame, "paused": True, "format": "KITVDMP1"})
        data = path.read_bytes()
        self.assertEqual(data[:8], b"KITVDMP1")
        self.assertEqual(len(data), 24 + 65536 + 64 * 2 + 64 * 2 + 24)
        regs = data[-24:]
        self.assertEqual((regs[2], regs[7], regs[12]), (0x38, 0x01, 0x81))
        self.assertEqual(self.call("vdp_dump", {"path": str(path)}, ok=False)["error"]["code"], "io_error")
        self.assertEqual(self.call("status")["result"]["frame"], frame)

    def test_line_trace_records_raster_writes_and_interrupts(self):
        self.start()
        self.call("pause")
        self.call("advance", {"frames": 30})
        quiet = self.work / "quiet-lines.jsonl"
        result = self.call("line_trace", {"frames": 2, "path": str(quiet)})["result"]
        events = self.trace_records(quiet)
        self.assertEqual((result["advanced"], result["events"]), (2, len(events)))
        # Without C the fixture only rewrites the backdrop, interrupt, and width registers once per frame.
        self.assertEqual({(e["kind"], e.get("reg"), e.get("value")) for e in events}, {("reg", 7, 1), ("reg", 0, 4), ("reg", 12, 0x81)})
        self.legacy("pad 1 down c")
        self.call("advance", {"frames": 2})
        raster = self.work / "raster.jsonl"
        result = self.call("line_trace", {"frames": 1, "path": str(raster)})["result"]
        events = self.trace_records(raster)
        self.assertEqual(result["events"], len(events))
        ints = [e for e in events if e["kind"] == "int"]
        rtes = [e for e in events if e["kind"] == "rte"]
        cram = [e for e in events if e["kind"] == "cram"]
        self.assertGreaterEqual(len([e for e in ints if e["level"] == 4]), 200)
        lines = [e["line"] for e in ints if e["level"] == 4]
        self.assertEqual(lines, sorted(lines))
        self.assertEqual(len(cram), len([e for e in ints if e["level"] == 4]))
        self.assertTrue(all(e["address"] == 0 for e in cram))
        # Each CRAM write lands on the line whose interrupt produced it.
        self.assertEqual([e["line"] for e in cram], lines)
        handled = [e for e in rtes if e["level"] == 4]
        self.assertEqual(len(handled), len(lines))
        self.assertTrue(all(10 < e["cycles"] < 200 for e in handled), handled[:3])
        filtered = self.work / "only-cram.jsonl"
        self.call("line_trace", {"frames": 1, "path": str(filtered), "kinds": ["cram"]})
        self.assertEqual({e["kind"] for e in self.trace_records(filtered)}, {"cram"})
        for params in ({"frames": 0, "path": str(self.work / "x")}, {"frames": 601, "path": str(self.work / "x")},
                       {"frames": 1, "path": "x"}, {"frames": 1, "path": str(self.work / "x"), "kinds": ["dma"]},
                       {"frames": 1, "path": str(self.work / "x"), "kinds": []}):
            self.assertEqual(self.call("line_trace", params, ok=False)["error"]["code"], "invalid_params", params)
        self.assertEqual(self.call("line_trace", {"frames": 1, "path": str(raster)}, ok=False)["error"]["code"], "io_error")

    def test_z80_state_ram_and_frame_cycles(self):
        self.start()
        self.assertEqual(self.call("z80_state", ok=False)["error"]["code"], "invalid_state")
        self.call("pause")
        self.call("advance", {"frames": 30})
        program = self.call("z80_read", {"address": 0, "size": 6})["result"]
        self.assertEqual(program["hex"], "2100103418fd")
        first = int(self.call("z80_read", {"address": 0x1000, "size": 1})["result"]["hex"], 16)
        state = self.call("z80_state")["result"]
        self.assertEqual((state["reset"], state["busreq"], state["busack"]), (False, False, False))
        self.assertNotIn("pc", state)
        self.assertEqual(state["hl"], 0x1000)
        # NTSC: about 59,700 Z80 cycles per frame, all spent running the loop.
        self.assertTrue(59000 < state["last_frame"]["run"] < 60500, state)
        self.assertEqual((state["last_frame"]["busack"], state["last_frame"]["reset"]), (0, 0))
        self.call("advance", {"frames": 1})
        self.assertNotEqual(int(self.call("z80_read", {"address": 0x1000, "size": 1})["result"]["hex"], 16), first)
        for params in ({"address": 0x2000, "size": 1}, {"address": 0x1FFF, "size": 2}, {"address": 0, "size": 0}, {"address": 0, "size": 257}):
            self.assertEqual(self.call("z80_read", params, ok=False)["error"]["code"], "invalid_params")
        # Holding B makes the 68000 request the Z80 bus: the loop stops for whole frames.
        self.legacy("pad 1 down b")
        path = self.work / "z80.jsonl"
        self.call("trace", {"frames": 4, "path": str(path), "watches": [
            {"space": "z80_frame", "address": 0, "size": 4}, {"space": "z80_frame", "address": 1, "size": 4},
            {"space": "z80_ram", "address": 0x1000}]})
        last = self.trace_records(path)[-1]["values"]
        self.assertLess(last[0], 1000)
        self.assertGreater(last[1], 58000)
        stopped = self.call("z80_state")["result"]
        self.assertTrue(stopped["busreq"] and stopped["busack"])
        held = self.call("z80_read", {"address": 0x1000, "size": 1})["result"]["hex"]
        self.call("advance", {"frames": 2})
        self.assertEqual(self.call("z80_read", {"address": 0x1000, "size": 1})["result"]["hex"], held)
        self.assertEqual(self.call("trace", {"frames": 1, "path": str(self.work / "bad.jsonl"), "watches": [
            {"space": "z80_frame", "address": 3, "size": 4}]}, ok=False)["error"]["code"], "invalid_params")

    def run_trace(self, name, frames=60):
        path = self.work / f"{name}.jsonl"
        self.call("trace", {"frames": frames, "path": str(path), "watches": [
            {"address": 0xFF0004, "size": 4}, {"address": 0xFF0008, "size": 2}, {"space": "z80_ram", "address": 0x1000},
            {"space": "vdp_reg", "address": 7}, {"space": "cram", "address": 0}]})
        return self.trace_records(path)

    def test_state_round_trip_is_deterministic(self):
        self.start()
        self.call("pause")
        self.call("advance", {"frames": 40})
        status = self.call("status")["result"]
        self.assertEqual(len(status["rom_sha1"]), 40)
        saved = self.call("save_state", {"name": "before-a"})["result"]
        self.assertGreater(saved["bytes"], 65536)
        self.assertTrue((self.work / "states/before-a.state").exists())
        self.assertEqual(list((self.work / "states").glob("*.tmp.*")), [])
        start = saved["frame"]
        # Same input after the same state must give identical frame-by-frame values.
        self.legacy("pad 1 down a")
        first = self.run_trace("first")
        self.legacy("pad 1 up a")
        self.call("advance", {"frames": 30})
        loaded = self.call("load_state", {"name": "before-a"})["result"]
        self.assertEqual((loaded["frame"], loaded["saved_frame"]), (start, start))
        self.legacy("pad 1 down a")
        second = self.run_trace("second")
        self.legacy("pad 1 up a")
        self.assertEqual(first, second)
        self.assertEqual(first[0]["frame"], start)
        self.assertTrue(any(r["values"][3] == 2 for r in first))  # A changed the backdrop
        for name in ("", ".", "..", "-x", "a/b", "x" * 65, "a b"):
            self.assertEqual(self.call("save_state", {"name": name}, ok=False)["error"]["code"], "invalid_params", name)
        self.assertEqual(self.call("load_state", {"name": "missing"}, ok=False)["error"]["code"], "not_found")
        corrupt = self.work / "states/corrupt.state"
        data = bytearray((self.work / "states/before-a.state").read_bytes())
        data[-1] ^= 0xFF
        corrupt.write_bytes(data)
        self.assertEqual(self.call("load_state", {"name": "corrupt"}, ok=False)["error"]["code"], "io_error")
        other = bytearray((self.work / "states/before-a.state").read_bytes())
        other[12] ^= 0xFF  # ROM digest of a different cartridge
        (self.work / "states/other.state").write_bytes(other)
        self.assertEqual(self.call("load_state", {"name": "other"}, ok=False)["error"]["code"], "invalid_state")
        self.assertEqual(self.call("status")["result"]["frame"], self.call("status")["result"]["frame"])

    def test_soft_reset_is_deterministic(self):
        self.start()
        self.call("pause")
        self.call("advance", {"frames": 40})
        self.call("save_state", {"name": "base"})
        results = []
        for attempt in range(2):
            self.call("load_state", {"name": "base"})
            result = self.call("reset")["result"]
            self.assertTrue(result["scheduled"])
            results.append(self.run_trace(f"reset-{attempt}", 30))
        self.assertEqual(results[0], results[1])
        # The fixture clears its frame counter at reset and counts again from zero.
        counters = [r["values"][0] for r in results[0]]
        self.assertLess(min(counters[1:]), 30)
        self.assertEqual(self.call("reset", {"kind": "hard"}, ok=False)["error"]["code"], "unsupported")
        self.assertEqual(self.call("reset", {"kind": "x"}, ok=False)["error"]["code"], "invalid_params")


if __name__ == "__main__":
    unittest.main()
