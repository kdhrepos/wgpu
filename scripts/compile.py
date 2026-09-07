#!/usr/bin/python3

import os
import glob
import argparse
import subprocess
from concurrent.futures import ThreadPoolExecutor, as_completed

# Fixed compile parallelism (was len(benchmarks)).
MAX_PARALLEL = 20

base_dir = os.path.dirname(os.path.abspath(__file__))
samples_path = os.path.abspath(
    os.path.join(base_dir, "..", "mgpusim", "samples"))
# Tool binaries (currently just cmd/footprint) — same "package main" build as
# samples/, but not a benchmark, so kept in its own discovery/build pass.
cmd_path = os.path.abspath(
    os.path.join(base_dir, "..", "mgpusim", "cmd"))
# The worktree this compile.py lives in, then the main workspace as fallback.
worktree_root = os.path.dirname(base_dir)
main_tree = os.path.expanduser(os.path.join("~", "mgvm"))
# Platform preferred by flag-less auto-detection (primary platform).
DEFAULT_PGO_PLATFORM = "mesh"


def _is_main_package_dir(full):
    # The main file may be named main.go or <name>.go, so detect by
    # `package main` rather than a fixed filename.
    for go_file in glob.glob(os.path.join(full, "*.go")):
        if go_file.endswith("_test.go"):
            continue
        with open(go_file, encoding="utf-8", errors="ignore") as f:
            for line in f:
                if line.startswith("package main"):
                    return True
    return False


def discover_sample_dirs():
    # Compile ALL benchmarks: every samples/<name> that is a Go main package.
    dirs = []
    for name in sorted(os.listdir(samples_path)):
        full = os.path.join(samples_path, name)
        if os.path.isdir(full) and _is_main_package_dir(full):
            dirs.append(name)
    return dirs


def discover_cmd_dirs():
    # Tool binaries under mgpusim/cmd/<name>, same detection as samples.
    if not os.path.isdir(cmd_path):
        return []
    dirs = []
    for name in sorted(os.listdir(cmd_path)):
        full = os.path.join(cmd_path, name)
        if os.path.isdir(full) and _is_main_package_dir(full):
            dirs.append(name)
    return dirs


def dir_has_profile(d):
    return any(os.path.getsize(p) > 0
               for p in glob.glob(os.path.join(d, "*.prof")))


def _profile_dirs(pdir, size):
    # cpu-prof dirs (holding >=1 non-empty .prof) under a platform dir.
    if size:
        cand = os.path.join(pdir, "cpu-prof-" + size)
        return [cand] if os.path.isdir(cand) and dir_has_profile(cand) else []
    return sorted(g for g in glob.glob(os.path.join(pdir, "cpu-prof-*"))
                  if os.path.isdir(g) and dir_has_profile(g))


def autodetect_pgo_dir(platform, size, root_override):
    # Resolve <root>/slurm/<platform>/cpu-prof-<size>. Search the current
    # worktree first, then the main workspace. size=None auto-detects when a
    # single cpu-prof-* dir exists.
    roots = [root_override] if root_override else [worktree_root, main_tree]
    for root in roots:
        ds = _profile_dirs(os.path.join(root, "slurm", platform), size)
        if len(ds) == 1:
            return ds[0], root
        if len(ds) > 1:
            sizes = [os.path.basename(d).replace("cpu-prof-", "") for d in ds]
            raise SystemExit(
                f"Multiple profile sizes for {platform}: {sizes}. "
                f"Pick one with --pgo-size.")
    return None, None


def autodetect_any_pgo_dir(size, root_override):
    # Flag-less auto: prefer DEFAULT_PGO_PLATFORM, else the sole platform that
    # has profiles. Worktree first, then the main workspace.
    roots = [root_override] if root_override else [worktree_root, main_tree]
    for root in roots:
        slurm = os.path.join(root, "slurm")
        if not os.path.isdir(slurm):
            continue
        pref = _profile_dirs(os.path.join(slurm, DEFAULT_PGO_PLATFORM), size)
        if len(pref) == 1:
            return pref[0], root, DEFAULT_PGO_PLATFORM
        found = []
        for plat in sorted(os.listdir(slurm)):
            pd = _profile_dirs(os.path.join(slurm, plat), size)
            if len(pd) == 1:
                found.append((plat, pd[0]))
        if len(found) == 1:
            return found[0][1], root, found[0][0]
        if len(found) > 1:
            plats = [p for p, _ in found]
            print(f"PGO auto: several platforms have profiles under {slurm}: "
                  f"{plats}. Pass --pgo-platform to pick one.")
            return None, root, None
    return None, None, None


def resolve_pgo(bench_dir, pgo):
    # pgo is None, a profile file, or a directory holding per-bench profiles.
    if not pgo:
        return None
    if os.path.isfile(pgo):
        return os.path.abspath(pgo)                      # one profile for all
    if os.path.isdir(pgo):
        cands = sorted(glob.glob(os.path.join(pgo, bench_dir + "_*.prof")))
        cands += sorted(glob.glob(os.path.join(pgo, bench_dir + ".prof")))
        for c in cands:
            if os.path.getsize(c) > 0:                   # skip 0-byte (failed)
                return os.path.abspath(c)
    return None                                          # no profile -> no PGO


def build_env(goamd64):
    env = os.environ.copy()
    if goamd64:
        env["GOAMD64"] = goamd64
    return env


def compile_one(bench_dir, pgo, goamd64):
    full_path = os.path.join(samples_path, bench_dir)
    cmd = "go build"
    prof = resolve_pgo(bench_dir, pgo)
    if prof:
        cmd += " -pgo=" + prof
    fp = open(os.devnull, 'w')
    p = subprocess.Popen(cmd, shell=True, cwd=full_path,
                         stdout=fp, stderr=fp, env=build_env(goamd64))
    p.wait()
    tag = "PGO" if prof else "no-pgo"
    if p.returncode == 0:
        print(f"Compiled {full_path} [{tag}]", 'success')
        return False
    print(f"Compile failed {full_path} [{tag}]", 'fail')
    return True


def compile_tool(name, goamd64):
    # Tool binaries (mgpusim/cmd/<name>) are unrelated to any benchmark's hot
    # loop, so no PGO profile applies here.
    full_path = os.path.join(cmd_path, name)
    fp = open(os.devnull, 'w')
    p = subprocess.Popen("go build", shell=True, cwd=full_path,
                         stdout=fp, stderr=fp, env=build_env(goamd64))
    p.wait()
    if p.returncode == 0:
        print(f"Compiled {full_path}", 'success')
        return False
    print(f"Compile failed {full_path}", 'fail')
    return True


def parse_args():
    ap = argparse.ArgumentParser(description="Build all benchmark samples.")
    ap.add_argument(
        "--pgo", default=None,
        help="Explicit PGO source: a single .prof (all benchmarks) or a "
             "directory with <bench>_*.prof (per-benchmark). Overrides "
             "--pgo-platform.")
    ap.add_argument(
        "--pgo-platform", default=None,
        help="Auto-detect PGO profiles from <worktree>/slurm/<platform>/"
             "cpu-prof-<size>/ (falls back to ~/mgvm/slurm/...). e.g. mesh.")
    ap.add_argument(
        "--pgo-size", default=None,
        help="Grid size for --pgo-platform (e.g. 6x6). Auto-detected when a "
             "single cpu-prof-* dir exists.")
    ap.add_argument(
        "--pgo-root", default=None,
        help="Override the workspace root searched for profiles "
             "(default: this worktree, then ~/mgvm).")
    ap.add_argument(
        "--no-pgo", action="store_true",
        help="Disable PGO entirely (skip flag-less auto-detection).")
    ap.add_argument(
        "--goamd64", default=None,
        choices=["v1", "v2", "v3", "v4"],
        help="Target x86-64 microarchitecture level (e.g. v3 = AVX2/BMI2/FMA).")
    return ap.parse_args()


def resolve_pgo_source(args):
    if args.no_pgo:                                      # explicit off
        return None
    if args.pgo:                                         # explicit path wins
        return args.pgo
    if args.pgo_platform:                                # explicit platform
        pgo_dir, root = autodetect_pgo_dir(
            args.pgo_platform, args.pgo_size, args.pgo_root)
        if pgo_dir:
            print(f"PGO auto: {pgo_dir}  (root={root})")
        else:
            print(f"PGO auto: no profiles for platform "
                  f"'{args.pgo_platform}' -> building without PGO.")
        return pgo_dir
    # flag-less: auto-detect platform + dir from worktree/main tree
    pgo_dir, root, plat = autodetect_any_pgo_dir(args.pgo_size, args.pgo_root)
    if pgo_dir:
        print(f"PGO auto (default): platform={plat}  {pgo_dir}  (root={root})")
    else:
        print("PGO auto (default): no profiles found -> building without PGO. "
              "(--pgo-platform to pick, --no-pgo to silence)")
    return pgo_dir


def main():
    args = parse_args()
    pgo = resolve_pgo_source(args)
    sample_dirs = discover_sample_dirs()
    cmd_dirs = discover_cmd_dirs()
    opts = []
    if pgo:
        opts.append(f"pgo={pgo}")
    if args.goamd64:
        opts.append(f"GOAMD64={args.goamd64}")
    print(f"Compiling {len(sample_dirs)} benchmarks "
          f"(parallelism={MAX_PARALLEL}) {' '.join(opts)}".rstrip())

    err = False
    with ThreadPoolExecutor(max_workers=MAX_PARALLEL) as executor:
        futures = {
            executor.submit(compile_one, d, pgo, args.goamd64): d
            for d in sample_dirs}
        for future in as_completed(futures):
            err |= future.result()

    if cmd_dirs:
        print(f"Compiling {len(cmd_dirs)} tool binaries: {', '.join(cmd_dirs)}")
        with ThreadPoolExecutor(max_workers=MAX_PARALLEL) as executor:
            futures = {
                executor.submit(compile_tool, d, args.goamd64): d
                for d in cmd_dirs}
            for future in as_completed(futures):
                err |= future.result()

    if err:
        print("Some benchmarks failed to compile")
    else:
        print("All benchmarks compiled successfully")


if __name__ == '__main__':
    main()
