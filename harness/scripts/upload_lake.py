"""上传目录树到 NAS(断点续传:按文件大小跳过已传文件)。

用法:
    python harness/scripts/upload_lake.py <local_root> <remote_root> [--workers 6] [--check]

环境变量(均有默认值):
    QUANT_NAS_HOST  NAS 地址(必填,或写入 .env)
    QUANT_NAS_USER  默认 <nas-user>
    QUANT_NAS_KEY   默认 ~/.ssh/id_rsa
"""

from __future__ import annotations

import argparse
import concurrent.futures as futures
import os
import sys
import threading
import time
import warnings

warnings.filterwarnings("ignore")

import paramiko

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _nasenv import nas_host  # noqa: E402

HOST = nas_host()
USER = os.environ.get("QUANT_NAS_USER", "<nas-user>")
KEY = os.environ.get("QUANT_NAS_KEY", os.path.expanduser("~/.ssh/id_rsa"))


def mkdirs(sftp: paramiko.SFTPClient, path: str) -> None:
    parts = path.strip("/").split("/")
    cur = ""
    for part in parts:
        cur = f"{cur}/{part}" if cur else f"/{part}"
        try:
            sftp.stat(cur)
        except OSError:
            try:
                sftp.mkdir(cur)
            except OSError:
                pass


def collect(local_root: str, remote_root: str):
    out = []
    for dirpath, _dirnames, filenames in os.walk(local_root):
        rel = os.path.relpath(dirpath, local_root).replace("\\", "/")
        remote_dir = remote_root if rel == "." else f"{remote_root}/{rel}"
        for name in filenames:
            local = os.path.join(dirpath, name)
            out.append((local, f"{remote_dir}/{name}"))
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description="上传目录树到 NAS(断点续传)")
    parser.add_argument("local_root", help="本地目录")
    parser.add_argument("remote_root", help="远端目录(自动创建)")
    parser.add_argument("--workers", type=int, default=6, help="并行传输数")
    parser.add_argument("--check", action="store_true", help="只统计待传文件,不上传")
    args = parser.parse_args()

    files = collect(args.local_root, args.remote_root)
    total = sum(os.path.getsize(f) for f, _ in files)
    print(f"{len(files)} files, {total/1e9:.2f} GB -> {args.remote_root}")

    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(HOST, username=USER, key_filename=KEY, timeout=15, allow_agent=False, look_for_keys=False)

    dirs = sorted({remote.rsplit("/", 1)[0] for _, remote in files})
    with client.open_sftp() as sftp:
        for d in dirs:
            mkdirs(sftp, d)

    pending = []
    skipped = 0
    skipped_bytes = 0
    with client.open_sftp() as sftp:
        for local, remote in files:
            size = os.path.getsize(local)
            try:
                if sftp.stat(remote).st_size == size:
                    skipped += 1
                    skipped_bytes += size
                    continue
            except OSError:
                pass
            pending.append((local, remote, size))
    print(f"already uploaded: {skipped} files ({skipped_bytes/1e9:.2f} GB); "
          f"pending: {len(pending)} files ({sum(s for _, _, s in pending)/1e9:.2f} GB)")
    if args.check or not pending:
        client.close()
        return 0

    lock = threading.Lock()
    done = {"n": 0, "bytes": 0}
    started = time.time()

    def put(item):
        local, remote, size = item
        for attempt in range(3):
            try:
                with client.open_sftp() as sftp:
                    sftp.put(local, remote)
                break
            except Exception:  # noqa: BLE001
                if attempt == 2:
                    raise
                time.sleep(2 + attempt * 3)
        with lock:
            done["n"] += 1
            done["bytes"] += size
            if done["n"] % 25 == 0 or done["n"] == len(pending):
                rate = done["bytes"] / max(1e-6, time.time() - started) / 1e6
                print(f"  {done['n']}/{len(pending)} files, {done['bytes']/1e9:.2f} GB, {rate:.0f} MB/s", flush=True)

    try:
        with futures.ThreadPoolExecutor(max_workers=args.workers) as pool:
            list(pool.map(put, pending))
    finally:
        client.close()
    print(f"upload complete in {(time.time()-started)/60:.1f} min")
    return 0


if __name__ == "__main__":
    sys.exit(main())
