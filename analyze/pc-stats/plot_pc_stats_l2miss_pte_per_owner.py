import os
import argparse
import numpy as np
import pandas as pd
import matplotlib.pyplot as plt
from matplotlib.lines import Line2D

# PC별 PTE "특성"의 핵심 3개 차원만 사용.
# 그중 sharing(요청 칩렛 수)과 reuse(주소당 접근 수)가 가장 중요 -> 산점도의 x/y축으로.
# hop(거리)은 보조 -> 색으로.
METRICS = {
    'avg_requestor_chiplets':        'sharing\n(#chiplets)',
    'avg_access_per_addr_per_owner': 'reuse\n(acc/addr)',
    'avg_hop_count_per_owner':       'hop\n(distance)',
}
LOG_METRICS = {'avg_access_per_addr_per_owner'}


def _grid(n):
    ncols = min(5, n)
    nrows = int(np.ceil(n / ncols))
    return nrows, ncols


def plot_bubble(df, benchmarks, out_png):
    """핵심 산점도: x=공유도(sharing), y=재사용(reuse, log), 색=hop. 한 점 = 한 PC."""
    nrows, ncols = _grid(len(benchmarks))
    fig, axes = plt.subplots(nrows, ncols, figsize=(ncols * 3.8, nrows * 3.6),
                             squeeze=False, constrained_layout=True)
    axes = axes.ravel()

    hop_min, hop_max = df['avg_hop_count_per_owner'].min(), df['avg_hop_count_per_owner'].max()

    for ax, bench in zip(axes, benchmarks):
        sub = df[df['benchmark'] == bench]
        sc = ax.scatter(
            sub['avg_requestor_chiplets'],
            sub['avg_access_per_addr_per_owner'].clip(lower=1),
            s=70, c=sub['avg_hop_count_per_owner'],
            cmap='viridis', vmin=hop_min, vmax=hop_max,
            alpha=0.8, edgecolors='black', linewidths=0.5,
        )
        ax.set_yscale('log')
        ax.set_xlim(left=0)
        ax.set_title(f'{bench}  (n={len(sub)})', fontsize=10)
        ax.set_xlabel('sharing (#requestor chiplets)', fontsize=8)
        ax.set_ylabel('reuse (acc/addr, log)', fontsize=8)
        ax.tick_params(labelsize=7)
        ax.grid(True, which='both', alpha=0.25, linewidth=0.4)

    for ax in axes[len(benchmarks):]:
        ax.axis('off')

    cbar = fig.colorbar(sc, ax=axes.tolist(), fraction=0.02, pad=0.01)
    cbar.set_label('avg hop count', fontsize=9)
    fig.suptitle('Per-PC PTE characteristics: sharing (x) vs reuse (y), color = hop count',
                 fontsize=14)
    fig.savefig(out_png, dpi=400, bbox_inches='tight')
    plt.close(fig)
    return out_png


def plot_parallel(df, benchmarks, out_png, top_k=12):
    """평행좌표: 벤치마크 내에서 각 metric을 0~1 정규화, PC 1개 = 선 1개."""
    cols = list(METRICS.keys())
    nrows, ncols = _grid(len(benchmarks))
    fig, axes = plt.subplots(nrows, ncols, figsize=(ncols * 3.6, nrows * 3.0),
                             squeeze=False)
    axes = axes.ravel()
    x = np.arange(len(cols))

    for ax, bench in zip(axes, benchmarks):
        sub = df[df['benchmark'] == bench].sort_values(
            'total_access_count', ascending=False).head(top_k).copy()

        norm = sub[cols].astype(float).copy()
        for c in cols:
            v = norm[c]
            if c in LOG_METRICS:
                v = np.log10(v.clip(lower=1))
            lo, hi = v.min(), v.max()
            norm[c] = 0.5 if hi - lo < 1e-12 else (v - lo) / (hi - lo)

        cmap = plt.get_cmap('turbo', max(len(sub), 2))
        for i, (_, row) in enumerate(norm.iterrows()):
            ax.plot(x, row[cols].values, color=cmap(i), alpha=0.8, linewidth=1.3,
                    marker='o', markersize=3)

        ax.set_xticks(x)
        ax.set_xticklabels([METRICS[c] for c in cols], fontsize=6.5)
        ax.set_ylim(-0.05, 1.05)
        ax.set_title(f'{bench}  (top {min(top_k, len(sub))} PCs)', fontsize=10)
        ax.tick_params(axis='y', labelsize=7)
        ax.grid(True, axis='x', alpha=0.3, linewidth=0.5)

    for ax in axes[len(benchmarks):]:
        ax.axis('off')

    fig.suptitle('Per-PC PTE fingerprint (each line = one PC, min-max normalized within benchmark)',
                 fontsize=14, y=0.995)
    fig.tight_layout(rect=[0, 0, 1, 0.98])
    fig.savefig(out_png, dpi=400, bbox_inches='tight')
    plt.close(fig)
    return out_png


def main():
    p = argparse.ArgumentParser(description="Visualize per-PC PTE access characteristics.")
    p.add_argument('xlsx', nargs='?',
                   default=os.path.join(os.path.dirname(__file__),
                                        '20260525-222553',
                                        't_pc_stats_l2miss_pte_per_owner_1.xlsx'))
    p.add_argument('--top-k', type=int, default=12,
                   help='max PCs per benchmark in the parallel-coordinates plot')
    args = p.parse_args()

    df = pd.read_excel(args.xlsx)
    # 일부 셀에 깨진 비숫자 값(예: spmv 'b')이 섞여 있어 강제 숫자 변환.
    for c in list(METRICS) + ['total_access_count']:
        df[c] = pd.to_numeric(df[c], errors='coerce')
    df['total_access_count'] = df['total_access_count'].fillna(1)
    df = df.dropna(subset=['avg_requestor_chiplets', 'avg_access_per_addr_per_owner'])
    benchmarks = sorted(df['benchmark'].unique())

    out_dir = os.path.dirname(os.path.abspath(args.xlsx))
    base = os.path.splitext(os.path.basename(args.xlsx))[0]

    b = plot_bubble(df, benchmarks, os.path.join(out_dir, f'{base}__bubble.png'))
    pc = plot_parallel(df, benchmarks, os.path.join(out_dir, f'{base}__parallel.png'),
                       top_k=args.top_k)
    print(f"Saved:\n  {b}\n  {pc}")


if __name__ == '__main__':
    main()
