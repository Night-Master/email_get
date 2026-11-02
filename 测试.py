import json

import requests


BASE_URL = "http://127.0.0.1"
ACCOUNTS_URL = f"{BASE_URL}/api/accounts"
MESSAGES_URL = f"{BASE_URL}/api/messages"


accounts_response = requests.get(ACCOUNTS_URL, timeout=10)
accounts_response.raise_for_status()

accounts = accounts_response.json().get("accounts", [])
if not accounts:
    raise SystemExit("no accounts found")

account = accounts[0]
account_id = account["id"]
email = account["email"]

messages_response = requests.get(
    MESSAGES_URL,
    params={
        "accountId": account_id,
        "limit": 5,
    },
    timeout=20,
)
messages_response.raise_for_status()

messages = messages_response.json().get("messages", [])

print(f"account: {email}")
print(json.dumps(messages, indent=2, ensure_ascii=False))
