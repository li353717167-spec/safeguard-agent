#!/usr/bin/env python3
"""把真实命令输出渲染成 README 用的终端 GIF。

输入是 demo/gif/scenes.json（分镜：命令 + 输出行），输出是 docs/assets/demo.gif。

为什么用"渲染"而不是屏幕录制：
- 可复现：同样的输入永远得到同样的 GIF，便于随代码一起更新；
- 干净：没有窗口边框、鼠标、系统通知；
- 体积小：按行打字机 + 有限帧数，控制在 2MB 以内。

渲染细节：
- ASCII 用 Consolas（等宽），中文用微软雅黑，按"ASCII 1 格 / 中文 2 格"排版，
  所以表格与中文混排能对齐；
- 底部常有水印，说明这是脚本模型 + 真实编排（不夸大演示）。
"""
import json
import os
import sys

from PIL import Image, ImageDraw, ImageFont

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SCENES = os.path.join(ROOT, "demo", "gif", "scenes.json")
OUT = os.path.join(ROOT, "docs", "assets", "demo.gif")

# 画布与排版
W = 1000
PAD = 22
CHROME = 46
FONT_SIZE = 16
LINE_H = 24
CELL = 9  # Consolas 16px 的单字符宽度（取整，保证网格对齐）

BG = (13, 17, 23)
CHROME_BG = (22, 27, 34)
DOT = [(255, 95, 86), (255, 189, 46), (39, 201, 63)]
FG = (201, 209, 217)
DIM = (139, 148, 158)
GREEN = (126, 231, 135)
BLUE = (88, 166, 255)
ORANGE = (255, 166, 87)
BOLD = (230, 237, 243)
WATERMARK = (72, 79, 88)


def fonts():
    ascii_font = ImageFont.truetype("C:/Windows/Fonts/consola.ttf", FONT_SIZE)
    cjk_font = ImageFont.truetype("C:/Windows/Fonts/msyh.ttc", FONT_SIZE - 1)
    small = ImageFont.truetype("C:/Windows/Fonts/msyh.ttc", 12)
    title = ImageFont.truetype("C:/Windows/Fonts/msyh.ttc", 13)
    return ascii_font, cjk_font, small, title


# 终端字体没有 emoji 字形，统一换成等价的符号，避免渲染成豆腐块。
SYMBOLS = {"✅": "[OK]", "❌": "[X]", "⚠️": "[!]", "⚠": "[!]", "→": "->"}


def normalize(text):
    for src, dst in SYMBOLS.items():
        text = text.replace(src, dst)
    return text


def cells(text):
    """显示宽度：ASCII 1 格，其余 2 格。"""
    return sum(1 if ord(ch) < 128 else 2 for ch in text)


def wrap(text, max_cells):
    """按显示宽度折行（不切坏中文）。"""
    out, cur, width = [], "", 0
    for ch in text:
        cw = 1 if ord(ch) < 128 else 2
        if width + cw > max_cells and cur:
            out.append(cur)
            cur, width = "", 0
        cur += ch
        width += cw
    out.append(cur)
    return out


def draw_mixed(draw, x, y, text, ascii_font, cjk_font, color):
    cx = x
    for ch in text:
        if ord(ch) < 128:
            draw.text((cx, y), ch, font=ascii_font, fill=color)
            cx += CELL
        else:
            draw.text((cx, y), ch, font=cjk_font, fill=color)
            cx += 2 * CELL


def line_color(line):
    if line.startswith("---"):
        return BOLD
    if any(k in line for k in ("待审批", "需要人工审批", "ticket=", "waiting_approval")):
        return ORANGE
    if any(k in line for k in ("最终回答", "停止原因=answered", "✅", "成功：", "已批准")):
        return GREEN
    if any(k in line for k in ("approval.requested", "tool.executing", "tool.executed", "task.resumed")):
        return BLUE
    if line.startswith("注意：") or "说明：" in line:
        return DIM
    return FG


def render_frame(scene, shown, max_lines, height, f):
    img = Image.new("RGB", (W, height), BG)
    d = ImageDraw.Draw(img)
    ascii_font, cjk_font, small, title = f

    # 标题栏
    d.rectangle([0, 0, W, CHROME], fill=CHROME_BG)
    for i, color in enumerate(DOT):
        d.ellipse([16 + i * 20, 17, 28 + i * 20, 29], fill=color)
    d.text((W // 2 - 150, 15), "safeguard-agent — 运维 Agent 演示", font=title, fill=DIM)

    y = CHROME + 14
    # 命令行
    draw_mixed(d, PAD, y, "$ ", ascii_font, cjk_font, GREEN)
    for i, part in enumerate(wrap(normalize(scene["cmd"]), 104)):
        draw_mixed(d, PAD + 2 * CELL, y + i * LINE_H, part, ascii_font, cjk_font, BOLD)
    y += LINE_H * max(1, len(wrap(scene["cmd"], 104)))

    for line in shown:
        line = normalize(line)
        first = True
        for chunk in wrap(line, 104):
            if y > height - 40:
                break
            # 折行的续行做悬挂缩进并变暗：终端里"一行太长被折了"的观感，而不是表格断了
            x = PAD if first else PAD + 2 * CELL
            color = line_color(line) if first else DIM
            draw_mixed(d, x, y, chunk, ascii_font, cjk_font, color)
            y += LINE_H
            first = False

    # 水印：诚实说明演示环境
    d.text((W - 300, height - 20), "脚本模型 · 真实编排/审批/MySQL", font=small, fill=WATERMARK)
    return img


def main():
    with open(SCENES, encoding="utf-8") as fh:
        scenes = json.load(fh)

    f = fonts()
    max_lines = max(
        sum(len(wrap(l, 104)) for l in s["lines"]) + len(wrap(s["cmd"], 104)) + 2
        for s in scenes
    )
    height = CHROME + 14 + max_lines * LINE_H + 26

    frames, durations = [], []
    for scene in scenes:
        shown = []
        frames.append(render_frame(scene, shown, max_lines, height, f))
        durations.append(600)
        # 按行打字机：每帧 2 行，节奏接近真实输出
        step = 2
        for i in range(0, len(scene["lines"]), step):
            shown = scene["lines"][: i + step]
            frames.append(render_frame(scene, shown, max_lines, height, f))
            durations.append(90)
        frames.append(render_frame(scene, scene["lines"], max_lines, height, f))
        durations.append(scene.get("hold_ms", 1400))

    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    # 用调色板压缩体积：128 色对终端截图足够
    pal = [im.convert("P", palette=Image.ADAPTIVE, colors=128) for im in frames]
    pal[0].save(
        OUT,
        save_all=True,
        append_images=pal[1:],
        duration=durations,
        loop=0,
        optimize=True,
        disposal=2,
    )
    size = os.path.getsize(OUT)
    print(f"wrote {OUT}: {len(pal)} frames, {size/1024/1024:.2f} MB, {W}x{height}")


if __name__ == "__main__":
    sys.exit(main())
