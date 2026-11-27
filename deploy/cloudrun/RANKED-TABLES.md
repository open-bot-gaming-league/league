# RANKED-TABLES.md -- playing submitted bots' ranked tables in the cloud

Every bot version a coach finalizes (`go run ./sdk/cmd/bot submit`;
BOT-STORAGE.md) queues a ranked table: the bot and ten seats drawn from the
five house bots (copies allowed), every pair heads-up on a million duplicate
deals, every seat run as wasm in the arena sandbox. `mcp/ranked.py` has the
queue and the runner; `poker/arena/cmd/rankedtable` plays the table. The
coach sees the table in `GET /table/api/tables` and on the table page (Ranked
tables), and reads the result at `/table/results/<id>` or from the MCP tool
`table_results` (no id: their newest table).

## How it is split

- **The service** (obgl-bot-poker) only queues: finalize writes the table
  to Firestore (`oauth_ranked-tables`), appends it to the queue
  (`oauth_ranked-queue/queue`) and to the account's list
  (`oauth_account-ranked`). It serves the results from the records bucket,
  mounted read-only: `OBGL_RECORDS=/records` makes the table server read
  `/records/<id>.json` (entrypoint.sh's `-records`).
- **The job** (`ranked-job.yaml`, obgl-ranked) plays: the same image, run as
  `python ./mcp/ranked.py run`. It plays the queue's head under a lease it
  renews every 40 s, writes the record to the records bucket, marks the
  table done or failed, takes it off the queue, and goes on until the queue
  is empty; then it exits. One table at a time, 8 pairs at once.

The queue lives in Firestore, so a job that dies loses nothing: the next
execution finds the head running with its lease (2 minutes) run out and
plays it again, from the same seed. After 3 starts a table fails as
interrupted (so a bot that kills the runner cannot hold the queue). Each
write that two processes could make at once (the queue, the account's
list) is a conditional write that retries (`AuthStore.update`).

## Buckets

- **Bots** (BOT-STORAGE.md's): the job mounts it read-only at `/bots`
  (Cloud Storage FUSE): a bot reads its data on demand, nothing is copied
  to the job's memory-backed disk. The runner checks each file's size
  there before it plays.
- **Records**, new: e.g. `PROJECT-obgl-records`, the service's region,
  Standard class, uniform access, public access prevention, versioning
  off, soft delete off. One object per finished table, `<id>.json`
  (20-60 KB). The job mounts it read-write at `/records`; the service
  read-only at `/records`. Records are kept until deleted (no lifecycle).

## IAM

A dedicated service account for the job, `RANKED_SERVICE_ACCOUNT`:

- `roles/datastore.user` on the project (the queue, the tables, the bots'
  versions in Firestore);
- `roles/storage.objectViewer` on the bots bucket;
- `roles/storage.objectUser` on the records bucket (a record is written to
  a temporary object and renamed: create and delete).

The service's account gets `roles/storage.objectViewer` on the records
bucket.

## Create it

    gcloud run jobs replace deploy/cloudrun/ranked-job.yaml --project PROJECT --region REGION

with IMAGE the service's image (the same tag: the job's `ranked.py` and
the service's must agree on the queue), and the service deployed with

    --add-volume name=records,type=cloud-storage,bucket=RECORDS_BUCKET,readonly=true
    --add-volume-mount volume=records,mount-path=/records
    --set-env-vars OBGL_RECORDS=/records

(Cloud Storage volumes need `--execution-environment gen2` on the
service too.)

## Running it

The job is started by hand. A table waits in the queue (the coach sees it
queued, with its place in line) until an execution runs:

    gcloud run jobs execute obgl-ranked --project PROJECT --region REGION

An execution started while another runs sees the head's live lease and
exits; the running one plays the rest of the queue. An execution that
starts within 2 minutes of one that died finds the dead one's lease still
live and exits; the table plays at the next start.

## Cost

A table's compute is in its record (`measured`); `rankedtable cost
record.json` prices it at the Jobs price (league.CloudRunJobs). Most of a
table's cost is the Dove seats the seed draws (Dove spends about ten times
the other house bots' time a decision).

## Deleting an account

`delete_account.py` deletes the account's tables (`oauth_ranked-tables`)
and its list (`oauth_account-ranked`), and prints the command that removes
their records: a record names its owner's account
(`seats[0].owner.account`). The runner skips a deleted table left in the
queue. The results page does not show the account.
