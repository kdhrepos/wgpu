"""논문급 공유 그림 스타일 (IEEE/ACM 2-column 기준).

사용법
-----
    from plot_style import apply_paper_style, color_for, save_fig
    from plot_style import SIZE_SINGLE, SIZE_DOUBLE, HATCHES

    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_SINGLE)
    ax.bar(..., color=color_for(0), hatch=HATCHES[0])
    save_fig(fig, out_dir, "my-figure")
"""

from __future__ import annotations

import os
from pathlib import Path
from typing import Iterable

import matplotlib as mpl
import matplotlib.pyplot as plt
from matplotlib import font_manager as _fm

# ── CJK 폰트 등록 ──────────────────────────────────────────────────────────
# TTC multi-face 라 mpl 기본 스캔 누락. addfont 로 명시 등록.
# 첫 face = "Noto Sans CJK JP" 이름으로 등록되지만 PanCJK → 한글 글리프 포함.
_CJK_PATHS = ["/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"]
for _p in _CJK_PATHS:
    if os.path.exists(_p):
        try:
            _fm.fontManager.addfont(_p)
        except Exception:
            pass

# ── 팔레트 ─────────────────────────────────────────────────────────────────
# Okabe-Ito (색약 친화, 8색).  grayscale 인쇄에서도 명도 차이로 구별 가능.
OKABE_ITO = [
    "#0072B2",  # 파랑     (L≈40)
    "#D55E00",  # 주황     (L≈52)
    "#009E73",  # 초록     (L≈57)
    "#CC79A7",  # 자홍     (L≈60)
    "#E69F00",  # 노랑주황 (L≈71)
    "#56B4E9",  # 하늘     (L≈73)
    "#F0E442",  # 노랑     (L≈88)
    "#000000",  # 검정     (L≈0)
]

# 초록 → 빨강 그라데이션 (eviction severity 시각화용, 5단계)
SEVERITY_5 = ["#2ca02c", "#a6d96a", "#fdae61", "#f46d43", "#d73027"]
SEVERITY_6 = ["#2ca02c", "#78c679", "#a6d96a", "#fdae61", "#f46d43", "#d73027"]

# 흑백 인쇄 대비를 위한 hatch 패턴 (색 + hatch 병행 권장)
HATCHES = ["", "///", "...", "xxx", "---", "+++", "\\\\\\", "ooo"]

# ── 표준 figure 크기 (인치) ────────────────────────────────────────────────
# IEEE Transactions / ACM SIGARCH 2-column 기준
SIZE_SINGLE      = (3.33, 2.2)   # 단일 칼럼
SIZE_SINGLE_TALL = (3.33, 2.9)
SIZE_DOUBLE      = (6.9,  2.4)   # 이중 칼럼
SIZE_DOUBLE_TALL = (6.9,  3.4)
SIZE_WIDE        = (6.9,  4.0)   # 많은 벤치마크 (가로 막대 등)

_APPLIED = False


def apply_paper_style() -> None:
    """rcParams 덮어쓰기. 여러 번 불러도 안전 (idempotent)."""
    global _APPLIED
    if _APPLIED:
        return
    _APPLIED = True

    mpl.rcParams.update({
        # ── 폰트 ──
        "font.family":      "sans-serif",
        "font.sans-serif":  [
            "Noto Sans CJK JP", "Noto Sans CJK KR",
            "Helvetica", "Arial", "DejaVu Sans",
        ],
        "axes.unicode_minus": False,
        "font.size":          8,
        "axes.titlesize":     8,
        "axes.labelsize":     8,
        "xtick.labelsize":    7,
        "ytick.labelsize":    7,
        "legend.fontsize":    7,
        "legend.title_fontsize": 7,
        "figure.titlesize":   9,

        # ── 선·축 ──
        "axes.linewidth":       0.6,
        "xtick.major.width":    0.6,
        "ytick.major.width":    0.6,
        "xtick.minor.width":    0.4,
        "ytick.minor.width":    0.4,
        "xtick.major.size":     2.5,
        "ytick.major.size":     2.5,
        "xtick.minor.size":     1.5,
        "ytick.minor.size":     1.5,
        "xtick.direction":      "out",
        "ytick.direction":      "out",
        "lines.linewidth":      1.0,
        "lines.markersize":     3,
        "patch.linewidth":      0.5,

        # ── spine: top/right 제거 ──
        "axes.spines.top":    False,
        "axes.spines.right":  False,

        # ── 격자 (y축만, 연하게) ──
        "axes.grid":          True,
        "axes.grid.axis":     "y",
        "grid.color":         "#cccccc",
        "grid.linewidth":     0.4,
        "grid.alpha":         1.0,
        "grid.linestyle":     "--",
        "axes.axisbelow":     True,

        # ── 범례 ──
        "legend.frameon":       False,
        "legend.borderpad":     0.4,
        "legend.labelspacing":  0.3,
        "legend.handlelength":  1.2,
        "legend.handleheight":  0.7,
        "legend.handletextpad": 0.4,

        # ── 저장 ──
        "figure.dpi":        150,
        "savefig.dpi":       400,
        "savefig.bbox":      "tight",
        "savefig.pad_inches": 0.03,

        # PDF: 폰트 Type 42 임베드 (인쇄소/리뷰어 호환)
        "pdf.fonttype": 42,
        "ps.fonttype":  42,

        # constrained_layout
        "figure.constrained_layout.use":    True,
        "figure.constrained_layout.h_pad":  0.05,
        "figure.constrained_layout.w_pad":  0.05,
        "figure.constrained_layout.hspace": 0.05,
        "figure.constrained_layout.wspace": 0.05,
    })


def color_for(i: int) -> str:
    """Okabe-Ito 팔레트 wrap-around 인덱싱."""
    return OKABE_ITO[i % len(OKABE_ITO)]


def save_fig(fig, out_dir, name: str, formats: Iterable[str] = ("png",)) -> list[Path]:
    """fig 를 out_dir/name.<ext> 로 저장. 디렉토리 자동 생성."""
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    if isinstance(formats, str):          # "png" -> ("png",); 문자순회 버그 방지
        formats = (formats,)
    paths: list[Path] = []
    for ext in formats:
        p = out_dir / f"{name}.{ext}"
        fig.savefig(p)
        paths.append(p)
    plt.close(fig)
    return paths


__all__ = [
    "OKABE_ITO", "SEVERITY_5", "SEVERITY_6", "HATCHES",
    "SIZE_SINGLE", "SIZE_SINGLE_TALL",
    "SIZE_DOUBLE", "SIZE_DOUBLE_TALL", "SIZE_WIDE",
    "apply_paper_style", "color_for", "save_fig",
]
