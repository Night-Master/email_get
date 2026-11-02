---
name: mail-query
description: Query and fetch mail for a specific mailbox by calling the running mail service HTTP API. Use this when the user wants the agent to list available mailboxes, trigger fetch for specific email addresses, or search remote mail by keyword/regex/recent time through the service instead of connecting to IMAP or POP3 directly.
---

# Mail Query

Use the scripts in `skills/mail-query/scripts/`. All operations must go through the running mail service HTTP API.

## When to use

- The user wants the agent to list which mailboxes are currently available from the running service
- The user wants the agent to trigger fetch for one or more email addresses through the running service
- The user wants the agent to search remote mail by keyword, regex, or recent time window through the running service
- The user wants mailbox-specific actions without clicking around in the web UI

## Default behavior

- `protocol`: `auto`
- `recent-seconds`: `86400`
- `fields`: `all`
- `limit`: `20`
- `scan-limit`: `20`
- `folders`: `INBOX,Junk` (server-side default; verification codes often land in Junk. Override with `--folders INBOX` etc.)

`auto` tries IMAP first, then POP3. POP3 has no folder concept and ignores `folders`.

## Required input

- A running mail service, default `http://127.0.0.1:8080`
- The target mailbox should already exist in the service account list if you plan to use `--email`
- The service requires login. Password resolution order: `--password` argument > `EMAIL_GET_PASSWORD` env var > `release/auth_password.txt` next to the binary

## Scripts

- `scripts/list_mailboxes.py`: list mailbox accounts exposed by the service
- `scripts/fetch_mail.py`: trigger mailbox fetch through the service
- `scripts/search_mail.py`: search remote mail through the service

## Commands

List current mailboxes from the service:

```bash
python3 skills/mail-query/scripts/list_mailboxes.py \
  --pretty
```

Filter current mailboxes:

```bash
python3 skills/mail-query/scripts/list_mailboxes.py \
  --keyword outlook \
  --pretty
```

Trigger fetch by email:

```bash
python3 skills/mail-query/scripts/fetch_mail.py \
  --email 'user@example.com' \
  --pretty
```

Search remote mail by email:

```bash
python3 skills/mail-query/scripts/search_mail.py \
  --email 'user@example.com' \
  --keyword '領取獎勵' \
  --pretty
```

Regex query:

```bash
python3 skills/mail-query/scripts/search_mail.py \
  --email 'user@example.com' \
  --regex '验证码|verification code' \
  --fields subject body \
  --recent-seconds 3600 \
  --pretty
```

## Notes

- The scripts do not log in to IMAP or POP3 directly
- The service handles protocol selection, cached endpoints, errors, and mailbox state
- Prefer `--email` for agent workflows because it matches the service API directly
- If the script exits non-zero, inspect the returned JSON for `error` fields
- HTTP 401 means the password is missing or wrong; pass `--password` or set `EMAIL_GET_PASSWORD`
