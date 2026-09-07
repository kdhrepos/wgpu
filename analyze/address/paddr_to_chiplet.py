#!/usr/bin/env python3
"""
PADDR -> home chiplet / L2 bank / set id (mesh, 36-chiplet single-GPU).

Usage:
  ./paddr_to_chiplet.py 2625806336 2550304768 ...
  echo 2553901056 | ./paddr_to_chiplet.py -
"""
import sys

OFFSET = 2147487744       # 1*2GB + 4KB (numGPU=1, GPU idx=1)
CHIPLET_STRIPE = 131072   # 128KB = 32 banks * 4KB page
BANK_STRIPE = 64          # cacheline
N_CHIPLETS = 36
N_BANKS = 32
N_SETS = 128
M = (1 << 64) - 1


def wang(x):
    h = x & M
    h ^= h >> 33
    h = (h * 0xff51afd7ed558ccd) & M
    h ^= h >> 33
    h = (h * 0xc4ceb9fe1a85ec53) & M
    h ^= h >> 33
    return h


def calc(addr):
    rel = addr - OFFSET
    chiplet = (rel % (N_CHIPLETS * CHIPLET_STRIPE)) // CHIPLET_STRIPE
    bank = (rel // BANK_STRIPE) % N_BANKS
    set_id = wang(addr // BANK_STRIPE // N_BANKS) % N_SETS
    return chiplet, bank, set_id


def parse(tok):
    tok = tok.strip()
    if not tok:
        return None
    return int(tok, 16) if tok.lower().startswith("0x") else int(tok)


def main():
    args = sys.argv[1:]
    if not args:
        print(__doc__.strip())
        sys.exit(1)
    toks = []
    for a in args:
        if a == "-":
            toks += [l for l in sys.stdin.read().split() if l]
        else:
            toks.append(a)
    for t in toks:
        addr = parse(t)
        if addr is None:
            continue
        c, b, s = calc(addr)
        print(f"addr={addr} (0x{addr:x}) -> chiplet {c}, bank {b}, set {s}")


if __name__ == "__main__":
    main()
