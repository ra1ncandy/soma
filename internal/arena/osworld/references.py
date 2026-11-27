"""Reference solutions for replicated OSWorld tasks that have no gold file to validate against.

prepare.py uses these only to show that a task's checker can be passed; contestants never see
them. Keys are the first 8 characters of the OSWorld task id. Each function edits the workspace
directory in place, like an agent would.
"""
import os

from PIL import Image, ImageEnhance, ImageOps


def _desktop(ws, name):
    return os.path.join(ws, "Desktop", name)


def palette(ws):
    Image.open(_desktop(ws, "computer.png")).convert("RGB").convert("P", palette=Image.ADAPTIVE).save(_desktop(ws, "palette_computer.png"))


def more_saturation(ws):
    img = Image.open(_desktop(ws, "woman_sitting_by_the_tree2.png")).convert("RGB")
    ImageEnhance.Color(img).enhance(1.6).save(_desktop(ws, "edited_colorful.png"))


def mirror(ws):
    ImageOps.mirror(Image.open(_desktop(ws, "berry.png"))).save(_desktop(ws, "berry_mirror.png"))


def darker(ws):
    img = Image.open(_desktop(ws, "woman_sitting_by_the_tree.png")).convert("RGB")
    ImageEnhance.Brightness(img).enhance(0.7).save(_desktop(ws, "edited_darker.png"))


def more_contrast(ws):
    img = Image.open(_desktop(ws, "berries.png")).convert("RGB")
    ImageEnhance.Contrast(img).enhance(1.5).save(_desktop(ws, "berries_contrast.png"))


def green_background(ws):
    img = Image.open(_desktop(ws, "white_background_with_object.png")).convert("RGBA")
    px = img.load()
    for y in range(img.height):
        for x in range(img.width):
            r, g, b, a = px[x, y]
            if (r, g, b) != (0, 0, 0):  # everything but the pure-black object is background
                px[x, y] = (0, 200, 0, a)
    img.save(_desktop(ws, "green_background_with_object.png"))


def centre_triangle(ws):
    img = Image.open(_desktop(ws, "Triangle_On_The_Side.png")).convert("RGB")
    px = img.load()
    yellow = [(x, y) for y in range(img.height) for x in range(img.width)
              if px[x, y][0] > 180 and px[x, y][1] > 180 and px[x, y][2] < 120]
    xs, ys = [p[0] for p in yellow], [p[1] for p in yellow]
    box = (min(xs), min(ys), max(xs) + 1, max(ys) + 1)
    tri = img.crop(box)
    background = px[0, 0]
    out = img.copy()
    out.paste(background, box)
    out.paste(tri, ((img.width - tri.width) // 2, (img.height - tri.height) // 2))
    out.save(_desktop(ws, "Triangle_In_The_Middle.png"))


REFERENCES = {
    "06ca5602": palette,
    "554785e9": more_saturation,
    "72f83cdc": mirror,
    "7a4deb26": darker,
    "f723c744": more_contrast,
    "734d6579": green_background,
    "f4aec372": centre_triangle,
}
