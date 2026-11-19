#!/usr/bin/python3

import argparse
import glob
import os
import subprocess
from concurrent.futures import ThreadPoolExecutor, as_completed

SAMPLES = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                       "..", "mgpusim", "samples")


def is_main_package(d):
    for f in glob.glob(os.path.join(d, "*.go")):
        if f.endswith("_test.go"):
            continue
        with open(f, encoding="utf-8", errors="ignore") as fp:
            if any(line.startswith("package main") for line in fp):
                return True
    return False


def find_profile(name, pgo):
    if not pgo:
        return None
    if os.path.isfile(pgo):
        return os.path.abspath(pgo)
    for c in sorted(glob.glob(os.path.join(pgo, name + "*.prof"))):
        if os.path.getsize(c) > 0:
            return os.path.abspath(c)
    return None


def build(name, pgo, env):
    cmd = ["go", "build"]
    prof = find_profile(name, pgo)
    if prof:
        cmd.append("-pgo=" + prof)
    r = subprocess.run(cmd, cwd=os.path.join(SAMPLES, name), env=env,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    print(f"{'ok  ' if r.returncode == 0 else 'FAIL'} {name}"
          f"{' [pgo]' if prof else ''}")
    return r.returncode == 0


def main():
    ap = argparse.ArgumentParser(description="Build all benchmark samples.")
    ap.add_argument("-j", type=int, default=20, help="parallel builds")
    ap.add_argument("--pgo", help="a .prof file, or a dir of <bench>*.prof")
    ap.add_argument("--goamd64", choices=["v1", "v2", "v3", "v4"])
    args = ap.parse_args()

    env = os.environ.copy()
    if args.goamd64:
        env["GOAMD64"] = args.goamd64

    names = sorted(n for n in os.listdir(SAMPLES)
                   if is_main_package(os.path.join(SAMPLES, n)))
    with ThreadPoolExecutor(max_workers=args.j) as ex:
        futs = [ex.submit(build, n, args.pgo, env) for n in names]
        failed = sum(not f.result() for f in as_completed(futs))

    print(f"{len(names) - failed}/{len(names)} built")
    raise SystemExit(1 if failed else 0)


if __name__ == "__main__":
    main()
