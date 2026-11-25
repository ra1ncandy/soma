#!/usr/bin/env python3
"""Lightweight MCP stdio server for soma's isolated Docker desktop.

Provides hybrid perception & action over Xvfb + AT-SPI2 + xdotool + scrot + Pillow:
- Tier 1: desktop_a11y_tree (interactive-only @e1/@e2 refs with exact bounding boxes)
- Tier 2: desktop_screenshot (optional Set-of-Mark boxes, downscaled JPEG/PNG) + desktop_crop
- Actions: desktop_click (by @ref or x,y), desktop_type, desktop_key, desktop_drag,
           desktop_scroll, desktop_clipboard_get/set, desktop_launch_app, desktop_list_windows.
"""
import base64
import io
import json
import os
import subprocess
import sys
import tempfile
import time

from PIL import Image, ImageDraw

ENV = dict(os.environ)
ENV.setdefault("DISPLAY", ":99")

REF_MAP: dict[str, tuple[int, int, int, int]] = {}

INTERACTIVE_ROLES = {
    "push button", "toggle button", "check box", "radio button", "menu item",
    "text", "password text", "entry", "combo box", "link", "page tab",
    "list item", "tree item", "spin button", "slider", "document web",
}


def collect_a11y_elements(max_nodes: int = 80) -> list[dict]:
    """Walk Linux AT-SPI2 tree for interactive elements with screen bounding boxes."""
    REF_MAP.clear()
    items: list[dict] = []
    try:
        import pyatspi  # type: ignore

        desktop = pyatspi.Registry.getDesktop(0)

        def walk(node, depth: int = 0):
            if len(items) >= max_nodes or depth > 14 or node is None:
                return
            try:
                role = (node.getRoleName() or "").lower()
                name = (node.name or "").strip()
                if role in INTERACTIVE_ROLES and name:
                    comp = node.queryComponent()
                    ext = comp.getExtents(pyatspi.DESKTOP_COORDS)
                    if ext.width > 2 and ext.height > 2 and ext.x >= 0 and ext.y >= 0:
                        ref = f"e{len(items) + 1}"
                        REF_MAP[ref] = (ext.x, ext.y, ext.width, ext.height)
                        items.append({
                            "ref": ref,
                            "role": role,
                            "name": name[:80],
                            "rect": [ext.x, ext.y, ext.width, ext.height],
                        })
            except Exception:
                pass
            try:
                for i in range(node.childCount):
                    if len(items) >= max_nodes:
                        break
                    walk(node.getChildAtIndex(i), depth + 1)
            except Exception:
                pass

        for i in range(desktop.childCount):
            walk(desktop.getChildAtIndex(i), 0)
    except Exception:
        pass
    return items


def capture_screen() -> Image.Image:
    with tempfile.NamedTemporaryFile(suffix=".png", delete=False) as tf:
        path = tf.name
    try:
        subprocess.run(["scrot", "-o", path], env=ENV, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        with Image.open(path) as img:
            return img.convert("RGB")
    finally:
        if os.path.exists(path):
            os.unlink(path)


def encode_image(img: Image.Image, max_width: int = 1280) -> tuple[str, str, int, int]:
    if max_width > 0 and img.width > max_width:
        ratio = max_width / float(img.width)
        img = img.resize((max_width, max(1, int(img.height * ratio))), Image.LANCZOS)
    buf = io.BytesIO()
    img.save(buf, format="JPEG", quality=80, optimize=True)
    return base64.b64encode(buf.getvalue()).decode("ascii"), "image/jpeg", img.width, img.height


TOOLS = [
    {
        "name": "desktop_a11y_tree",
        "description": "Return compact interactive-only AT-SPI2 UI elements with @e1/@e2 refs and screen bounding boxes.",
        "inputSchema": {
            "type": "object",
            "properties": {"max_nodes": {"type": "integer", "description": "Max elements (default 80)"}},
        },
    },
    {
        "name": "desktop_screenshot",
        "description": "Capture the isolated desktop screen as a compressed image (optionally with Set-of-Mark @eN boxes).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "som": {"type": "boolean", "description": "Overlay numbered Set-of-Mark boxes from AT-SPI2"},
                "max_width": {"type": "integer", "description": "Max image width in pixels (default 1280)"},
            },
        },
    },
    {
        "name": "desktop_crop",
        "description": "Capture a native-resolution zoomed crop (x, y, width, height) of the isolated desktop.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "x": {"type": "integer"}, "y": {"type": "integer"},
                "width": {"type": "integer"}, "height": {"type": "integer"},
            },
            "required": ["x", "y", "width", "height"],
        },
    },
    {
        "name": "desktop_click",
        "description": "Click on the isolated desktop by @ref (e.g. 'e4') or (x, y) coordinates.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "ref": {"type": "string"}, "x": {"type": "integer"}, "y": {"type": "integer"},
                "button": {"type": "integer", "description": "1=left, 2=middle, 3=right"},
                "double": {"type": "boolean"},
            },
        },
    },
    {
        "name": "desktop_type",
        "description": "Type text into the focused window on the isolated desktop.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "text": {"type": "string"},
                "enter": {"type": "boolean", "description": "Press Return after typing"},
            },
            "required": ["text"],
        },
    },
    {
        "name": "desktop_key",
        "description": "Send a key or shortcut (e.g. 'Return', 'ctrl+l', 'alt+Tab', 'Escape').",
        "inputSchema": {
            "type": "object",
            "properties": {"keys": {"type": "string"}},
            "required": ["keys"],
        },
    },
    {
        "name": "desktop_scroll",
        "description": "Scroll at (x, y); positive clicks scroll up, negative scroll down.",
        "inputSchema": {
            "type": "object",
            "properties": {"x": {"type": "integer"}, "y": {"type": "integer"}, "clicks": {"type": "integer"}},
            "required": ["clicks"],
        },
    },
    {
        "name": "desktop_launch_app",
        "description": "Launch a GUI application inside the isolated desktop (e.g. 'chromium --no-sandbox https://example.com').",
        "inputSchema": {
            "type": "object",
            "properties": {"command": {"type": "string"}},
            "required": ["command"],
        },
    },
    {
        "name": "desktop_open_url",
        "description": "Open a URL in Chromium inside the isolated desktop (adds https:// if missing).",
        "inputSchema": {
            "type": "object",
            "properties": {"url": {"type": "string"}},
            "required": ["url"],
        },
    },
    {
        "name": "desktop_list_windows",
        "description": "List open windows inside the isolated desktop.",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "desktop_wait",
        "description": "Wait for N seconds (1-10) for a page or app to finish loading.",
        "inputSchema": {
            "type": "object",
            "properties": {"seconds": {"type": "number"}},
            "required": ["seconds"],
        },
    },
    {
        "name": "desktop_exec",
        "description": "Run a shell command inside the isolated desktop container (supports sudo apt-get, pip, npm, git, etc.).",
        "inputSchema": {
            "type": "object",
            "properties": {
                "command": {"type": "string"},
                "timeout_sec": {"type": "integer"},
            },
            "required": ["command"],
        },
    },
]


def handle_tool(name: str, args: dict) -> dict:
    if name == "desktop_a11y_tree":
        elems = collect_a11y_elements(int(args.get("max_nodes") or 80))
        if not elems:
            return {"content": [{"type": "text", "text": "a11y_tree: 0 interactive elements (use desktop_screenshot for visual grounding)"}]}
        lines = [f"@{e['ref']} [{e['role']}] {e['name']!r} rect={e['rect']}" for e in elems]
        return {"content": [{"type": "text", "text": "\n".join(lines)}]}

    if name == "desktop_screenshot":
        img = capture_screen()
        som = bool(args.get("som", False))
        elems = []
        if som:
            elems = collect_a11y_elements(60)
            draw = ImageDraw.Draw(img)
            for e in elems:
                x, y, w, h = e["rect"]
                draw.rectangle([x, y, x + w, y + h], outline="red", width=2)
                draw.rectangle([x, max(0, y - 14), x + 36, y], fill="red")
                draw.text((x + 2, max(0, y - 13)), e["ref"], fill="white")
        b64, mime, w, h = encode_image(img, int(args.get("max_width") or 1280))
        summary = f"desktop_screenshot {w}x{h} som={som} refs={len(elems)}"
        return {
            "content": [
                {"type": "text", "text": summary},
                {"type": "image", "data": b64, "mimeType": mime},
            ]
        }

    if name == "desktop_crop":
        img = capture_screen()
        x, y, w, h = int(args["x"]), int(args["y"]), int(args["width"]), int(args["height"])
        crop = img.crop((max(0, x), max(0, y), min(img.width, x + w), min(img.height, y + h)))
        b64, mime, cw, ch = encode_image(crop, max_width=0)
        return {
            "content": [
                {"type": "text", "text": f"desktop_crop ({x},{y},{cw}x{ch})"},
                {"type": "image", "data": b64, "mimeType": mime},
            ]
        }

    if name == "desktop_click":
        ref = (args.get("ref") or "").lstrip("@").strip()
        if ref:
            if ref not in REF_MAP:
                collect_a11y_elements(80)
            if ref not in REF_MAP:
                return {"isError": True, "content": [{"type": "text", "text": f"unknown ref @{ref}"}]}
            rx, ry, rw, rh = REF_MAP[ref]
            x, y = rx + rw // 2, ry + rh // 2
        else:
            x, y = int(args["x"]), int(args["y"])
        btn = str(int(args.get("button") or 1))
        cmd = ["xdotool", "mousemove", "--sync", str(x), str(y), "click"]
        if args.get("double"):
            cmd += ["--repeat", "2", "--delay", "80"]
        cmd.append(btn)
        subprocess.run(cmd, env=ENV, check=True)
        return {"content": [{"type": "text", "text": f"clicked ({x}, {y}) button={btn}"}]}

    if name == "desktop_type":
        text = str(args["text"])
        subprocess.run(["xdotool", "type", "--clearmodifiers", "--delay", "12", "--", text], env=ENV, check=True)
        if args.get("enter"):
            subprocess.run(["xdotool", "key", "Return"], env=ENV, check=True)
        return {"content": [{"type": "text", "text": f"typed {len(text)} chars"}]}

    if name == "desktop_key":
        keys = str(args["keys"])
        subprocess.run(["xdotool", "key", "--clearmodifiers", keys], env=ENV, check=True)
        return {"content": [{"type": "text", "text": f"sent key {keys}"}]}

    if name == "desktop_scroll":
        if "seconds" in args and "clicks" not in args and "amount" not in args and "delta" not in args:
            secs = max(0.2, min(10.0, float(args.get("seconds") or 1.0)))
            time.sleep(secs)
            return {"content": [{"type": "text", "text": f"waited {secs:.1f}s"}]}
        raw_clicks = args.get("clicks", args.get("amount", args.get("delta", -3)))
        clicks = int(raw_clicks)
        if str(args.get("direction") or "").lower() == "down" and clicks > 0:
            clicks = -clicks
        elif str(args.get("direction") or "").lower() == "up" and clicks < 0:
            clicks = -clicks
        if "x" in args and "y" in args:
            subprocess.run(["xdotool", "mousemove", "--sync", str(int(args["x"])), str(int(args["y"]))], env=ENV, check=True)
        btn = "4" if clicks > 0 else "5"
        subprocess.run(["xdotool", "click", "--repeat", str(abs(clicks)), btn], env=ENV, check=True)
        return {"content": [{"type": "text", "text": f"scrolled {clicks}"}]}

    if name == "desktop_launch_app":
        cmd = str(args["command"]).strip()
        subprocess.Popen(["/bin/sh", "-c", cmd], env=ENV, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        return {"content": [{"type": "text", "text": f"launched: {cmd}"}]}

    if name == "desktop_open_url":
        url = str(args.get("url") or "").strip()
        if not url:
            return {"isError": True, "content": [{"type": "text", "text": "desktop_open_url requires non-empty url"}]}
        if not url.startswith(("http://", "https://", "file://")):
            url = "https://" + url
        # Pass the URL as its own argv entry, never through a shell: inside double quotes a shell
        # still expands $(...) and backticks, so a crafted URL would run commands.
        subprocess.Popen(["chromium", "--no-sandbox", "--start-maximized", "--", url], env=ENV, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        return {"content": [{"type": "text", "text": f"opened in Chromium: {url}"}]}

    if name == "desktop_list_windows":
        res = subprocess.run(["wmctrl", "-lG"], env=ENV, capture_output=True, text=True)
        out = res.stdout.strip() or "(no windows)"
        return {"content": [{"type": "text", "text": out}]}

    if name == "desktop_wait":
        secs = max(0.2, min(10.0, float(args.get("seconds") or 1.0)))
        time.sleep(secs)
        return {"content": [{"type": "text", "text": f"waited {secs:.1f}s"}]}

    if name == "desktop_exec":
        cmd = str(args.get("command") or "").strip()
        if not cmd:
            return {"isError": True, "content": [{"type": "text", "text": "desktop_exec requires non-empty command"}]}
        timeout = max(1, min(180, int(args.get("timeout_sec") or 60)))
        try:
            res = subprocess.run(
                ["/bin/sh", "-c", cmd],
                env=ENV,
                cwd="/workspace",
                capture_output=True,
                text=True,
                timeout=timeout,
            )
            out = (res.stdout or "") + (res.stderr or "")
            if len(out) > 12000:
                out = "...(truncated)\n" + out[-12000:]
            return {"content": [{"type": "text", "text": f"exit_code={res.returncode}\n{out.strip()}"}]}
        except subprocess.TimeoutExpired:
            return {"isError": True, "content": [{"type": "text", "text": f"desktop_exec timed out after {timeout}s"}]}

    return {"isError": True, "content": [{"type": "text", "text": f"unknown tool: {name}"}]}


def main():
    for raw in sys.stdin:
        raw = raw.strip()
        if not raw:
            continue
        try:
            req = json.loads(raw)
        except Exception:
            continue
        req_id = req.get("id")
        method = req.get("method", "")
        params = req.get("params") or {}

        if method == "notifications/initialized":
            continue
        if method == "initialize":
            resp = {
                "jsonrpc": "2.0",
                "id": req_id,
                "result": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": "soma-desktop-mcp", "version": "0.5.0"},
                },
            }
        elif method == "tools/list":
            resp = {"jsonrpc": "2.0", "id": req_id, "result": {"tools": TOOLS}}
        elif method == "tools/call":
            try:
                result = handle_tool(params.get("name", ""), params.get("arguments") or {})
                resp = {"jsonrpc": "2.0", "id": req_id, "result": result}
            except Exception as exc:
                resp = {"jsonrpc": "2.0", "id": req_id, "result": {"isError": True, "content": [{"type": "text", "text": str(exc)}]}}
        else:
            resp = {"jsonrpc": "2.0", "id": req_id, "error": {"code": -32601, "message": f"method not found: {method}"}}

        sys.stdout.write(json.dumps(resp) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
