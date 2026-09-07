#!/usr/bin/python3
"""워크트리 하나를 지정해, 선택한 벤치마크들을 빌드 + 로컬 실행으로 한 번에 돌리는
가벼운 테스트 러너.

slurm 은 쓰지 않는다 (sbatch heredoc per job 대신 로컬 Popen per job). functional
-verify 는 켜지 않고, exit code 로 "크래시 없이 돌았는지"만 판정한다. 시뮬레이터
내부는 단일 스레드 (-parallel 미사용).

사용 예:
    python3 scripts/test_worktree.py crazy-cannon-aa7fe7
    python3 scripts/test_worktree.py crazy-cannon-aa7fe7 --jobs 3 --max-inst 10000000
    python3 scripts/test_worktree.py /abs/path/to/worktree --dry-run

벤치마크 목록은 scripts/test_benchmarks.txt (--list 로 변경) 에서 읽는다.
각 벤치의 워크로드 args 는 *대상 워크트리* 의 scripts/params_small.py (없으면
params.py) 의 benchmark_configs 에서 재사용한다.
"""

import os
import sys
import time
import argparse
import importlib.util
import subprocess
from concurrent.futures import ThreadPoolExecutor

# run.py 의 base_options 미러링. -scheduling=lasp 등 공통 플래그.
BASE_OPTIONS = (
    " -timing"
    " -no-progress-bar"
    " -report-all"
    " -mem-allocator-type=lasp"
    " -use-lasp-mem-alloc"
    " -scheduling=lasp"
)

USERNAME = os.path.expanduser("~")
WORKTREES_ROOT = os.path.join(USERNAME, "mgvm", ".claude", "worktrees")
DEFAULT_LIST = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                            "test_benchmarks.txt")


def resolve_worktree(name_or_path):
    """워크트리 이름(.claude/worktrees/<name>) 또는 직접 경로를 절대경로로 해석하고
    git work tree 인지 검증한다."""
    candidates = []
    if os.path.isabs(name_or_path) or os.sep in name_or_path:
        candidates.append(os.path.abspath(name_or_path))
    else:
        candidates.append(os.path.join(WORKTREES_ROOT, name_or_path))
        candidates.append(os.path.abspath(name_or_path))

    for path in candidates:
        if os.path.isdir(path):
            r = subprocess.run(
                ["git", "-C", path, "rev-parse", "--is-inside-work-tree"],
                capture_output=True, text=True)
            if r.returncode == 0 and r.stdout.strip() == "true":
                return path
    sys.exit(f"[error] worktree 를 찾을 수 없음: {name_or_path}\n"
             f"        확인한 경로: {', '.join(candidates)}")


def load_benchmark_configs(worktree):
    """대상 워크트리의 params_small.py (없으면 params.py) 를 동적 import 해서
    benchmark_configs 를 돌려준다."""
    scripts_dir = os.path.join(worktree, "scripts")
    for fname in ("params_small.py", "params.py"):
        fpath = os.path.join(scripts_dir, fname)
        if not os.path.isfile(fpath):
            continue
        spec = importlib.util.spec_from_file_location(
            f"_wt_params_{abs(hash(worktree))}", fpath)
        mod = importlib.util.module_from_spec(spec)
        # params 모듈이 옆 모듈을 import 할 수 있으니 scripts_dir 를 경로에 넣어줌.
        sys.path.insert(0, scripts_dir)
        try:
            spec.loader.exec_module(mod)
        finally:
            sys.path.remove(scripts_dir)
        return getattr(mod, "benchmark_configs", {}), fname
    sys.exit(f"[error] {scripts_dir} 에 params_small.py / params.py 가 없음")


def read_benchmark_list(list_path):
    if not os.path.isfile(list_path):
        sys.exit(f"[error] 벤치마크 목록 파일이 없음: {list_path}")
    names = []
    with open(list_path) as f:
        for line in f:
            line = line.split("#", 1)[0].strip()
            if line:
                names.append(line)
    if not names:
        sys.exit(f"[error] {list_path} 에 선택된 벤치마크가 없음")
    return names


def resolve_sample_dir(bench, cfg):
    # compile.py 와 동일한 별칭 해석: 'name' 이 있으면 그 디렉터리/바이너리.
    return cfg.get("name", bench)


def build_command(worktree, bench, cfg, args):
    """run.py 의 명령 조립을 테스트용으로 축약 미러링. -max-inst 는 CLI 값으로 강제."""
    binary = resolve_sample_dir(bench, cfg)
    alias = cfg.get("alias", binary)
    binary_path = os.path.join(worktree, "mgpusim", "samples", binary, binary)

    cmd = [binary_path]
    cmd += BASE_OPTIONS.split()
    cmd += [
        f"-benchmark={alias}",
        f"-platform-type={args.platform}",
        f"-xDim={args.xDim}",
        f"-yDim={args.yDim}",
        "-sched-partition", cfg.get("sched-partition", "Xdiv"),
        "-data-partitioning", cfg.get("data-partitioning", "kernel-wide"),
        "-data-strides", str(cfg.get("data-strides", "1")),
        "-col-size-for-lasp", str(cfg.get("col-size-for-lasp", 0)),
    ]
    if cfg.get("params"):
        cmd += cfg["params"].split()
    # 퀵 테스트 보장: config 의 max_inst 무시하고 CLI 상한 강제.
    cmd += [f"-max-inst={args.max_inst}"]
    return binary, binary_path, cmd


def build_samples(worktree, sample_dirs, jobs, dry_run):
    """선택된 벤치의 유니크 샘플 디렉터리에서 go build (compile.py 패턴)."""
    results = {}

    def _build(sample_dir):
        cwd = os.path.join(worktree, "mgpusim", "samples", sample_dir)
        if not os.path.isdir(cwd):
            return sample_dir, False, f"샘플 디렉터리 없음: {cwd}"
        if dry_run:
            return sample_dir, True, "(dry-run: go build 생략)"
        p = subprocess.run("go build", shell=True, cwd=cwd,
                           capture_output=True, text=True)
        ok = p.returncode == 0
        return sample_dir, ok, (p.stderr.strip() if not ok else "")

    with ThreadPoolExecutor(max_workers=jobs) as ex:
        for sample_dir, ok, msg in ex.map(_build, sample_dirs):
            results[sample_dir] = (ok, msg)
            tag = "ok" if ok else "FAIL"
            line = f"[build] {sample_dir:24s} {tag}"
            if msg:
                line += f"  {msg.splitlines()[0] if msg else ''}"
            print(line)
    return results


def run_one(job):
    bench, cmd, out_path, err_path, dry_run = job
    if dry_run:
        return bench, None, 0.0
    start = time.time()
    with open(out_path, "w") as out, open(err_path, "w") as err:
        p = subprocess.run(cmd, stdout=out, stderr=err)
    return bench, p.returncode, time.time() - start


def main():
    ap = argparse.ArgumentParser(description="워크트리 벤치마크 테스트 러너")
    ap.add_argument("worktree", help="워크트리 이름(.claude/worktrees/<name>) 또는 경로")
    ap.add_argument("--list", default=DEFAULT_LIST, help="벤치마크 목록 파일")
    ap.add_argument("--jobs", type=int, default=4, help="동시 실행/빌드 수")
    ap.add_argument("--max-inst", type=int, default=30_000_000,
                    help="퀵 실행 명령어 상한 (-max-inst)")
    ap.add_argument("--platform", default="mesh")
    ap.add_argument("--xDim", type=int, default=4)
    ap.add_argument("--yDim", type=int, default=4)
    ap.add_argument("--dry-run", action="store_true",
                    help="빌드/실행 없이 해석 결과와 명령줄만 출력")
    args = ap.parse_args()

    worktree = resolve_worktree(args.worktree)
    wt_name = os.path.basename(worktree.rstrip(os.sep))
    configs, params_file = load_benchmark_configs(worktree)
    benches = read_benchmark_list(args.list)

    print(f"worktree : {worktree}")
    print(f"params   : scripts/{params_file}")
    print(f"benches  : {', '.join(benches)}")
    print(f"max-inst : {args.max_inst}   jobs: {args.jobs}   "
          f"platform: {args.platform} {args.xDim}x{args.yDim}")
    print()

    # 알 수 없는 벤치 거르기.
    unknown = [b for b in benches if b not in configs]
    if unknown:
        print(f"[warn] params 에 없는 벤치 (건너뜀): {', '.join(unknown)}\n")
    benches = [b for b in benches if b in configs]
    if not benches:
        sys.exit("[error] 실행할 벤치가 없음")

    # 스크래치 출력 디렉터리.
    ts = time.strftime("%Y%m%d-%H%M%S")
    out_dir = os.path.join(USERNAME, "mgvm", ".test-runs", wt_name, ts)
    if not args.dry_run:
        os.makedirs(out_dir, exist_ok=True)

    # 명령 구성.
    commands = {}
    sample_dirs = []
    for b in benches:
        cfg = configs[b]
        sample_dir, binary_path, cmd = build_command(worktree, b, cfg, args)
        commands[b] = (sample_dir, binary_path, cmd)
        if sample_dir not in sample_dirs:
            sample_dirs.append(sample_dir)

    if args.dry_run:
        print("== dry-run: 실행될 명령 ==")
        for b in benches:
            _, _, cmd = commands[b]
            print(f"\n# {b}\n{' '.join(cmd)}")
        return

    # Pass 1: 빌드.
    print("== Pass 1: build ==")
    build_results = build_samples(worktree, sample_dirs, args.jobs, args.dry_run)
    print()

    # Pass 2: 실행 (job dispatch).
    print("== Pass 2: run ==")
    jobs = []
    skipped = {}
    for b in benches:
        sample_dir, _, cmd = commands[b]
        ok, _ = build_results.get(sample_dir, (False, "빌드 결과 없음"))
        if not ok:
            skipped[b] = "BUILD FAIL"
            continue
        out_path = os.path.join(out_dir, f"{b}.out")
        err_path = os.path.join(out_dir, f"{b}.err")
        jobs.append((b, cmd, out_path, err_path, args.dry_run))

    run_results = {}
    with ThreadPoolExecutor(max_workers=args.jobs) as ex:
        for bench, rc, wall in ex.map(run_one, jobs):
            run_results[bench] = (rc, wall)
            print(f"[run] {bench:24s} exit={rc}  {wall:6.1f}s")

    # 요약.
    print("\n== summary ==")
    print(f"{'bench':24s} {'build':6s} {'run':8s} {'wall':>8s}  log")
    any_fail = False
    for b in benches:
        sample_dir, _, _ = commands[b]
        b_ok, _ = build_results.get(sample_dir, (False, ""))
        build_tag = "ok" if b_ok else "FAIL"
        if b in skipped:
            run_tag, wall_tag, log = "-", "-", ""
            any_fail = True
        else:
            rc, wall = run_results.get(b, (None, 0.0))
            run_tag = f"exit={rc}"
            wall_tag = f"{wall:.1f}s"
            log = os.path.join(out_dir, f"{b}.out")
            if rc != 0:
                any_fail = True
        print(f"{b:24s} {build_tag:6s} {run_tag:8s} {wall_tag:>8s}  {log}")

    print(f"\nlogs: {out_dir}")
    sys.exit(1 if any_fail else 0)


if __name__ == "__main__":
    main()
