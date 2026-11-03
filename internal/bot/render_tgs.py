"""Render a few frames from bounded, validated TGS JSON supplied by stickers.go."""
import sys
from pathlib import Path

from rlottie_python import LottieAnimation

source, destination, count = sys.argv[1:]
animation = LottieAnimation.from_data(Path(source).read_text(encoding="utf-8"))
total = animation.lottie_animation_get_totalframe()
width, height = animation.lottie_animation_get_size()
if not 0 < total <= 600 or not (0 < width <= 1024 and 0 < height <= 1024):
    raise ValueError("Invalid sticker dimensions or frame count")
count = min(int(count), total, 6)
for index in range(count):
    frame = min(total - 1, total * (index + 1) // (count + 1))
    image = animation.render_pillow_frame(frame_num=frame)
    image.save(Path(destination) / f"frame-{index:02}.png")
