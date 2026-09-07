#!/usr/bin/env python3
"""
pc_to_buffer.py
---------------
Static analyzer that takes a GCN3 disassembly and produces a CSV mapping each
memory-access PC (flat_load/store, global_load/store, buffer_load/store) to the
Go-side KernelArgs field it accesses (i.e., the OpenCL kernel argument / buffer
name).

Usage:
    python3 pc_to_buffer.py <kernels.disasm>
                            [--go <benchmark.go>]
                            [--output <out.csv>]
                            [--quiet]

If --go is omitted, the script auto-discovers a benchmark.go alongside the
disassembly. Output is written to CSV; a compact summary is also printed to
stdout. The script is purely static — no runtime trace is consulted.
"""

import re
import argparse
import csv
import os
import sys
from collections import OrderedDict


# ---------------------------------------------------------------------------
# Go type -> byte size (for KernelArgs offset computation, HSA ABI alignment)
# ---------------------------------------------------------------------------
TYPE_SIZE = {
    'driver.GPUPtr':   8,
    'driver.LocalPtr': 4,
    'int8': 1, 'uint8': 1, 'byte': 1, 'bool': 1,
    'int16': 2, 'uint16': 2,
    'int32': 4, 'uint32': 4, 'float32': 4, 'rune': 4,
    'int64': 8, 'uint64': 8, 'float64': 8,
    'int': 8, 'uint': 8,
}


def type_size(t):
    t = t.strip().lstrip('*')
    return TYPE_SIZE.get(t, 8)


def align_up(off, size):
    a = min(size, 8)
    return (off + a - 1) // a * a


# ---------------------------------------------------------------------------
# Parse Go file for KernelArgs structs and compute field offsets
# ---------------------------------------------------------------------------
STRUCT_RE = re.compile(r'type\s+(\w+Args)\s+struct\s*\{([^}]+)\}', re.DOTALL)
FIELD_RE = re.compile(r'^([\w,\s]+?)\s+([\w\.\*]+)\s*$')


def parse_go_kernel_args(go_path):
    text = open(go_path).read()
    structs = OrderedDict()
    for m in STRUCT_RE.finditer(text):
        name = m.group(1)
        body = m.group(2)
        fields = []
        offset = 0
        for line in body.splitlines():
            line = re.sub(r'//.*$', '', line).strip()
            if not line:
                continue
            fm = FIELD_RE.match(line)
            if not fm:
                continue
            names = [n.strip() for n in fm.group(1).split(',')]
            ftype = fm.group(2).strip()
            sz = type_size(ftype)
            for n in names:
                offset = align_up(offset, sz)
                fields.append((n, ftype, offset))
                offset += sz
        structs[name] = fields
    return structs


# ---------------------------------------------------------------------------
# Disassembly parsing
# ---------------------------------------------------------------------------
KERNEL_LABEL_RE = re.compile(
    r'^([0-9a-fA-F]+)\s+<?([\w_]+?)(?:\$local)?>?:\s*$')
BB_LABEL_RE = re.compile(r'^[0-9a-fA-F]+\s+<?BB\d+_\d+>?:\s*$')
SRC_LINE_RE = re.compile(r'^\s*;\s+(/[^:]+):(\d+)\s*$')
INSTR_RE = re.compile(
    r'^\s+(\S+)\s*(.*?)\s*//\s*([0-9a-fA-F]+):\s*([0-9A-F ]+)\s*$'
)
MEM_PREFIXES = ('flat_load', 'flat_store',
                'global_load', 'global_store',
                'buffer_load', 'buffer_store')


def parse_disasm(path):
    kernels = []
    cur = None
    last_src_line = None
    last_src_text = None

    for raw in open(path):
        # Basic-block labels (BB0_1, BB1_3, ...) are NOT new kernels — skip
        if BB_LABEL_RE.match(raw):
            continue

        m = KERNEL_LABEL_RE.match(raw)
        if m:
            cur = {'name': m.group(2),
                   'start_pc': int(m.group(1), 16),
                   'instructions': []}
            kernels.append(cur)
            last_src_line = None
            last_src_text = None
            continue

        m = SRC_LINE_RE.match(raw)
        if m:
            last_src_line = os.path.basename(m.group(1)) + ':' + m.group(2)
            continue

        s = raw.strip()
        if s.startswith(';') and not re.match(r'^;\s+/', s):
            last_src_text = s.lstrip(';').strip()
            continue

        m = INSTR_RE.match(raw)
        if m and cur is not None:
            cur['instructions'].append({
                'pc': int(m.group(3), 16),
                'mnemonic': m.group(1),
                'operands': m.group(2).strip(),
                'src_line': last_src_line,
                'src_text': last_src_text,
            })

    return kernels


# ---------------------------------------------------------------------------
# Phase 1 — Track SGPR <-> kernarg offset (snapshot at each PC)
# ---------------------------------------------------------------------------
SLOAD_RE = re.compile(
    r's_load_dword(?:x2|x4)?\s+'
    r's(?:\[(\d+):(\d+)\]|(\d+))'
    r'\s*,\s*'
    r's\[(\d+):(\d+)\]'
    r'\s*,\s*'
    r'(0x[0-9a-fA-F]+|\d+)'
)


def parse_sload(text):
    m = SLOAD_RE.search(text)
    if not m:
        return None
    if m.group(1):
        lo, hi = int(m.group(1)), int(m.group(2))
    else:
        lo = hi = int(m.group(3))
    base = (int(m.group(4)), int(m.group(5)))
    off = int(m.group(6), 0)
    return lo, hi, base, off


# ---------------------------------------------------------------------------
# Phase 2 — Backward trace: which SGPR did v[X:Y] come from?
# ---------------------------------------------------------------------------
VADD_RE = re.compile(
    r'v_add(?:c)?_u32_e32\s+v(\d+)\s*,\s*vcc\s*,\s*'
    r'(s\d+|v\d+|[\-0-9x]+)\s*,\s*'
    r'(s\d+|v\d+|[\-0-9x]+)'
)
VMOV_SGPR_RE = re.compile(r'v_mov_b32_e32\s+v(\d+)\s*,\s*s(\d+)')
VADDR_RE = re.compile(r'v\[(\d+):(\d+)\]')
DEF_VREG_RE = re.compile(r'^\s*(\S+)\s+v(\d+)')


def trace_to_sgpr(vreg, instructions, idx, depth=0, visited=None):
    """Backward-walk from idx-1 to 0, find which SGPR ultimately produces vreg.
    Returns SGPR number, or None on failure."""
    if visited is None:
        visited = set()
    if depth > 8 or vreg in visited:
        return None
    visited.add(vreg)

    for i in range(idx - 1, -1, -1):
        ins = instructions[i]
        full = ins['mnemonic'] + ' ' + ins['operands']

        m = VMOV_SGPR_RE.match(full)
        if m and int(m.group(1)) == vreg:
            return int(m.group(2))

        m = VADD_RE.match(full)
        if m and int(m.group(1)) == vreg:
            for src in (m.group(2), m.group(3)):
                if src.startswith('s'):
                    return int(src[1:])
            for src in (m.group(2), m.group(3)):
                if src.startswith('v'):
                    r = trace_to_sgpr(int(src[1:]), instructions, i,
                                      depth + 1, visited.copy())
                    if r is not None:
                        return r
            return None

        # Some other def of vreg → give up (keeps the trace simple)
        m = DEF_VREG_RE.match(full)
        if m and int(m.group(2)) == vreg and not full.startswith(
                ('v_add_u32', 'v_addc_u32', 'v_mov_b32', 'v_lshlrev',
                 'v_ashrrev', 'v_sub_u32', 'v_subrev_u32')):
            return None

    return None


# ---------------------------------------------------------------------------
# Main per-kernel analysis
# ---------------------------------------------------------------------------
def analyze_kernel(kernel, offset_to_name):
    snapshot = {}      # (s_lo, s_hi) -> kernarg_offset
    rows = []

    # Determine kernarg base SGPR by frequency of use across all s_load's in
    # this kernel. The first s_load may target a dispatch_ptr (s[4:5]) rather
    # than the kernarg segment ptr (often s[6:7]), so picking the most-used
    # base is more reliable than picking the first one.
    base_counts = {}
    for ins in kernel['instructions']:
        if ins['mnemonic'].startswith('s_load_dword'):
            parsed = parse_sload(ins['mnemonic'] + ' ' + ins['operands'])
            if parsed:
                # Weight dwordx4 more, as it touches more SGPRs (= more
                # evidence that this is the kernarg base).
                weight = 4 if ins['mnemonic'] == 's_load_dwordx4' else \
                         2 if ins['mnemonic'] == 's_load_dwordx2' else 1
                base_counts[parsed[2]] = base_counts.get(parsed[2], 0) + weight
    kernarg_base = (max(base_counts, key=base_counts.get)
                    if base_counts else None)

    for idx, ins in enumerate(kernel['instructions']):
        if ins['mnemonic'].startswith('s_load_dword'):
            parsed = parse_sload(ins['mnemonic'] + ' ' + ins['operands'])
            if parsed:
                lo, hi, base, off = parsed
                if base == kernarg_base:
                    # x4 loads 4 dwords = two consecutive 8-byte pointer slots
                    if ins['mnemonic'] == 's_load_dwordx4':
                        snapshot[(lo,     lo + 1)] = off
                        snapshot[(lo + 2, lo + 3)] = off + 8
                    else:
                        snapshot[(lo, hi)] = off

        if any(ins['mnemonic'].startswith(p) for p in MEM_PREFIXES):
            mode = 'load' if 'load' in ins['mnemonic'] else 'store'
            addr = VADDR_RE.search(ins['operands'])
            if not addr:
                continue
            vlo = int(addr.group(1))
            vhi = int(addr.group(2))

            slo = trace_to_sgpr(vlo, kernel['instructions'], idx)
            shi = trace_to_sgpr(vhi, kernel['instructions'], idx)

            buffer = '?'
            kernarg_off = None
            if slo is not None and shi is not None:
                pair = (min(slo, shi), max(slo, shi))
                if pair in snapshot:
                    kernarg_off = snapshot[pair]
                    buffer = offset_to_name.get(kernarg_off,
                                                f'@0x{kernarg_off:x}')
                else:
                    buffer = f'?@s[{pair[0]}:{pair[1]}]'

            rows.append({
                'kernel': kernel['name'],
                'pc': f'0x{ins["pc"]:x}',
                'mnemonic': ins['mnemonic'],
                'mode': mode,
                'buffer': buffer,
                'kernarg_offset': (f'0x{kernarg_off:x}'
                                   if kernarg_off is not None else '?'),
                'operands': ins['operands'],
                'src_line': ins.get('src_line') or '',
                'src_text': ins.get('src_text') or '',
            })

    return rows


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------
def find_go_file(disasm_path):
    d = os.path.dirname(os.path.abspath(disasm_path))
    for cand_dir in [d, os.path.dirname(d), os.path.dirname(os.path.dirname(d))]:
        if not os.path.isdir(cand_dir):
            continue
        for fn in sorted(os.listdir(cand_dir)):
            if not fn.endswith('.go') or fn == 'esc.go':
                continue
            p = os.path.join(cand_dir, fn)
            try:
                txt = open(p).read()
                if 'Args struct' in txt:
                    return p
            except (IOError, UnicodeDecodeError):
                continue
    return None


def match_kernel_to_struct(kernel_name, kernel_idx, struct_keys):
    if not struct_keys:
        return None
    if len(struct_keys) == 1:
        return struct_keys[0]

    # First: try to find a struct whose digit matches kernel index (1-based)
    for sk in struct_keys:
        digits = re.findall(r'\d+', sk)
        if digits and int(digits[0]) == kernel_idx + 1:
            return sk

    # Fallback: positional
    return struct_keys[min(kernel_idx, len(struct_keys) - 1)]


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('disasm', help='path to kernels.disasm')
    ap.add_argument('--go', help='path to benchmark.go (auto-discover if omitted)')
    ap.add_argument('--output', '-o', help='output CSV path')
    ap.add_argument('--quiet', '-q', action='store_true',
                    help='suppress info logs and stdout table')
    args = ap.parse_args()

    log = (lambda *a, **kw: None) if args.quiet else \
          (lambda *a, **kw: print(*a, file=sys.stderr, **kw))

    # ---- Go-side KernelArgs ----
    go_path = args.go or find_go_file(args.disasm)
    structs = {}
    if go_path:
        log(f'[info] parsing Go kernarg structs: {go_path}')
        structs = parse_go_kernel_args(go_path)
        for sn, fields in structs.items():
            log(f'  {sn}:')
            for fn, ft, off in fields:
                log(f'    0x{off:02x}  {fn:30s}  {ft}')
    else:
        log('[warn] no benchmark.go found; output will use offsets only')

    # ---- Disassembly ----
    log(f'[info] parsing disassembly: {args.disasm}')
    kernels = parse_disasm(args.disasm)
    log(f'[info] {len(kernels)} kernel(s): '
        f'{", ".join(k["name"] for k in kernels)}')

    struct_keys = list(structs.keys())
    all_rows = []

    for i, kernel in enumerate(kernels):
        chosen = match_kernel_to_struct(kernel['name'], i, struct_keys)
        offset_to_name = {}
        if chosen:
            offset_to_name = {off: name for name, _, off in structs[chosen]}
            log(f'[info] kernel \'{kernel["name"]}\' <-> {chosen}')
        else:
            log(f'[info] kernel \'{kernel["name"]}\': no matching Args struct')

        all_rows.extend(analyze_kernel(kernel, offset_to_name))

    if not all_rows:
        log('[warn] no memory instructions found')
        return

    # ---- CSV ----
    if args.output:
        out_path = args.output
    else:
        out_path = os.path.join(os.path.dirname(os.path.abspath(args.disasm)),
                                'pc_to_buffer.csv')

    cols = ['kernel', 'pc', 'mnemonic', 'mode', 'buffer',
            'kernarg_offset', 'src_line', 'src_text', 'operands']
    with open(out_path, 'w', newline='') as f:
        w = csv.DictWriter(f, fieldnames=cols)
        w.writeheader()
        w.writerows(all_rows)
    log(f'[info] wrote {len(all_rows)} rows to {out_path}')

    # ---- Stdout summary ----
    if not args.quiet:
        print()
        print(f'{"kernel":<28} {"pc":<10} {"mnemonic":<22} '
              f'{"mode":<6} {"buffer":<22} {"src":<24}')
        print('-' * 115)
        for r in all_rows:
            src = r['src_line'] or ''
            print(f'{r["kernel"]:<28} {r["pc"]:<10} {r["mnemonic"]:<22} '
                  f'{r["mode"]:<6} {r["buffer"]:<22} {src:<24}')


if __name__ == '__main__':
    main()
