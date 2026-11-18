#!/usr/bin/env python3
"""Mux a completed BlastEM offline capture with exact emulated-frame timestamps."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import wave


def auto_crop(meta):
    """Union of the recorded active display areas, so no frame loses picture."""
    areas = meta.get('active_areas')
    if not areas:
        raise ValueError('Recording has no active_areas; record with a newer core or pass --crop W H X Y')
    left = min(a['x'] for a in areas)
    top = min(a['y'] for a in areas)
    right = max(a['x'] + a['width'] for a in areas)
    bottom = max(a['y'] + a['height'] for a in areas)
    return (right - left, bottom - top, left, top)


def mux(prefix, output, crop=None, preset='veryfast'):
    meta = json.loads(Path(str(prefix) + '.json').read_text())
    if not meta.get('complete'):
        raise ValueError('Capture did not finish successfully')
    crop_filter = ''
    if crop == 'auto':
        crop = auto_crop(meta)
    if crop is not None:
        width, height, x, y = crop
        if min(width, height) <= 0 or min(x, y) < 0 or x + width > meta['width'] or y + height > meta['height']:
            raise ValueError('Crop must lie inside the recorded frame')
        crop_filter = f'crop={width}:{height}:{x}:{y},'
    audio = Path(str(prefix) + '.wav')
    with wave.open(str(audio)) as wav:
        if (wav.getnframes(), wav.getframerate(), wav.getnchannels()) != (meta['audio_samples'], meta['sample_rate'], 2):
            raise ValueError('WAV does not match recording metadata')
    duration = meta['frames'] * meta['fps_den'] / meta['fps_num']
    difference = meta['audio_samples'] / meta['sample_rate'] - duration
    if abs(difference) > .04:
        raise ValueError(f'Audio/video duration mismatch: {difference:.6f}s')
    chunk_frames = meta.get('video_chunk_frames', meta['frames'])
    chunk_count = meta.get('video_chunks', 1)
    if chunk_frames < 1 or chunk_count != (meta['frames'] + chunk_frames - 1) // chunk_frames:
        raise ValueError('Invalid video chunk metadata')
    videos = [Path(str(prefix) + ('.apng' if i == 0 else f'.part{i:04d}.apng')).resolve()
              for i in range(chunk_count)]
    # Verify every frame is decodable before producing a delivery file. This
    # catches truncated APNGs and demuxers that silently stop at a size boundary.
    for i, video in enumerate(videos):
        probe = json.loads(subprocess.check_output(['ffprobe', '-v', 'error', '-count_frames',
            '-select_streams', 'v:0', '-show_entries', 'stream=nb_read_frames', '-of', 'json', str(video)]))
        expected = min(chunk_frames, meta['frames'] - i * chunk_frames)
        if int(probe['streams'][0]['nb_read_frames']) != expected:
            raise ValueError(f'Video frame count mismatch: {video}')
    # APNG delays use 16-bit rationals. Rebuild timestamps from nominal frame
    # indices across all chunks, rather than accumulating rounded delays.
    with tempfile.TemporaryDirectory(prefix='blastem-mux-') as tmp:
        listing = Path(tmp) / 'video.ffconcat'
        def quote(path):
            return "'" + str(path).replace("'", "'\\''") + "'"
        listing.write_text('ffconcat version 1.0\n' + ''.join(
            'file ' + quote(v) + '\nduration ' + format(
                min(chunk_frames, meta['frames'] - i * chunk_frames) * meta['fps_den'] / meta['fps_num'], '.12f') + '\n'
            for i, v in enumerate(videos)))
        subprocess.run(['ffmpeg', '-v', 'warning', '-xerror', '-nostdin', '-n',
            '-f', 'concat', '-safe', '0', '-i', str(listing), '-i', str(audio),
            '-map', '0:v:0', '-map', '1:a:0',
            '-vf', f"setpts=N*{meta['fps_den']}/{meta['fps_num']}/TB,{crop_filter}scale=1280:960:flags=neighbor,setsar=1,fps=30",
            '-c:v', 'libx264', '-preset', preset, '-crf', '18', '-threads', '4',
            '-c:a', 'aac', '-b:a', '192k', '-movflags', '+faststart', str(output)], check=True)
    probe = json.loads(subprocess.check_output(['ffprobe', '-v', 'error',
        '-show_entries', 'stream=codec_type,nb_frames,duration', '-of', 'json', str(output)]))
    video = next(s for s in probe['streams'] if s['codec_type'] == 'video')
    if abs(int(video['nb_frames']) - duration * 30) > 1 or abs(float(video['duration']) - duration) > .04:
        raise ValueError('Encoded video does not cover the complete capture')
    return {'video_seconds': duration, 'audio_minus_video_seconds': difference,
            'frames': meta['frames'], 'crop': crop, 'display_aspect_ratio': '4:3', 'output': str(output)}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('prefix', type=Path)
    parser.add_argument('output', type=Path)
    parser.add_argument('--crop', nargs='+', metavar='VALUE',
                        help='auto (union of the recorded active display areas) or WIDTH HEIGHT X Y '
                             'before 4:3 aspect correction (H32/224 NTSC: 256 224 32 9)')
    parser.add_argument('--preset', choices=('ultrafast', 'veryfast', 'fast'), default='veryfast')
    args = parser.parse_args()
    crop = None
    if args.crop == ['auto']:
        crop = 'auto'
    elif args.crop:
        if len(args.crop) != 4 or not all(v.isdigit() for v in args.crop):
            parser.error('--crop takes auto or four integers: WIDTH HEIGHT X Y')
        crop = tuple(int(v) for v in args.crop)
    print(json.dumps(mux(args.prefix, args.output, crop, args.preset), indent=2))
