"""(고리 2a) 벤치마크별 원격 접근 비율 막대.

`local_remote_ratio.py` 가 만든 엑셀을 읽어 remote_ratio 만 막대로.

입력
----
  _out/cacheline/<run-id>/local_remote_access_ratio.xlsx
  (없으면 graceful skip — `analyze/run.sh cacheline/local_remote_ratio.py <date>` 먼저)

출력
----
  _out/cacheline/remote-ratio/<run_tag>/all.{png,pdf}
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

import numpy as np
import pandas as pd
import matplotlib.pyplot as plt

from _paths import local_remote_ratio_xlsx, run_root, domain_out_dir
from plot_style import apply_paper_style, color_for, save_fig, SIZE_SINGLE


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--date", default="mesh-6x6/latest")
    args = ap.parse_args()

    xlsx = local_remote_ratio_xlsx(args.date)
    if not xlsx.exists():
        print(f"no input: {xlsx}\n"
              f"  먼저 'analyze/run.sh cacheline/local_remote_ratio.py {args.date}' 를 돌리세요.",
              file=sys.stderr)
        sys.exit(1)

    df = pd.read_excel(xlsx)
    if "remote_ratio" not in df.columns or "benchmark" not in df.columns:
        print(f"unexpected schema in {xlsx}: {df.columns.tolist()}", file=sys.stderr)
        sys.exit(1)

    df = df[["benchmark", "remote_ratio"]].copy()
    df["remote_pct"] = df["remote_ratio"] * 100.0
    df = df.sort_values("remote_pct", ascending=True)

    apply_paper_style()
    fig, ax = plt.subplots(figsize=SIZE_SINGLE)
    y = np.arange(len(df))
    ax.barh(y, df["remote_pct"],
            color=[color_for(i) for i in range(len(df))],
            edgecolor="black", linewidth=0.3)
    ax.axvline(50, color="#444", linestyle="--", linewidth=0.6, alpha=0.6)
    ax.set_yticks(y)
    ax.set_yticklabels(df["benchmark"], fontsize=6)
    ax.set_xlabel("원격 접근 비율 (%)")
    ax.set_xlim(0, 100)
    ax.set_title("벤치마크별 원격(L2 → 다른 칩렛) 접근 비율")
    ax.grid(axis="x", alpha=0.2)

    out = domain_out_dir("cacheline", "remote-ratio", args.date)
    save_fig(fig, out, "all")
    df.to_csv(out / "remote_ratio.csv", index=False)
    print(f"wrote {out}/all.{{png,pdf}} + remote_ratio.csv")


if __name__ == "__main__":
    main()
