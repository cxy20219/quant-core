"""NAS 连接参数:优先读环境变量,其次读项目根 .env(该文件不入库)。

用法:
    from _nasenv import nas_host, nas_user, nas_url
    host = nas_host()          # 未配置时返回空串,调用方给出明确报错
"""

from __future__ import annotations

import os
from pathlib import Path


def load_env() -> None:
    """把项目根 .env 载入 os.environ(不覆盖已存在的变量)。"""
    root = Path(__file__).resolve().parents[2]
    env_file = root / ".env"
    if not env_file.exists():
        return
    try:
        text = env_file.read_text(encoding="utf-8")
    except OSError:
        return
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        key = key.strip()
        value = value.strip().strip('"').strip("'")
        if key:
            os.environ.setdefault(key, value)


def nas_host() -> str:
    load_env()
    return os.environ.get("QUANT_NAS_HOST", "").strip()


def nas_user() -> str:
    load_env()
    return os.environ.get("QUANT_NAS_USER", "").strip()


def nas_url() -> str:
    """服务地址:QUANT_NAS_URL 优先;否则由 host 拼出 http://host:8000。"""
    load_env()
    url = os.environ.get("QUANT_NAS_URL", "").strip()
    if url:
        return url.rstrip("/")
    host = nas_host()
    return f"http://{host}:8000" if host else ""
