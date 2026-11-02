#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json

from api_client import ServiceClient


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="通过服务 API 触发收件")
    parser.add_argument("--base-url", default="http://127.0.0.1:8080", help="服务地址")
    parser.add_argument("--password", default="", help="管理密码,默认读 release/auth_password.txt 或 EMAIL_GET_PASSWORD")
    parser.add_argument("--email", action="append", dest="emails", default=[], help="指定邮箱，可重复传")
    parser.add_argument("--account-id", action="append", dest="account_ids", default=[], help="指定账号 id，可重复传")
    parser.add_argument("--limit", type=int, default=20, help="每个账号最多抓多少封")
    parser.add_argument("--folders", default="", help="逗号分隔的文件夹,默认 INBOX,Junk")
    parser.add_argument("--all-messages", action="store_true", help="抓最近邮件，不只未读")
    parser.add_argument("--mark-seen", action="store_true", help="IMAP 下抓取后标记已读")
    parser.add_argument("--pretty", action="store_true", help="格式化 JSON 输出")
    return parser


def main() -> int:
    args = build_parser().parse_args()
    payload = {
        "emails": args.emails,
        "accountIds": args.account_ids,
        "limit": max(1, args.limit),
        "allMessages": args.all_messages,
        "markSeen": args.mark_seen,
    }
    if args.folders.strip():
        payload["folders"] = [f.strip() for f in args.folders.split(",") if f.strip()]
    result = ServiceClient(args.base_url, args.password).post("/api/fetch", payload)
    if args.pretty:
        print(json.dumps(result, ensure_ascii=False, indent=2))
    else:
        print(json.dumps(result, ensure_ascii=False))
    failures = [item for item in result.get("results", []) if item.get("error")]
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
