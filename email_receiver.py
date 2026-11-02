#!/usr/bin/env python3
from __future__ import annotations

import argparse
import email
import html
import imaplib
import json
import re
import sys
import time
import urllib.parse
import urllib.request
from dataclasses import dataclass
from email.header import decode_header
from email.message import Message
from pathlib import Path
from typing import Iterable


DEFAULT_IMAP_PORT = 993
DEFAULT_FOLDER = "INBOX"


@dataclass
class Account:
    email_address: str
    password: str
    imap_server: str
    imap_port: int = DEFAULT_IMAP_PORT
    client_id: str | None = None
    refresh_token: str | None = None


def decode_mime(value: str | None) -> str:
    if not value:
        return ""
    parts = []
    for chunk, encoding in decode_header(value):
        if isinstance(chunk, bytes):
            parts.append(chunk.decode(encoding or "utf-8", errors="replace"))
        else:
            parts.append(chunk)
    return "".join(parts).strip()


def sanitize_filename(value: str) -> str:
    value = value.strip() or "mail"
    value = re.sub(r"[^\w.\-]+", "_", value, flags=re.ASCII)
    return value[:120]


def guess_imap_server(email_address: str) -> str:
    domain = email_address.split("@", 1)[-1].lower()
    known = {
        "gmail.com": "imap.gmail.com",
        "outlook.com": "outlook.office365.com",
        "hotmail.com": "outlook.office365.com",
        "live.com": "outlook.office365.com",
        "work9.cn": "imap.hotmuil.com",
        "hotmuil.com": "imap.hotmuil.com",
        "qq.com": "imap.qq.com",
        "163.com": "imap.163.com",
        "126.com": "imap.126.com",
        "yeah.net": "imap.yeah.net",
    }
    return known.get(domain, f"imap.{domain}")


def parse_account_line(line: str) -> Account | None:
    raw = line.strip()
    if not raw or raw.startswith("#"):
        return None

    parts = [part.strip() for part in raw.split("----") if part.strip()]
    if len(parts) < 2:
        raise ValueError(f"账号行格式不对: {line!r}")

    email_address = parts[0]
    password = parts[1]

    if len(parts) == 4 and "." in parts[2]:
        imap_server = parts[2]
        try:
            imap_port = int(parts[3])
        except ValueError as exc:
            raise ValueError(f"端口不是数字: {line!r}") from exc
        return Account(email_address=email_address, password=password, imap_server=imap_server, imap_port=imap_port)

    if len(parts) == 4 and re.fullmatch(r"[0-9a-fA-F-]{36}", parts[2]):
        return Account(
            email_address=email_address,
            password=password,
            imap_server=guess_imap_server(email_address),
            client_id=parts[2],
            refresh_token=parts[3],
        )

    if len(parts) > 2:
        raise ValueError("不支持的账号格式，请使用 `邮箱----密码`、`邮箱----密码----IMAP服务器----端口` 或 Outlook 四段 OAuth 格式。")

    return Account(email_address=email_address, password=password, imap_server=guess_imap_server(email_address))


def load_accounts(path: Path) -> list[Account]:
    accounts = []
    for idx, line in enumerate(path.read_text(encoding="utf-8").splitlines(), start=1):
        try:
            account = parse_account_line(line)
        except ValueError as exc:
            raise ValueError(f"{path}:{idx}: {exc}") from exc
        if account:
            accounts.append(account)
    if not accounts:
        raise ValueError(f"没有读取到任何账号: {path}")
    return accounts


def html_to_text(content: str) -> str:
    text = re.sub(r"(?is)<(script|style).*?>.*?</\1>", "", content)
    text = re.sub(r"(?i)<br\s*/?>", "\n", text)
    text = re.sub(r"(?i)</p>", "\n", text)
    text = re.sub(r"(?s)<.*?>", "", text)
    text = html.unescape(text)
    return re.sub(r"\n{3,}", "\n\n", text).strip()


def extract_body(message: Message) -> str:
    if message.is_multipart():
        text_chunks = []
        html_chunks = []
        for part in message.walk():
            content_disposition = (part.get("Content-Disposition") or "").lower()
            if "attachment" in content_disposition:
                continue
            payload = part.get_payload(decode=True)
            if payload is None:
                continue
            charset = part.get_content_charset() or "utf-8"
            decoded = payload.decode(charset, errors="replace")
            content_type = part.get_content_type()
            if content_type == "text/plain":
                text_chunks.append(decoded.strip())
            elif content_type == "text/html":
                html_chunks.append(html_to_text(decoded))
        body = "\n\n".join(chunk for chunk in text_chunks if chunk).strip()
        if body:
            return body
        return "\n\n".join(chunk for chunk in html_chunks if chunk).strip()

    payload = message.get_payload(decode=True)
    if payload is None:
        return ""
    charset = message.get_content_charset() or "utf-8"
    decoded = payload.decode(charset, errors="replace")
    if message.get_content_type() == "text/html":
        return html_to_text(decoded)
    return decoded.strip()


def save_attachments(message: Message, target_dir: Path) -> list[str]:
    saved = []
    for part in message.walk():
        content_disposition = (part.get("Content-Disposition") or "").lower()
        if "attachment" not in content_disposition:
            continue

        filename = decode_mime(part.get_filename()) or "attachment.bin"
        filename = sanitize_filename(filename)
        payload = part.get_payload(decode=True)
        if payload is None:
            continue
        file_path = target_dir / filename
        file_path.write_bytes(payload)
        saved.append(str(file_path))
    return saved


def iter_target_ids(mail: imaplib.IMAP4_SSL, only_unseen: bool, limit: int) -> Iterable[bytes]:
    criterion = "UNSEEN" if only_unseen else "ALL"
    status, data = mail.search(None, criterion)
    if status != "OK":
        raise RuntimeError(f"搜索邮件失败: {status}")
    ids = data[0].split()
    return ids[-limit:]


def request_outlook_access_token(account: Account) -> str:
    if not account.client_id or not account.refresh_token:
        raise RuntimeError("缺少 Outlook OAuth 所需的 client_id 或 refresh_token")

    token_endpoints = [
        "https://login.microsoftonline.com/consumers/oauth2/v2.0/token",
        "https://login.microsoftonline.com/common/oauth2/v2.0/token",
    ]
    last_error = None
    for url in token_endpoints:
        data = urllib.parse.urlencode(
            {
                "client_id": account.client_id,
                "grant_type": "refresh_token",
                "refresh_token": account.refresh_token,
                "scope": "https://outlook.office.com/IMAP.AccessAsUser.All offline_access",
            }
        ).encode()
        req = urllib.request.Request(url, data=data, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                payload = json.loads(resp.read().decode())
            access_token = payload.get("access_token")
            if access_token:
                return access_token
            last_error = payload
        except Exception as exc:
            last_error = exc
    raise RuntimeError(f"获取 Outlook access token 失败: {last_error}")


def login_imap(mail: imaplib.IMAP4_SSL, account: Account) -> None:
    if account.client_id and account.refresh_token:
        access_token = request_outlook_access_token(account)
        auth_string = f"user={account.email_address}\x01auth=Bearer {access_token}\x01\x01"
        mail.authenticate("XOAUTH2", lambda _: auth_string.encode())
        return
    mail.login(account.email_address, account.password)


def print_message_preview(account: Account, mail_id: bytes, sender: str, subject: str, date_text: str, body: str) -> None:
    preview = re.sub(r"\s+", " ", body).strip()[:200]
    print(f"[MAIL] {account.email_address} id={mail_id.decode()} from={sender or '-'}")
    print(f"       subject={subject or '-'}")
    print(f"       date={date_text or '-'}")
    print(f"       body={preview or '-'}")


def fetch_account(
    account: Account,
    output_dir: Path,
    folder: str,
    only_unseen: bool,
    limit: int,
    mark_seen: bool,
) -> int:
    account_dir = output_dir / sanitize_filename(account.email_address)
    account_dir.mkdir(parents=True, exist_ok=True)

    mail = imaplib.IMAP4_SSL(account.imap_server, account.imap_port)
    try:
        login_imap(mail, account)
        status, _ = mail.select(folder)
        if status != "OK":
            raise RuntimeError(f"无法打开文件夹 {folder!r}")

        count = 0
        for mail_id in iter_target_ids(mail, only_unseen=only_unseen, limit=limit):
            fetch_flags = "(RFC822 FLAGS)" if mark_seen else "(BODY.PEEK[] FLAGS)"
            status, data = mail.fetch(mail_id, fetch_flags)
            if status != "OK" or not data or not data[0]:
                continue

            raw_message = data[0][1]
            if not raw_message:
                continue

            message = email.message_from_bytes(raw_message)
            subject = decode_mime(message.get("Subject")) or "no_subject"
            sender = decode_mime(message.get("From"))
            date_text = decode_mime(message.get("Date"))
            body = extract_body(message)

            message_dir = account_dir / f"{mail_id.decode()}_{sanitize_filename(subject)}"
            message_dir.mkdir(parents=True, exist_ok=True)

            summary = [
                f"Account: {account.email_address}",
                f"From: {sender}",
                f"Subject: {subject}",
                f"Date: {date_text}",
                "",
                body,
            ]
            (message_dir / "message.txt").write_text("\n".join(summary).strip() + "\n", encoding="utf-8")
            save_attachments(message, message_dir)
            print_message_preview(account, mail_id, sender, subject, date_text, body)
            count += 1

        return count
    finally:
        try:
            mail.close()
        except Exception:
            pass
        try:
            mail.logout()
        except Exception:
            pass


def run_once(args: argparse.Namespace) -> int:
    accounts = load_accounts(Path(args.accounts))
    output_dir = Path(args.output)
    output_dir.mkdir(parents=True, exist_ok=True)

    total = 0
    for account in accounts:
        try:
            count = fetch_account(
                account=account,
                output_dir=output_dir,
                folder=args.folder,
                only_unseen=not args.all_messages,
                limit=args.limit,
                mark_seen=args.mark_seen,
            )
        except imaplib.IMAP4.error as exc:
            print(f"[LOGIN FAILED] {account.email_address}: {exc}", file=sys.stderr)
            continue
        except Exception as exc:
            print(f"[ERROR] {account.email_address}: {exc}", file=sys.stderr)
            continue

        print(f"[OK] {account.email_address}: 保存 {count} 封邮件")
        total += count

    return total


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="批量 IMAP 收件脚本")
    parser.add_argument("--accounts", default="accounts.txt", help="账号文件路径")
    parser.add_argument("--output", default="downloads", help="邮件保存目录")
    parser.add_argument("--folder", default=DEFAULT_FOLDER, help="邮箱文件夹，默认 INBOX")
    parser.add_argument("--limit", type=int, default=10, help="每个账号最多抓取多少封")
    parser.add_argument("--all-messages", action="store_true", help="抓取全部邮件，不只未读")
    parser.add_argument("--mark-seen", action="store_true", help="抓取后标记为已读")
    parser.add_argument("--poll-interval", type=int, default=0, help="轮询秒数，0 表示只跑一次")
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()

    if args.limit <= 0:
        parser.error("--limit 必须大于 0")
    if args.poll_interval < 0:
        parser.error("--poll-interval 不能小于 0")

    if args.poll_interval == 0:
        run_once(args)
        return 0

    while True:
        print(time.strftime("[%Y-%m-%d %H:%M:%S] 开始检查邮件"))
        run_once(args)
        print(f"等待 {args.poll_interval} 秒后继续...")
        time.sleep(args.poll_interval)


if __name__ == "__main__":
    raise SystemExit(main())
