# -*- coding: utf-8 -*-
"""归一化行组数超 int16 上限的源 parquet 文件。

背景:旧湖 2026 年部分文件按"单只股票单日"写行组(>32767 个),parquet 规范的
RowGroup.ordinal 为 int16,溢出后 parquet-go 会拒绝打开。此脚本把这些文件
按 (code, datetime) 重新排序并用正常行组大小重写,输出到暂存目录,
再由 quantd migrate 从暂存目录迁移。

用法:
    python tools/normalize_source.py --src <旧湖根> --staging <暂存根>
"""

from __future__ import annotations

import argparse
import glob
import os
import sys

import pyarrow.parquet as pq

ROW_GROUP_ROWS = 131072


def find_bad(lake_root: str) -> list[str]:
    bad = []
    for market in ("hs", "bj"):
        pattern = os.path.join(lake_root, "bars_1m", f"market={market}", "**", "*.parquet")
        for path in sorted(glob.glob(pattern, recursive=True)):
            n = pq.ParquetFile(path).metadata.num_row_groups
            if n > 32767:
                bad.append(path)
    return bad


def normalize(src: str, dst: str) -> None:
    table = pq.read_table(src, columns=None)
    table = table.sort_by([("code", "ascending"), ("datetime", "ascending")])
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    pq.write_table(table, dst, compression="zstd", row_group_size=ROW_GROUP_ROWS)
    del table


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--src", required=True, help="旧湖根目录")
    parser.add_argument("--staging", required=True, help="归一化输出根目录")
    parser.add_argument("--limit", type=int, default=0, help="只处理前 N 个文件(调试用)")
    args = parser.parse_args()

    bad = find_bad(args.src)
    if args.limit:
        bad = bad[: args.limit]
    print(f"to normalize: {len(bad)} files")
    for path in bad:
        rel = os.path.relpath(path, args.src)
        dst = os.path.join(args.staging, rel)
        if os.path.exists(dst):
            src_rows = pq.ParquetFile(path).metadata.num_rows
            dst_rows = pq.ParquetFile(dst).metadata.num_rows
            if src_rows == dst_rows:
                print(f"skip (already normalized): {rel}", flush=True)
                continue
        print(f"normalizing {rel} ...", flush=True)
        normalize(path, dst)
        print(f"  -> {dst} ({os.path.getsize(dst)/1e6:.0f} MB)", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
