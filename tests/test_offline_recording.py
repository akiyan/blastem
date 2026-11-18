"""End-to-end offline capture using the redistributable controller/tone fixture."""
import array
import hashlib
import json
import os
from pathlib import Path
import statistics
import subprocess
import tempfile
import unittest
import wave
import zlib

ROOT = Path(__file__).resolve().parents[1]
CORE = ROOT / 'blastem'
ROM = ROOT / 'mcp/testdata/fixture/build/blastem-mcp-fixture.bin'


@unittest.skipUnless(CORE.exists() and ROM.exists(), 'build BlastEM and fixture first')
class OfflineCaptureTest(unittest.TestCase):
    def test_headless_sync_inputs_repeatability_and_overwrite_protection(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            events = root / 'inputs.txt'
            events.write_text('120 1 down a\n240 1 up a\n')
            env = dict(os.environ, SDL_VIDEODRIVER='invalid', SDL_AUDIODRIVER='invalid',
                       XDG_DATA_HOME=str(root / 'data'), XDG_CONFIG_HOME=str(root / 'config'),
                       BLASTEM_NO_GUI='1')
            env.pop('DISPLAY', None)
            env.pop('WAYLAND_DISPLAY', None)
            def run(prefix, *args, ok=True, frames=600):
                result = subprocess.run([str(CORE), '--record', str(prefix), str(frames),
                                         '--record-input', str(events), *args, str(ROM)],
                                        env=env, capture_output=True, timeout=30)
                self.assertEqual(result.returncode == 0, ok, result.stderr.decode())
            hashes = []
            for name in ('first', 'repeat', 'pal'):
                prefix = root / name
                run(prefix, *(['-r', 'E'] if name == 'pal' else []))
                meta = json.loads(prefix.with_suffix('.json').read_text())
                self.assertTrue(meta['complete'])
                self.assertEqual(meta['frames'], 600)
                self.assertEqual(meta['width'], 320)
                self.assertEqual(meta['height'], 256 if name == 'pal' else 240)
                video_seconds = meta['frames'] * meta['fps_den'] / meta['fps_num']
                self.assertLess(abs(meta['audio_samples'] / 44100 - video_seconds), .04)
                with wave.open(str(prefix.with_suffix('.wav'))) as wav:
                    self.assertEqual((wav.getnchannels(), wav.getframerate()), (2, 44100))
                    self.assertEqual(wav.getnframes(), meta['audio_samples'])
                    samples = array.array('h', wav.readframes(wav.getnframes()))[::2]
                # A produces the fixture's 437 Hz PSG tone; releasing A silences it.
                def deviation(lo, hi):
                    return statistics.pstdev(samples[int(lo*44100):int(hi*44100)])
                self.assertLess(deviation(.5, 1.5), 1)
                self.assertGreater(deviation(2.6, 3.6), 300)
                self.assertLess(deviation(5, 6), 1)
                png = subprocess.check_output(['ffmpeg', '-v', 'error', '-i', str(prefix.with_suffix('.apng')),
                    '-vf', 'select=eq(n\\,180)', '-frames:v', '1', '-f', 'rawvideo', '-pix_fmt', 'rgb24', '-'])
                self.assertEqual(len(png), 320 * meta['height'] * 3)
                # Bottom backdrop changes to red while A is pressed.
                self.assertEqual(png[-3:], bytes((255, 0, 0)))
                hashes.append(tuple(hashlib.sha256(prefix.with_suffix(s).read_bytes()).hexdigest()
                                    for s in ('.apng', '.wav')))
            self.assertEqual(hashes[0], hashes[1])
            run(root / 'first', ok=False)
            self.assertEqual(hashes[0], tuple(hashlib.sha256((root / 'first').with_suffix(s).read_bytes()).hexdigest()
                                             for s in ('.apng', '.wav')))
            run(root / 'split', frames=1801)
            split = json.loads((root / 'split.json').read_text())
            self.assertEqual(split['video_chunks'], 2)
            self.assertEqual(split['video_chunk_frames'], 1800)
            for suffix, expected in (('.apng', 1800), ('.part0001.apng', 1)):
                path = root / ('split' + suffix)
                data = path.read_bytes(); pos = 8; controls = 0
                while pos < len(data):
                    size = int.from_bytes(data[pos:pos+4], 'big')
                    kind = data[pos+4:pos+8]; payload = data[pos+8:pos+8+size]
                    crc = int.from_bytes(data[pos+8+size:pos+12+size], 'big')
                    self.assertEqual(zlib.crc32(kind+payload), crc, kind)
                    if kind == b'acTL': self.assertEqual(int.from_bytes(payload[:4], 'big'), expected)
                    if kind == b'fcTL': controls += 1
                    pos += size + 12
                self.assertEqual(controls, expected)
                self.assertEqual(pos, len(data))
                count = subprocess.check_output(['ffprobe', '-v', 'error', '-count_frames',
                    '-show_entries', 'stream=nb_read_frames', '-of', 'csv=p=0', str(path)])
                self.assertEqual(int(count.strip()), expected)
            subprocess.run(['python3', str(ROOT / 'tools/mux_capture.py'),
                str(root / 'split'), str(root / 'split.mp4')], check=True, capture_output=True, timeout=60)
            events.write_text('240 1 down a\n120 1 up a\n')
            run(root / 'invalid', ok=False)
            self.assertFalse((root / 'invalid.apng').exists())

    def test_stems_sound_log_and_muting(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            events = root / 'inputs.txt'
            events.write_text('120 1 down a\n240 1 up a\n')
            env = dict(os.environ, SDL_VIDEODRIVER='invalid', SDL_AUDIODRIVER='invalid',
                       XDG_DATA_HOME=str(root / 'data'), XDG_CONFIG_HOME=str(root / 'config'), BLASTEM_NO_GUI='1')
            env.pop('DISPLAY', None)
            env.pop('WAYLAND_DISPLAY', None)
            result = subprocess.run(['python3', str(ROOT / 'tools/record_stems.py'), str(ROM), str(root / 'run'), '360',
                                     '--record-input', str(events), '--core', str(CORE)], env=env, capture_output=True, timeout=120)
            self.assertEqual(result.returncode, 0, result.stderr.decode())
            report = json.loads((root / 'run.stems.json').read_text())
            psg, fm, dac = report['levels']['psg'], report['levels']['fm'], report['levels']['dac']
            # The fixture tone starts in the frame after A is pressed and stops after release.
            self.assertEqual((psg['first_active_frame'], psg['last_active_frame']), (121, 241))
            self.assertGreater(psg['ac_rms'], 100)
            self.assertLess(fm['ac_rms'], 2)
            self.assertEqual(dac['active_frames'], 0)
            self.assertLessEqual(report['mix_minus_stems']['max_abs'], 2)
            levels = [json.loads(line) for line in (root / 'run.levels.jsonl').read_text().splitlines()]
            self.assertEqual(len(levels), 360)
            self.assertEqual(levels[129]['frame'], 130)
            self.assertGreater(levels[129]['psg'][0], 100)
            self.assertEqual(levels[129]['fm'][0], 0)
            log = [json.loads(line) for line in (root / 'run.sound.jsonl').read_text().splitlines()]
            tone = sorted({r['frame'] for r in log if r.get('chip') == 'psg' and r['value'] == 0x94})
            self.assertEqual(tone, list(range(121, 241)))
            self.assertTrue(all(r['frame'] >= 1 and 'cycle' in r for r in log))
            meta = json.loads((root / 'run.psg.json').read_text())
            self.assertEqual((meta['video'], meta['sound_log'], meta['muted']), (False, False, ['fm1', 'fm2', 'fm3', 'fm4', 'fm5', 'fm6', 'dac']))
            self.assertFalse((root / 'run.apng').exists())
            for args in (['--record-mute', 'fm7'], ['--record-mute', '']):
                bad = subprocess.run([str(CORE), '--record', str(root / 'bad'), '10', *args, str(ROM)], env=env, capture_output=True, timeout=30)
                self.assertNotEqual(bad.returncode, 0)
            alone = subprocess.run([str(CORE), '--record-mute', 'fm', str(ROM)], env=env, capture_output=True, timeout=30)
            self.assertNotEqual(alone.returncode, 0)

    def test_active_areas_and_auto_crop(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            events = root / 'inputs.txt'
            events.write_text('100 1 down start\n200 1 up start\n')
            env = dict(os.environ, SDL_VIDEODRIVER='invalid', SDL_AUDIODRIVER='invalid',
                       XDG_DATA_HOME=str(root / 'data'), XDG_CONFIG_HOME=str(root / 'config'), BLASTEM_NO_GUI='1')
            env.pop('DISPLAY', None)
            env.pop('WAYLAND_DISPLAY', None)
            result = subprocess.run([str(CORE), '--record', str(root / 'run'), '300', '--record-input', str(events), str(ROM)],
                                    env=env, capture_output=True, timeout=60)
            self.assertEqual(result.returncode, 0, result.stderr.decode())
            meta = json.loads((root / 'run.json').read_text())
            # Holding Start switches the fixture to H32 from the next frame.
            self.assertEqual(meta['active_areas'], [
                {'from': 1, 'to': 101, 'x': 0, 'y': 9, 'width': 320, 'height': 224},
                {'from': 102, 'to': 201, 'x': 32, 'y': 9, 'width': 256, 'height': 224},
                {'from': 202, 'to': 300, 'x': 0, 'y': 9, 'width': 320, 'height': 224}])
            # The H32 part shows its picture centred with black side borders.
            frame = subprocess.check_output(['ffmpeg', '-v', 'error', '-i', str(root / 'run.apng'), '-vf', 'select=eq(n\\,150)',
                                             '-frames:v', '1', '-f', 'rawvideo', '-pix_fmt', 'rgb24', '-'])
            row = 120 * 320 * 3
            self.assertEqual(frame[row:row + 3], bytes(3))
            self.assertNotEqual(frame[row + 40 * 3:row + 41 * 3], bytes(3))
            out = subprocess.run(['python3', str(ROOT / 'tools/mux_capture.py'), str(root / 'run'), str(root / 'run.mp4'), '--crop', 'auto'],
                                 capture_output=True, timeout=120)
            self.assertEqual(out.returncode, 0, out.stderr.decode())
            self.assertEqual(json.loads(out.stdout)['crop'], [320, 224, 0, 9])
            import importlib.util
            spec = importlib.util.spec_from_file_location('mux_capture', ROOT / 'tools/mux_capture.py')
            mux = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(mux)
            self.assertEqual(mux.auto_crop({'active_areas': [meta['active_areas'][1]]}), (256, 224, 32, 9))
            with self.assertRaises(ValueError):
                mux.auto_crop({})


if __name__ == '__main__':
    unittest.main()
