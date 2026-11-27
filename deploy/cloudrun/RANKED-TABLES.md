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

- **The finalize job** (`finalize-job.yaml`; BOT-STORAGE.md) queues: when it
  files a version it writes the table to Firestore (`oauth_ranked-tables`), appends it to the queue
  (`oauth_ranked-queue/queue`) and to the account's list
  (`oauth_account-ranked`), and starts the ranked job (Running it).
- **The service** (obgl-bot-poker) serves the results from the records
  bucket, mounted read-only: `OBGL_RECORDS=/records` makes the table server
  read `/records/<id>.json` (entrypoint.sh's `-records`).
- **The job** (`ranked-job.yaml`) plays: the same image, run as
  `python ./mcp/ranked.py run`. It plays the queue's head under a lease it
  renews every 40 s, writes the record to the records bucket, marks the
  table done or failed, takes it off the queue, and goes on until the queue
  is empty; then it exits. One table at a time, 8 pairs at once.

The queue lives in Firestore, so a job that dies loses nothing: the next
execution finds the head running with its lease (2 minutes) run out and
plays it again, from the same seed. After 3 starts a table fails as
interrupted. Each
write that two processes could make at once (the queue, the account's
list) is a conditional write that retries (`AuthStore.update`).

## Badges

A finished table earns its coach badges by the bot's place among the
eleven: a top 5, a top 3, and a gold for a win (a win earns all three).
Each is worth points (by default 1, 3 and 10); points are summed per
season, and badges are kept across seasons. The job awards a table's
badges, once, before it marks the table done (`oauth_account-badges`, by
account: `mcp/badges.py`); the service shows them (`GET /table/api/badges`,
the table page's Ranked tables, the end of `table_results`). The season and
the points are settings, the same on both: `OBGL_SEASON` (default
`2026-S1`) and `OBGL_BADGE_POINTS` (`win=10,top-3=3,top-5=1`). A badge keeps
the points and season it was awarded with: a new value counts from the next
table. There is no leaderboard.

## Buckets

- **Bots** (BOT-STORAGE.md's): the job mounts it read-only at `/bots`
  (Cloud Storage FUSE): a bot reads its data on demand, nothing is copied
  to the job's memory-backed disk. The runner checks each file's size
  there before it plays.
- **Records**, new: `RECORDS_BUCKET` (its name is the deploy's: the
  service's volume and ranked-job.yaml), the service's region, Standard
  class, uniform access, public access prevention, versioning off, soft
  delete on (the default, 7 days). One object per finished table,
  `<id>.json` (20-60 KB). The job mounts it read-write at `/records`; the
  service read-only at `/records`. Records are kept until deleted (no
  lifecycle). A deleted record (an account's deletion, or the temporary
  object each record write renames) can be restored for 7 days and is
  billed for them.

## IAM

Names are as in BOT-STORAGE.md (Names): `RANKED_JOB` and `RANKED_ACCOUNT`
are `ranked-job.yaml`'s, `FINALIZE_ACCOUNT` is `finalize-job.yaml`'s,
`SERVICE_ACCOUNT` the service's.

A dedicated service account for the job, `RANKED_ACCOUNT`:

- `roles/datastore.user` on the project (the queue, the tables, the bots'
  versions in Firestore);
- `roles/storage.objectViewer` on the bots bucket;
- `roles/storage.objectUser` on the records bucket (a record is written to
  a temporary object and renamed: create and delete).

The service's account gets `roles/storage.objectViewer` on the records
bucket. Both the finalize job's account and the service's, which start
the job (Running it), get `roles/run.invoker` on the job only:

    gcloud run jobs add-iam-policy-binding RANKED_JOB --project PROJECT --region REGION \
        --member serviceAccount:FINALIZE_ACCOUNT --role roles/run.invoker
    gcloud run jobs add-iam-policy-binding RANKED_JOB --project PROJECT --region REGION \
        --member serviceAccount:SERVICE_ACCOUNT --role roles/run.invoker

## Create it

    gcloud run jobs replace deploy/cloudrun/ranked-job.yaml --project PROJECT --region REGION

with IMAGE the service's image (the same tag: the job's `ranked.py` and
the service's must agree on the queue), and the service deployed with

    --add-volume name=records,type=cloud-storage,bucket=RECORDS_BUCKET,readonly=true
    --add-volume-mount volume=records,mount-path=/records
    --set-env-vars OBGL_RECORDS=/records,OBGL_RANKED_JOB=projects/PROJECT/locations/REGION/jobs/RANKED_JOB

(Cloud Storage volumes need `--execution-environment gen2` on the
service too.)

## Running it

The ranked job is started, with the Run Admin API's `jobs.run`, by whoever
has `OBGL_RANKED_JOB` (`projects/PROJECT/locations/REGION/jobs/RANKED_JOB`)
and `roles/run.invoker` on `RANKED_JOB` (IAM):

- the finalize job (finalize-job.yaml sets it), when it queues a table;
- the service, when a coach looks at their tables (`GET /table/api/tables`,
  the table page, `table_results`) and one of theirs is queued.

Either starts an execution only if no runner holds the queue's head under
a live lease and none was started in the last 2 minutes; a runner holding the head plays on to the end of the
queue. So a start that fails (logged: `ranked: a table is queued, but its
runner did not start`, with the API's answer) or a runner that died is
made up by the next table queued or the next look, and the table stays
queued meanwhile. Without `OBGL_RANKED_JOB` nothing starts the job and
every start is by hand. The service refuses to start with it and no bot
submissions, or with `OBGL_RANKED_RUNNER` (the job is the runner).

By hand:

    gcloud run jobs execute RANKED_JOB --project PROJECT --region REGION

An execution started while another runs sees the head's live lease and
exits; the running one plays the rest of the queue.

## Cost

A table's compute is in its record (`measured`); `rankedtable cost
record.json` prices it at the Jobs price (league.CloudRunJobs). Most of a
table's cost is the Dove seats the seed draws (Dove spends about ten times
the other house bots' time a decision).

## Deleting an account

`delete_account.py` deletes the account's tables (`oauth_ranked-tables`),
its list (`oauth_account-ranked`) and its badges (`oauth_account-badges`),
and prints the command that removes their records (`--records-bucket
RECORDS_BUCKET`, or `OBGL_RECORDS_BUCKET`): a record names its owner's
account (`seats[0].owner.account`). The runner skips a deleted
table left in the queue. The results page does not show the account.
