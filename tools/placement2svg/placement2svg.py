#!/usr/bin/env python3
"""Render a bilc `roles/placement.json` placement manifest as an SVG diagram.

`placement.json` (`PlacementManifest` in tools/bilc/bilc.go; see docs/guide.md,
"What bilc does with placement") is the *only* placement manifest bilc
produces -- a JSON list of leaves, each with a clause match (an id, a
row/col pair -- either slot possibly the literal wildcard string "*" --
or the fully-wild default), an ordered chain of if/else conditions (raw
Go/Bil boolean-expression source text), the proc it resolves to, and
that call's link-index binds. It's mechanical and complete by design
(a host needs a concrete answer for every node), so this tool, like a
host, resolves every cell by walking the leaves in order and evaluating
each one's match plus condition chain -- first leaf that matches wins,
mirroring switch/if-else "first match wins" semantics and the
`resolveRole`/`evalExpr` functions in ../../emulator/cmd/wasm/static/index.html,
which this deliberately stays in lockstep with (same tiny expression
grammar: identifiers, int literals, ==, !=, &&, +, -, and the same "*"
wildcard-dimension convention) so the diagram always matches what the
real boot cascade would actually run.

Since placement.json never records a concrete grid/id-space size (bilc
never knows one at compile time), --rows/--cols/--ids pick an
illustrative size to render.
"""

import argparse
import json
import re
import sys
from pathlib import Path

CELL_W, CELL_H, GAP, MARGIN = 150, 80, 60, 50

# Okabe-Ito colorblind-safe qualitative palette.
PALETTE = ["#E69F00", "#56B4E9", "#009E73", "#F0E442",
           "#0072B2", "#D55E00", "#CC79A7", "#999999"]
DEFAULT_FILL = "#EDEDED"
DEFAULT_STROKE = "#888888"

WILDCARD = "*"


class EvalError(ValueError):
    pass


def eval_expr(src, env):
    """Evaluate one placement.json expression against env (r/c/rows/cols).

    Hand-rolled, not eval()/Function()-based, deliberately: this mirrors
    evalExpr in ../../emulator/cmd/wasm/static/index.html exactly (identifiers,
    int literals, ==, !=, &&, +, -) rather than accepting arbitrary
    Python syntax, so this tool's notion of "what placement.json expressions
    mean" can never drift ahead of what the real host actually implements.
    """
    i = 0
    n = len(src)

    def skip_ws():
        nonlocal i
        while i < n and src[i].isspace():
            i += 1

    def consume(tok):
        nonlocal i
        skip_ws()
        if src.startswith(tok, i):
            i += len(tok)
            return True
        return False

    def parse_primary():
        nonlocal i
        skip_ws()
        if consume("("):
            v = parse_and()
            if not consume(")"):
                raise EvalError(f"expected ) in {src!r}")
            return v
        m = re.match(r"[A-Za-z_][A-Za-z0-9_]*", src[i:])
        if m:
            i += len(m.group(0))
            if m.group(0) not in env:
                raise EvalError(f"unknown identifier {m.group(0)!r} in {src!r}")
            return env[m.group(0)]
        m = re.match(r"[0-9]+", src[i:])
        if m:
            i += len(m.group(0))
            return int(m.group(0))
        raise EvalError(f"unexpected token at {i} in {src!r}")

    def parse_add():
        v = parse_primary()
        while True:
            if consume("+"):
                v = v + parse_primary()
            elif consume("-"):
                v = v - parse_primary()
            else:
                return v

    def parse_cmp():
        v = parse_add()
        if consume("=="):
            return v == parse_add()
        if consume("!="):
            return v != parse_add()
        return v

    def parse_and():
        v = parse_cmp()
        while consume("&&"):
            rhs = parse_cmp()  # always parse, to advance i past a false lhs too
            v = bool(v) and bool(rhs)
        return v

    result = parse_and()
    skip_ws()
    if i != n:
        raise EvalError(f"trailing input in {src!r}")
    return result


def leaf_clause_matches(leaf, r, c, env):
    """Whether leaf's own `match` (ignoring its condition chain) applies
    at (r, c) -- a wildcarded slot ("*") always matches that dimension.
    """
    m = leaf["match"]
    if m.get("default"):
        return True
    if "id" in m:
        return eval_expr(m["id"], env) == r * env["cols"] + c
    row_ok = m.get("row") == WILDCARD or eval_expr(m["row"], env) == r
    col_ok = m.get("col") == WILDCARD or eval_expr(m["col"], env) == c
    return row_ok and col_ok


def resolve_role(leaves, r, c, rows, cols):
    """Mirrors index.html's resolveRole: first leaf whose clause match
    and if/else condition chain both hold wins.
    """
    env = {"r": r, "c": c, "rows": rows, "cols": cols}
    for leaf in leaves:
        if not leaf_clause_matches(leaf, r, c, env):
            continue
        ok = True
        for cond in leaf.get("conditions", []):
            v = bool(eval_expr(cond["expr"], env))
            if cond.get("negate"):
                v = not v
            if not v:
                ok = False
                break
        if ok:
            return leaf["proc"]
    return None


def is_grid_manifest(leaves):
    return not any("id" in leaf["match"] for leaf in leaves)


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


def leaf_binds_by_proc(leaves):
    """First leaf's `binds` for each distinct proc -- every leaf that
    resolves to the same proc shares the same channel/link wiring (the
    proc's own declared parameters never change), so the first one seen
    is as good as any for drawing that proc's link arrows.
    """
    out = {}
    for leaf in leaves:
        proc = leaf["proc"]
        if proc not in out and leaf.get("binds"):
            out[proc] = leaf["binds"]
    return out


def render_svg(leaves, rows, cols, ids, title):
    grid_mode = is_grid_manifest(leaves)
    colors = {}
    cells = []
    binds = leaf_binds_by_proc(leaves)

    if grid_mode:
        for r in range(rows):
            for c in range(cols):
                proc = resolve_role(leaves, r, c, rows, cols)
                cells.append({"r": r, "c": c, "proc": proc})
        grid_w, grid_h = cols, rows
    else:
        for node_id in range(ids):
            proc = resolve_role(leaves, 0, node_id, ids, ids)
            cells.append({"r": 0, "c": node_id, "proc": proc})
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
        label = proc if proc else "—"
        fill = proc_color(proc, colors) if proc else DEFAULT_FILL
        text_fill = "#ffffff" if proc else "#666666"
        svg.append(f'<rect x="{x}" y="{y}" width="{CELL_W}" height="{CELL_H}" rx="10" '
                    f'fill="{fill}" fill-opacity="{"0.9" if proc else "1"}" '
                    f'stroke="{DEFAULT_STROKE}" stroke-width="1.5"/>')
        corner = f'row {cell["r"]}, col {cell["c"]}' if grid_mode else f'id {cell["c"]}'
        svg.append(f'<text x="{x + CELL_W/2}" y="{y + CELL_H/2 - 4}" text-anchor="middle" '
                    f'font-size="15" font-weight="600" fill="{text_fill}">{escape(label)}</text>')
        svg.append(f'<text x="{x + CELL_W/2}" y="{y + CELL_H/2 + 16}" text-anchor="middle" '
                    f'font-size="10" fill="{text_fill}" opacity="0.85">{escape(corner)}</text>')

        if proc and proc in binds and grid_mode:
            cx, cy = x + CELL_W / 2, y + CELL_H / 2
            by_direction = {}
            for alias_name, idx in binds[proc].items():
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
                    dy = {"north": -8, "south": 16, "east": -8, "west": -8}[d]
                    svg.append(f'<text x="{lx:.1f}" y="{ly + dy:.1f}" text-anchor="middle" '
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
    ap.add_argument("manifest", type=Path, help="path to a roles/placement.json file")
    ap.add_argument("-o", "--out", type=Path, help="output .svg path (default: alongside input)")
    ap.add_argument("--rows", type=int, default=1, help="rows to render for a row/col grid (default: 1, auto-grown to fit explicit row matches)")
    ap.add_argument("--cols", type=int, default=6, help="cols to render for a row/col grid (default: 6)")
    ap.add_argument("--ids", type=int, default=6, help="node count to render for an id-based layout (default: 6)")
    args = ap.parse_args()

    manifest = json.loads(args.manifest.read_text())
    leaves = manifest.get("leaves", [])
    if not leaves:
        sys.exit(f"error: no leaves found in {args.manifest}")

    rows = args.rows
    cols = args.cols
    for leaf in leaves:
        m = leaf["match"]
        if isinstance(m.get("row"), str) and m["row"] != WILDCARD and m["row"].lstrip("-").isdigit():
            rows = max(rows, int(m["row"]) + 1)
        if isinstance(m.get("col"), str) and m["col"] != WILDCARD and m["col"].lstrip("-").isdigit():
            cols = max(cols, int(m["col"]) + 1)

    # placement.json always lives at nodeprog/<demo>/roles/placement.json -- the
    # demo name one level up is a far more useful title than "roles".
    parent = args.manifest.parent
    title = parent.parent.name if parent.name == "roles" and parent.parent.name else parent.name or args.manifest.name

    try:
        svg = render_svg(leaves, rows, cols, args.ids, title=title)
    except EvalError as e:
        sys.exit(f"error: {e}")

    out = args.out or args.manifest.with_suffix(".svg")
    out.write_text(svg)
    print(f"wrote {out}")


if __name__ == "__main__":
    main()
