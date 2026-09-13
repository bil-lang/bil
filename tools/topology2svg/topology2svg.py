#!/usr/bin/env python3
"""Render a bilc `.topology.yaml` placement manifest as an SVG diagram.

The manifest format is the small, fixed schema `PlacementManifest` in
tools/bilc/bilc.go emits (see docs/guide.md, "What bilc does with
placement") -- a `placements:` list of `{match: {...}}` / `proc:` pairs,
an optional `default:` clause, and a `links: aliases:` map of per-proc
link-index names. It is not general YAML (no nesting beyond one flow
map, no comments, no multi-line scalars), so this parses that exact
shape by hand rather than depending on PyYAML.

Grid placements use `row`/`col` (possibly symbolic, e.g. "cols-1",
since bilc never knows the concrete grid size); flat placements use
`id`. Since the manifest never records a concrete grid/id-space size,
--rows/--cols/--ids pick an illustrative size to render.
"""

import argparse
import re
import sys
from pathlib import Path

CELL_W, CELL_H, GAP, MARGIN = 150, 80, 60, 50

# Okabe-Ito colorblind-safe qualitative palette.
PALETTE = ["#E69F00", "#56B4E9", "#009E73", "#F0E442",
           "#0072B2", "#D55E00", "#CC79A7", "#999999"]
DEFAULT_FILL = "#EDEDED"
DEFAULT_STROKE = "#888888"


def parse_scalar(v):
    v = v.strip()
    if v.startswith('"') and v.endswith('"'):
        return v[1:-1]
    try:
        return int(v)
    except ValueError:
        return v


def parse_flow_map(s):
    s = s.strip()
    if not s.startswith("{") or not s.endswith("}"):
        raise ValueError(f"expected a flow map, got: {s!r}")
    inner = s[1:-1].strip()
    out = {}
    if not inner:
        return out
    for part in inner.split(","):
        key, _, val = part.partition(":")
        out[key.strip()] = parse_scalar(val)
    return out


def parse_manifest(text):
    lines = text.splitlines()
    i, n = 0, len(lines)
    placements = []
    has_default, default_proc = False, None
    aliases = {}

    while i < n:
        line = lines[i]
        if not line.strip():
            i += 1
            continue
        if line == "placements:":
            i += 1
            while i < n and lines[i].startswith("  - match: "):
                match = parse_flow_map(lines[i][len("  - match: "):])
                i += 1
                proc = None
                if i < n and lines[i].startswith("    proc: "):
                    proc = lines[i][len("    proc: "):].strip()
                    i += 1
                placements.append({"match": match, "proc": proc})
            continue
        if line == "default:":
            has_default = True
            i += 1
            if i < n and lines[i].startswith("  proc: "):
                default_proc = lines[i][len("  proc: "):].strip()
                i += 1
            continue
        if line == "links:":
            i += 1
            if i < n and lines[i].strip() == "aliases:":
                i += 1
                while i < n and lines[i].startswith("    ") and ":" in lines[i]:
                    name, _, rest = lines[i].strip().partition(":")
                    aliases[name.strip()] = parse_flow_map(rest.strip())
                    i += 1
            continue
        i += 1  # unrecognised line -- ignore rather than fail

    return placements, has_default, default_proc, aliases


def eval_expr(expr, rows, cols):
    """Resolve an int, or a bilc-style symbolic 'rows'/'cols' [+-N] expr."""
    if isinstance(expr, int):
        return expr
    m = re.match(r'^(rows|cols)\s*([+\-]\s*\d+)?$', str(expr))
    if not m:
        return None
    base = rows if m.group(1) == "rows" else cols
    delta = m.group(2)
    return base + int(delta.replace(" ", "")) if delta else base


def proc_color(name, colors):
    if name not in colors:
        colors[name] = PALETTE[len(colors) % len(PALETTE)]
    return colors[name]


DIRECTIONS = ("east", "west", "north", "south")


def alias_direction(alias_name):
    low = alias_name.lower()
    for d in DIRECTIONS:
        if d in low:
            kind = "to" if low.startswith("to") else "from" if low.startswith("from") else "link"
            return d, kind
    return None, None


def edge_point(cx, cy, direction):
    half_w, half_h = CELL_W / 2, CELL_H / 2
    return {
        "east": (cx + half_w, cy),
        "west": (cx - half_w, cy),
        "north": (cx, cy - half_h),
        "south": (cx, cy + half_h),
    }[direction]


def outward_point(cx, cy, direction, dist):
    ex, ey = edge_point(cx, cy, direction)
    dx, dy = {"east": (1, 0), "west": (-1, 0), "north": (0, -1), "south": (0, 1)}[direction]
    return ex + dx * dist, ey + dy * dist


def render_svg(placements, has_default, default_proc, aliases, rows, cols, ids, title):
    grid_mode = not any("id" in p["match"] for p in placements)
    colors = {}
    cells = []  # (label, r_or_index, c, proc, is_default_fill)

    def resolve_grid(r, c):
        for p in placements:
            m = p["match"]
            if "id" in m:
                continue
            row_ok = "row" not in m or eval_expr(m["row"], rows, cols) == r
            col_ok = "col" not in m or eval_expr(m["col"], rows, cols) == c
            if row_ok and col_ok:
                return p["proc"], True
        return default_proc, False

    def resolve_id(node_id):
        for p in placements:
            m = p["match"]
            if "id" not in m:
                continue
            if eval_expr(m["id"], rows, cols) == node_id:
                return p["proc"], True
        return default_proc, False

    if grid_mode:
        for r in range(rows):
            for c in range(cols):
                proc, explicit = resolve_grid(r, c)
                cells.append({"r": r, "c": c, "proc": proc, "explicit": explicit})
        grid_w, grid_h = cols, rows
    else:
        for node_id in range(ids):
            proc, explicit = resolve_id(node_id)
            cells.append({"r": 0, "c": node_id, "proc": proc, "explicit": explicit})
        grid_w, grid_h = ids, 1

    width = MARGIN * 2 + grid_w * CELL_W + (grid_w - 1) * GAP
    title_h = 40
    grid_top = MARGIN + title_h
    legend_h = 30
    height = grid_top + grid_h * CELL_H + (grid_h - 1) * GAP + MARGIN + legend_h

    svg = []
    svg.append(f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
                f'viewBox="0 0 {width} {height}" font-family="Helvetica, Arial, sans-serif">')
    svg.append(f'<title>{escape(title)}</title>')
    svg.append(f'<rect x="0" y="0" width="{width}" height="{height}" fill="#ffffff"/>')
    svg.append('<defs>'
                '<marker id="arrow-out" viewBox="0 0 10 10" refX="9" refY="5" '
                'markerWidth="7" markerHeight="7" orient="auto-start-reverse">'
                '<path d="M0,0 L10,5 L0,10 z" fill="#333"/></marker>'
                '</defs>')
    svg.append(f'<text x="{MARGIN}" y="{MARGIN}" font-size="18" font-weight="bold" '
                f'fill="#222">{escape(title)}</text>')

    def cell_xy(r, c):
        return (MARGIN + c * (CELL_W + GAP), grid_top + r * (CELL_H + GAP))

    for cell in cells:
        x, y = cell_xy(cell["r"], cell["c"])
        proc = cell["proc"]
        label = proc if proc else ("default" if not cell["explicit"] and has_default else "—")
        fill = proc_color(proc, colors) if proc else DEFAULT_FILL
        dash = '' if cell["explicit"] else ' stroke-dasharray="6,4"'
        text_fill = "#ffffff" if proc else "#666666"
        svg.append(f'<rect x="{x}" y="{y}" width="{CELL_W}" height="{CELL_H}" rx="10" '
                    f'fill="{fill}" fill-opacity="{"0.9" if proc else "1"}" '
                    f'stroke="{DEFAULT_STROKE}"{dash} stroke-width="1.5"/>')
        corner = f'row {cell["r"]}, col {cell["c"]}' if grid_mode else f'id {cell["c"]}'
        svg.append(f'<text x="{x + CELL_W/2}" y="{y + CELL_H/2 - 4}" text-anchor="middle" '
                    f'font-size="15" font-weight="600" fill="{text_fill}">{escape(label)}</text>')
        svg.append(f'<text x="{x + CELL_W/2}" y="{y + CELL_H/2 + 16}" text-anchor="middle" '
                    f'font-size="10" fill="{text_fill}" opacity="0.85">{escape(corner)}</text>')

        if proc and proc in aliases and grid_mode:
            cx, cy = x + CELL_W / 2, y + CELL_H / 2
            by_direction = {}
            for alias_name, idx in aliases[proc].items():
                d, kind = alias_direction(alias_name)
                if d is None:
                    continue
                by_direction.setdefault(d, []).append((alias_name, idx, kind))
            for d, entries in by_direction.items():
                ex, ey = edge_point(cx, cy, d)
                ox, oy = outward_point(cx, cy, d, GAP / 2)
                for k, (alias_name, idx, kind) in enumerate(entries):
                    off = (k - (len(entries) - 1) / 2) * 14
                    perp_x, perp_y = (0, off) if d in ("east", "west") else (off, 0)
                    x1, y1 = ex + perp_x, ey + perp_y
                    x2, y2 = ox + perp_x, oy + perp_y
                    if kind == "from":
                        x1, y1, x2, y2 = x2, y2, x1, y1
                    marker = ' marker-end="url(#arrow-out)"' if kind in ("to", "from") else ''
                    dasharray = ' stroke-dasharray="3,3"' if kind == "link" else ''
                    svg.append(f'<line x1="{x1:.1f}" y1="{y1:.1f}" x2="{x2:.1f}" y2="{y2:.1f}" '
                                f'stroke="#333"{dasharray} stroke-width="1.5"{marker}/>')
                    lx, ly = ox + perp_x, oy + perp_y
                    anchor = "middle"
                    dy = {"north": -8, "south": 16, "east": -8, "west": -8}[d]
                    svg.append(f'<text x="{lx:.1f}" y="{ly + dy:.1f}" text-anchor="{anchor}" '
                                f'font-size="10" fill="#333">{escape(alias_name)}:{idx}</text>')

    # Legend.
    legend_y = grid_top + grid_h * CELL_H + (grid_h - 1) * GAP + 24
    lx = MARGIN
    svg.append(f'<text x="{lx}" y="{legend_y}" font-size="11" fill="#555">Legend:</text>')
    lx += 60
    for proc, color in colors.items():
        svg.append(f'<rect x="{lx}" y="{legend_y - 10}" width="12" height="12" rx="2" fill="{color}"/>')
        svg.append(f'<text x="{lx + 16}" y="{legend_y}" font-size="11" fill="#333">{escape(proc)}</text>')
        lx += 16 + 9 * len(proc) + 20

    svg.append('</svg>')
    return "\n".join(svg)


def escape(s):
    return (str(s).replace("&", "&amp;").replace("<", "&lt;")
            .replace(">", "&gt;").replace('"', "&quot;"))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("manifest", type=Path, help="path to a *.topology.yaml file")
    ap.add_argument("-o", "--out", type=Path, help="output .svg path (default: alongside input)")
    ap.add_argument("--rows", type=int, default=1, help="rows to render for a row/col grid (default: 1, auto-grown to fit explicit row matches)")
    ap.add_argument("--cols", type=int, default=6, help="cols to render for a row/col grid (default: 6)")
    ap.add_argument("--ids", type=int, default=6, help="node count to render for an id-based layout (default: 6)")
    args = ap.parse_args()

    text = args.manifest.read_text()
    placements, has_default, default_proc, aliases = parse_manifest(text)
    if not placements and not has_default:
        sys.exit(f"error: no placements found in {args.manifest}")

    rows = args.rows
    cols = args.cols
    for p in placements:
        m = p["match"]
        if isinstance(m.get("row"), int):
            rows = max(rows, m["row"] + 1)
        if isinstance(m.get("col"), int):
            cols = max(cols, m["col"] + 1)

    svg = render_svg(placements, has_default, default_proc, aliases,
                      rows, cols, args.ids, title=args.manifest.name)

    out = args.out or args.manifest.with_suffix(".svg")
    out.write_text(svg)
    print(f"wrote {out}")


if __name__ == "__main__":
    main()
