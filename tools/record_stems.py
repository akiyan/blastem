#!/usr/bin/env python3
"""Record per-source audio stems with the headless recorder and summarize levels per frame.

Each stem is a separate deterministic run of `blastem --record` with every other
channel muted, so the stems share one timeline. Outputs (PREFIX = the given prefix):

  PREFIX.wav / PREFIX.json / PREFIX.sound.jsonl   full mix, metadata, register-write log
  PREFIX.<stem>.wav / PREFIX.<stem>.json            one run per stem
  PREFIX.levels.jsonl   per frame: [AC RMS, peak, DC mean] (16-bit full scale) for the mix and each stem
  PREFIX.stems.json     totals, active frame ranges, and the mix-vs-sum residual
"""
import argparse
import array
import json
import math
from pathlib import Path
import subprocess
import sys
import wave

try:
    import numpy as np
except ImportError:  # optional: speeds up long recordings
    np = None

GROUPS = {'fm': 'fm1,fm2,fm3,fm4,fm5,fm6', 'dac': 'dac', 'psg': 'psg1,psg2,psg3,noise'}
CHANNELS = ['fm1', 'fm2', 'fm3', 'fm4', 'fm5', 'fm6', 'dac', 'psg1', 'psg2', 'psg3', 'noise']
ALL = 'fm1,fm2,fm3,fm4,fm5,fm6,dac,psg1,psg2,psg3,noise'


def run(core, rom, prefix, frames, inputs, mute=None, log=False):
    command = [str(core), '--record', str(prefix), str(frames), '--record-no-video']
    if inputs:
        command += ['--record-input', str(inputs)]
    if mute:
        command += ['--record-mute', mute]
    if log:
        command.append('--record-sound-log')
    subprocess.run(command + [str(rom)], check=True, stdout=subprocess.DEVNULL)
    meta = json.loads(Path(str(prefix) + '.json').read_text())
    if not meta.get('complete'):
        raise RuntimeError(f'recording did not complete: {prefix}')
    return meta


def samples(path):
    with wave.open(str(path)) as w:
        if (w.getnchannels(), w.getsampwidth()) != (2, 2):
            raise ValueError(f'{path}: expected 16-bit stereo')
        data = array.array('h', w.readframes(w.getnframes()))
    if sys.byteorder != 'little':
        data.byteswap()
    return data


def frame_levels(data, bounds):
    """(AC RMS, peak |sample|, DC mean) per frame over both channels.

    The YM2612 output carries a constant offset, so activity is judged on the AC part.
    """
    if np is not None:
        values = np.frombuffer(data, dtype='<i2').astype(np.float64)
        out = []
        for start, end in bounds:
            part = values[start * 2:end * 2]
            if not part.size:
                out.append((0.0, 0, 0.0))
                continue
            mean = part.mean()
            out.append((float(np.sqrt(((part - mean) ** 2).mean())), int(np.abs(part).max()), float(mean)))
        return out
    out = []
    for start, end in bounds:
        part = data[start * 2:end * 2]
        if not part:
            out.append((0.0, 0, 0.0))
            continue
        mean = sum(part) / len(part)
        out.append((math.sqrt(max(sum(v * v for v in part) / len(part) - mean * mean, 0.0)),
                    max(abs(min(part)), abs(max(part))), mean))
    return out


def summarize(levels, threshold):
    active = [i for i, (rms, _, _) in enumerate(levels) if rms >= threshold]
    total = math.sqrt(sum(rms * rms for rms, _, _ in levels) / max(len(levels), 1))
    return {'ac_rms': round(total, 2), 'ac_rms_dbfs': round(20 * math.log10(total / 32768), 2) if total else None,
            'peak': max((p for _, p, _ in levels), default=0), 'active_frames': len(active),
            'first_active_frame': active[0] + 1 if active else None, 'last_active_frame': active[-1] + 1 if active else None}


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('rom', type=Path)
    parser.add_argument('prefix', type=Path)
    parser.add_argument('frames', type=int)
    parser.add_argument('--core', type=Path, default=Path(__file__).resolve().parents[1] / 'blastem')
    parser.add_argument('--record-input', type=Path)
    parser.add_argument('--per-channel', action='store_true', help='one stem per channel instead of fm/dac/psg groups')
    parser.add_argument('--threshold', type=float, default=1.0, help='AC RMS that counts a frame as active (default 1.0)')
    args = parser.parse_args()
    if args.frames < 1:
        parser.error('frames must be positive')
    stems = {name: name for name in CHANNELS} if args.per_channel else GROUPS
    meta = run(args.core, args.rom, args.prefix, args.frames, args.record_input, log=True)
    per_frame = 44100 * meta['fps_den'] / meta['fps_num']
    count = meta['frames']
    bounds = [(round(i * per_frame), round((i + 1) * per_frame)) for i in range(count)]
    mix = samples(str(args.prefix) + '.wav')
    levels = {'mix': frame_levels(mix, bounds)}
    residual = array.array('l', mix)
    for name, keep in stems.items():
        kept = set(keep.split(','))
        mute = ','.join(c for c in ALL.split(',') if c not in kept)
        stem_prefix = Path(f'{args.prefix}.{name}')
        run(args.core, args.rom, stem_prefix, args.frames, args.record_input, mute=mute)
        data = samples(str(stem_prefix) + '.wav')
        if len(data) != len(mix):
            raise RuntimeError(f'stem {name} has {len(data)} values, mix has {len(mix)}')
        levels[name] = frame_levels(data, bounds)
        for i, v in enumerate(data):
            residual[i] -= v
    worst = max((abs(v) for v in residual), default=0)
    residual_rms = math.sqrt(sum(v * v for v in residual) / max(len(residual), 1))
    with open(str(args.prefix) + '.levels.jsonl', 'x') as out:
        for i in range(count):
            out.write(json.dumps({'frame': i + 1, **{k: [round(v[i][0], 2), v[i][1], round(v[i][2], 1)] for k, v in levels.items()}}) + '\n')
    report = {'frames': count, 'stems': list(stems), 'sample_rate': 44100,
              'levels': {k: summarize(v, args.threshold) for k, v in levels.items()},
              'mix_minus_stems': {'max_abs': worst, 'rms': round(residual_rms, 3)},
              'sound_log': str(args.prefix) + '.sound.jsonl'}
    Path(str(args.prefix) + '.stems.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
