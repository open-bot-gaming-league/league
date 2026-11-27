#!/usr/bin/env python3
"""delete_account.py: delete one account's records from the Open Bot Gaming League's Firestore
database (CLOUDRUN.md, "Deleting an account"). A dry run unless --delete.

  delete_account.py ACCOUNT_ID --project PROJECT [--delete]

ACCOUNT_ID is "github:<id>" or "google:<sub>" (or "guest:<random>", for an
account left from before guest sign-in was dropped). Run it with
Application Default Credentials for PROJECT, or with FIRESTORE_EMULATOR_HOST
set. It lists the documents that hold the account's data, one per line, then
deletes them only with --delete. Exit 1 if it finds no account record.

It reads whole collections (the store keeps `record` as one unindexed JSON
string, so there is no query): fine at this scale, not a thing to run in a loop.
It does not touch the game server's hands file (CLOUDRUN.md, "Session state"),
nor the account's submitted bots' files and ranked tables' records in Cloud
Storage: it prints the commands that remove those (BOT-STORAGE.md and
RANKED-TABLES.md, "Deleting an account"). A deleted table still in the
ranked queue is skipped by the runner.
"""

import argparse
import hashlib
import json
import sys

from google.cloud import firestore


def find(db, account):
    """[(collection, document id)] holding `account`'s data."""
    refs = [(c, account) for c in ("oauth_accounts", "oauth_account-tables", "oauth_account-programs", "oauth_account-bots",
                                   "oauth_account-ranked", "oauth_account-badges")
            if db.collection(c).document(account).get().exists]
    for collection, field in (("table-sessions", "owner"), ("access", "subject"), ("refresh", "subject"), ("codes", "subject"),
                              ("program-tokens", "account"), ("program-usage", "account"), ("bot-submissions", "account"),
                              ("ranked-tables", "account")):
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
    if any(c in ("oauth_account-bots", "oauth_bot-submissions") for c, _ in refs):
        print(f"and its bots' files: gcloud storage rm -r gs://BUCKET/bots/{args.account}/ gs://BUCKET/uploads/{args.account}/",
              file=sys.stderr)
    records = [doc_id for c, doc_id in refs if c == "oauth_ranked-tables"]
    if records:
        # A ranked table's record names its owner's account (RANKED-TABLES.md).
        print("and its ranked tables' records: gcloud storage rm "
              + " ".join(f"gs://RECORDS_BUCKET/{t}.json" for t in records), file=sys.stderr)
    if args.delete:
        for collection, doc_id in refs:
            db.collection(collection).document(doc_id).delete()
    return 0 if found else 1


if __name__ == "__main__":
    sys.exit(main())
