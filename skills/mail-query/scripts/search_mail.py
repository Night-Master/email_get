#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json

from api_client import ServiceClient


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="通过服务 API 按条件远端搜索邮件")
    parser.add_argument("--base-url", default="http://127.0.0.1:8080", help="服务地址")
    parser.add_argument("--password", default="", help="管理密码,默认读 release/auth_password.txt 或 EMAIL_GET_PASSWORD")
    parser.add_argument("--email", action="append", dest="emails", default=[], help="指定邮箱，可重复传")
    parser.add_argument("--account-id", action="append", dest="account_ids", default=[], help="指定账号 id，可重复传")
    parser.add_argument("--recent-seconds", type=int, default=86400, help="最近多少秒内，默认 86400")
    parser.add_argument("--keyword", default="", help="关键词")
    parser.add_argument("--regex", default="", help="正则表达式")
    parser.add_argument("--fields", nargs="*", default=["all"], help="all/subject/from/body")
    parser.add_argument("--limit", type=int, default=20, help="返回多少条")
    parser.add_argument("--scan-limit", type=int, default=20, help="每个账号最多扫描多少封候选邮件")
    parser.add_argument("--folders", default="", help="逗号分隔的文件夹,默认 INBOX,Junk")
    parser.add_argument("--pretty", action="store_true", help="格式化 JSON 输出")
    return parser


def main() -> int:
    args = build_parser().parse_args()
    payload = {
        "emails": args.emails,
        "accountIds": args.account_ids,
        "recentSeconds": max(0, args.recent_seconds),
        "keyword": args.keyword,
        "regex": args.regex,
        "fields": args.fields,
        "limit": max(1, args.limit),
        "scanLimit": max(1, args.scan_limit),
    }
    if args.folders.strip():
        payload["folders"] = [f.strip() for f in args.folders.split(",") if f.strip()]
    result = ServiceClient(args.base_url, args.password).post("/api/search", payload)
    if args.pretty:
        print(json.dumps(result, ensure_ascii=False, indent=2))
    else:
        print(json.dumps(result, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
