#!/usr/bin/env python3
"""delete_account.py: delete one account's records from the OBGL Firestore
database (CLOUDRUN.md, "Deleting an account"). A dry run unless --delete.

  delete_account.py ACCOUNT_ID --project PROJECT [--delete]

ACCOUNT_ID is "github:<id>" or "google:<sub>" (or "guest:<random>", for an
account left from before guest sign-in was dropped). Run it with
Application Default Credentials for PROJECT, or with FIRESTORE_EMULATOR_HOST
set. It lists the documents that hold the account's data, one per line, then
deletes them only with --delete. Exit 1 if it finds no account record.

It reads whole collections (the store keeps `record` as one unindexed JSON
string, so there is no query): fine at this scale, not a thing to run in a loop.
It does not touch the game server's hands file (CLOUDRUN.md, "Session state").
"""

import argparse
import hashlib
import json
import sys

from google.cloud import firestore


def find(db, account):
    """[(collection, document id)] holding `account`'s data."""
    refs = [(c, account) for c in ("oauth_accounts", "oauth_account-tables", "oauth_account-programs")
            if db.collection(c).document(account).get().exists]
    for collection, field in (("table-sessions", "owner"), ("access", "subject"), ("refresh", "subject"), ("codes", "subject"),
                              ("program-tokens", "account"), ("program-usage", "account")):
        for doc in db.collection(f"oauth_{collection}").stream():
            record = json.loads(doc.get("record"))
            if record.get(field) != account:
                continue
            refs.append((f"oauth_{collection}", doc.id))
            if collection == "table-sessions" and "watch" in record:  # an old watch token: keyed by its sha256
                refs.append(("oauth_watch-tokens", hashlib.sha256(record["watch"].encode()).hexdigest()))
    return refs


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("account")
    ap.add_argument("--project", required=True)
    ap.add_argument("--delete", action="store_true", help="delete what is listed (default: list only)")
    args = ap.parse_args()
    db = firestore.Client(project=args.project)
    refs = find(db, args.account)
    found = ("oauth_accounts", args.account) in refs
    if not found:
        print(f"no account record {args.account!r} in {args.project}", file=sys.stderr)
    for collection, doc_id in refs:
        print(f"{'delete' if args.delete else 'would delete'} {collection}/{doc_id}")
    if args.delete:
        for collection, doc_id in refs:
            db.collection(collection).document(doc_id).delete()
    return 0 if found else 1


if __name__ == "__main__":
    sys.exit(main())
