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


@unittest.skipUnless(os.environ.get("DISPLAY"), "requires Xvfb and built SRAM fixture")
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
        env = dict(os.environ, SDL_AUDIODRIVER="dummy", BLASTEM_NO_GUI="1",
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


if __name__ == "__main__":
    unittest.main()
