"""在 NAS 上执行命令(地址见 .env 的 QUANT_NAS_HOST),避免 PowerShell 转义问题。

用法:
    python harness/scripts/remote_exec.py "shell command"
    python harness/scripts/remote_exec.py --sudo "systemctl restart docker"
    python harness/scripts/remote_exec.py --file path/to/script.sh

环境变量(或写入项目根 .env,该文件不入库):
    QUANT_NAS_HOST  NAS 地址(必填)
    QUANT_NAS_USER  SSH 用户(必填)
    QUANT_NAS_KEY   私钥路径(默认 %USERPROFILE%\\.ssh\\id_rsa)
    QUANT_SUDO_PASS 使用 --sudo 时必需(不落盘、不入库)
"""

from __future__ import annotations

import argparse
import os
import sys
import warnings

warnings.filterwarnings("ignore")

import paramiko

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from _nasenv import nas_host, nas_user  # noqa: E402

HOST = nas_host()
PORT = 22
USER = nas_user()
KEY_PATH = os.environ.get("QUANT_NAS_KEY", os.path.expanduser("~/.ssh/id_rsa"))
SUDO_PASS = os.environ.get("QUANT_SUDO_PASS", "")


def connect() -> paramiko.SSHClient:
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(HOST, port=PORT, username=USER, key_filename=KEY_PATH, timeout=15, allow_agent=False, look_for_keys=False)
    return client


def quote(text: str) -> str:
    return "'" + text.replace("'", "'\"'\"'") + "'"


def run(client: paramiko.SSHClient, command: str, use_sudo: bool = False) -> int:
    if use_sudo:
        if not SUDO_PASS:
            print("error: --sudo 需要设置环境变量 QUANT_SUDO_PASS", file=sys.stderr)
            return 2
        command = "echo '%s' | sudo -S bash -c %s" % (SUDO_PASS, quote(command))
    _stdin, stdout, stderr = client.exec_command(command, timeout=600)
    out = stdout.read().decode("utf-8", "replace")
    err = stderr.read().decode("utf-8", "replace")
    sys.stdout.write(out)
    if err.strip():
        sys.stderr.write(err)
    return stdout.channel.recv_exit_status()


def main() -> int:
    parser = argparse.ArgumentParser(description="在 NAS 上执行命令")
    parser.add_argument("command", nargs="?", help="要执行的 shell 命令")
    parser.add_argument("--sudo", action="store_true", help="以 sudo 执行(需要 QUANT_SUDO_PASS)")
    parser.add_argument("--file", help="执行本地脚本(上传到 /tmp 后 bash 执行)")
    args = parser.parse_args()
    if not args.command and not args.file:
        parser.print_help()
        return 2

    client = connect()
    try:
        if args.file:
            with open(args.file, "r", encoding="utf-8") as fh:
                script = fh.read()
            remote = "/tmp/remote_exec_%d.sh" % os.getpid()
            sftp = client.open_sftp()
            try:
                with sftp.open(remote, "w") as fh:
                    fh.write(script)
            finally:
                sftp.close()
            code = run(client, "bash %s" % remote, use_sudo=args.sudo)
            run(client, "rm -f %s" % remote)
            return code
        return run(client, args.command, use_sudo=args.sudo)
    finally:
        client.close()


if __name__ == "__main__":
    raise SystemExit(main())
