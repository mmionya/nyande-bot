"""Regenerate sticker fixtures with Pillow and FFmpeg (not needed to run Go tests)."""
import gzip
import json
import subprocess
from pathlib import Path
from PIL import Image, ImageDraw

root = Path(__file__).parent
image = Image.new('RGBA', (160, 64), (240, 20, 30, 255))
ImageDraw.Draw(image).text((35, 25), 'HELLO', fill='white')
image.save(root / 'sticker.webp', lossless=True)
animation = {
 'v': '5.5.7', 'fr': 30, 'ip': 0, 'op': 30, 'w': 96, 'h': 96,
 'layers': [{'ty': 1, 'ind': 1, 'ip': 0, 'op': 30, 'sw': 96, 'sh': 96, 'sc': '#f0141e',
  'ks': {'o': {'a': 0, 'k': 100}, 'r': {'a': 0, 'k': 0},
         'p': {'a': 0, 'k': [0, 0, 0]}, 'a': {'a': 0, 'k': [0, 0, 0]}, 's': {'a': 0, 'k': [100, 100, 100]}}}],
}
(root / 'sticker.tgs').write_bytes(gzip.compress(json.dumps(animation).encode(), mtime=0))
subprocess.run(['ffmpeg', '-hide_banner', '-loglevel', 'error', '-f', 'lavfi', '-i',
 'color=c=red:s=96x96:r=8:d=1', '-an', '-c:v', 'libvpx-vp9', '-threads', '1', '-y', str(root / 'sticker.webm')], check=True)
