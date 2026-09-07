"""PADDR -> home chiplet / L2 bank / L2 set 매핑.

mesh 플랫폼(36 칩렛, 1 GPU) 기본 설정 기준.
근거: mem/cache/directory.go(getSet), lmf/homenodefinder.go(GetChipletId),
      mgpusim/platform/builders/common.go mesh connectL1ToL2.
"""

import numpy as np

OFFSET = 2147487744       # = 1*2GB + 4KB
CHIPLET_STRIPE = 131072   # 128KB = 32 banks * 4KB page
BANK_STRIPE = 64          # cacheline
NUM_CHIPLETS = 36
NUM_BANKS = 32            # banks per chiplet
NUM_SETS = 128            # 128KB / 16-way / 64B
NUM_WAYS = 16

MASK64 = (1 << 64) - 1


def _wang(li):
    """fmix64."""
    h = li & MASK64
    h ^= h >> 33
    h = (h * 0xff51afd7ed558ccd) & MASK64
    h ^= h >> 33
    h = (h * 0xc4ceb9fe1a85ec53) & MASK64
    h ^= h >> 33
    return h


def home_chiplet(addr):
    rel = addr - OFFSET
    return (rel % (NUM_CHIPLETS * CHIPLET_STRIPE)) // CHIPLET_STRIPE


def l2_bank(addr):
    rel = addr - OFFSET
    return (rel // BANK_STRIPE) % NUM_BANKS


def l2_set(addr):
    return _wang(addr // BANK_STRIPE // NUM_BANKS) % NUM_SETS


# --------------------- numpy 벡터 버전 (대량 trace용) ----------------------
def _wang_vec(arr):
    h = arr.astype('uint64')
    h ^= h >> 33
    h *= np.uint64(0xff51afd7ed558ccd)
    h ^= h >> 33
    h *= np.uint64(0xc4ceb9fe1a85ec53)
    h ^= h >> 33
    return h


def home_chiplet_vec(addr_arr):
    rel = addr_arr.astype('int64') - OFFSET
    return ((rel % (NUM_CHIPLETS * CHIPLET_STRIPE)) // CHIPLET_STRIPE).astype('int16')


def l2_bank_vec(addr_arr):
    rel = addr_arr.astype('int64') - OFFSET
    return ((rel // BANK_STRIPE) % NUM_BANKS).astype('int16')


def l2_set_vec(addr_arr):
    blk = (addr_arr.astype('uint64') // BANK_STRIPE) // NUM_BANKS
    return (_wang_vec(blk) % np.uint64(NUM_SETS)).astype('int32')


if __name__ == "__main__":
    # 검증된 예시 (메모: reference_paddr_mapping)
    for a, exp in [(2639826944, (12, 0, 42)),
                   (2625806336, (13, 0, 84))]:
        got = (home_chiplet(a), l2_bank(a), l2_set(a))
        ok = "OK" if got == exp else "FAIL"
        print(f"{ok} addr={a}: got={got} exp={exp}")
