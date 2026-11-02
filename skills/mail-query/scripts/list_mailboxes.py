#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json

from api_client import ServiceClient


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="通过服务 API 列出当前邮箱账号")
    parser.add_argument("--base-url", default="http://127.0.0.1:8080", help="服务地址")
    parser.add_argument("--password", default="", help="管理密码,默认读 release/auth_password.txt 或 EMAIL_GET_PASSWORD")
    parser.add_argument("--keyword", default="", help="按邮箱/服务器/协议关键词过滤")
    parser.add_argument("--pretty", action="store_true", help="格式化 JSON 输出")
    return parser


def main() -> int:
    args = build_parser().parse_args()
    result = ServiceClient(args.base_url, args.password).get("/api/accounts", {"keyword": args.keyword})
    if args.pretty:
        print(json.dumps(result, ensure_ascii=False, indent=2))
    else:
        print(json.dumps(result, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
