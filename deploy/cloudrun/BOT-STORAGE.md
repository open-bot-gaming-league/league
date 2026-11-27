# BOT-STORAGE.md -- submitted bots' files in Cloud Storage

A coach submits a bot as one WebAssembly module (at most 64 MB) and a
folder of data files (at most 16 GB in all): `go run ./sdk/cmd/bot submit`
(poker/engine/sdk/cmd/bot), against the submission API on the table's API
(`mcp/bot_web.py`; the rules are `mcp/bots.py`'s, the storage interface
`mcp/bot_files.py`'s). This is what the service needs to store those files
in production, and why it is built the way it is.

## Why the files go straight to Cloud Storage

Cloud Run caps an HTTP/1 request at 32 MiB ("Maximum HTTP/1 request size:
32 MiB per request; no limit if using HTTP/2 server",
https://cloud.google.com/run/quotas, read 2026-11-27). Even past that cap
(HTTP/2 end to end), 16 GB through the service would hold the one
instance (`--max-instances 1`) for minutes per upload, and an instance's
disk is its memory. So no file passes through the service: for each file,
the server starts a Cloud Storage resumable upload session itself, with
its own credentials and the file's declared size, and hands the client
the session's address. The client PUTs the bytes there in 8 MiB chunks
(a multiple of 256 KiB, as resumable uploads require) and resumes from
the session's offset after a failure, in the same run or a later one (the
session is kept with the submission). This is the pattern Cloud Storage's
docs recommend over signed URLs for resumable uploads
(https://cloud.google.com/storage/docs/access-control/signed-urls, "Using
signed URLs with resumable uploads"), and it needs no signing key.

A session's address is a bearer secret for that one object, for a week
(https://cloud.google.com/storage/docs/resumable-uploads): it is given
only to the account's program, over HTTPS. Nothing a client uploads is
trusted until it is checked: finalize queues the submission, and the
finalize job (Checking submissions) reads every new object back, checks
its size and sha256 against what the submission declared, and checks the
module (`botcheck`). Only then is a file moved from `uploads/` to `bots/`.

## Bucket

One bucket for submitted bots, `tmg-obgl-bots`:

- **Location**: the service's region (`REGION`), Standard class.
- **Access**: uniform bucket-level access; public access prevention enforced.
  Nothing in it is public; the service reads it with its own identity.
- **Versioning**: off.
- **Soft delete**: off (`--soft-delete-duration=0`). Every accepted file is
  written to `uploads/`, copied to `bots/` and deleted from `uploads/`; with
  the default 7-day soft delete each deleted upload would be billed for 7
  more days (up to 16 GB a submission). A deleted bot file is gone at once.
- **Lifecycle**: delete objects with prefix `uploads/` older than 8 days.
  An open submission lasts 7 days (a session's life), and an abandoned one
  leaves its finished uploads there; nothing else ever needs them.

Layout (keys are object names; `mcp/bot_files.py`):

    uploads/<account>/<submission>/<sha256>   as uploaded, not yet checked
    bots/<account>/<sha256>                   checked: what bots run

Files are kept by sha256 per account, so a coach's next version with the
same data uploads only its module, and no account can name another's files.

## Names

The jobs and their accounts are named in their files only:
`finalize-job.yaml` (`FINALIZE_JOB`, its `metadata.name`; `FINALIZE_ACCOUNT`,
its `serviceAccountName`) and `ranked-job.yaml` (`RANKED_JOB`,
`RANKED_ACCOUNT`), and the code finds a job by its environment variable
(`OBGL_FINALIZE_JOB`, `OBGL_RANKED_JOB`). `SERVICE_ACCOUNT` is the service's
(CLOUDRUN.md).

## IAM

The service's account (`SERVICE_ACCOUNT`) gets
`roles/storage.objectAdmin` **on the bucket only**: it starts upload
sessions (object create), looks at objects' sizes, and deletes a replaced
submission's uploads. No `iam.serviceAccountTokenCreator`: there are no
signed URLs. It gets `roles/run.invoker` on `FINALIZE_JOB` (to start it)
and on `RANKED_JOB` (to start it again when a coach looks at a table that
waits with no runner: RANKED-TABLES.md).

The finalize job's account, `FINALIZE_ACCOUNT` (its own, so the ranked
job's stays read-only on the bucket), gets:

- `roles/storage.objectUser` on the bucket: it reads uploads back, copies
  them to `bots/` (rewrite) and deletes them;
- `roles/datastore.user` on the project (the submissions, the queue, the
  account's versions, and the ranked table it queues: Firestore);
- `roles/run.invoker` on `RANKED_JOB`, to start the ranked job when it
  queues a table.

The ranked job's account reads bots' files: `roles/storage.objectViewer` on
the bucket (RANKED-TABLES.md).

    gcloud run jobs add-iam-policy-binding FINALIZE_JOB --project PROJECT --region REGION \
        --member serviceAccount:SERVICE_ACCOUNT --role roles/run.invoker

## Turning it on

The image already has the module check (`/app/arena/bin/botcheck`,
`OBGL_BOTCHECK`). The service's deploy sets:

    OBGL_BOT_STORE=gcs:tmg-obgl-bots
    OBGL_BOT_SUBMITS_PER_HOUR=1    # per account; the default; 0 is no limit
    OBGL_FINALIZE_JOB=projects/PROJECT/locations/REGION/jobs/FINALIZE_JOB

Without `OBGL_BOT_STORE` the submission routes answer 503: that is the
switch that keeps submissions off. Without `OBGL_FINALIZE_JOB` the service
checks submissions itself, between requests: that is for local runs only
(the local stack, tests). In production the service does no checking; the
finalize job does.

## Checking submissions

Finalize answers at once: the submission is queued to be checked
(`oauth_finalize-queue`, state `checking`), and `bot submit` asks after it
(`GET /table/api/bots/submissions/<id>`) until it is `finalized` (the
version, and its ranked table queued) or `rejected` (why), for at most 15
minutes; then it says to look later with `bot status <id>`. The check is
the finalize job's (`finalize-job.yaml`, `python ./mcp/finalize.py run`):
1 vCPU and 2 GiB, scaled to zero between submissions. It checks the
queue's head under a 2-minute lease it renews every 40 s, one submission
at a time, and exits when the queue is empty.

The service starts an execution when it queues a submission, and again
when a coach's program asks after a submission still waiting that no
runner holds, if none was started in the last 2 minutes: a start that
failed, or a runner that died, is made up by the next ask. The longest
check (16 GB read back at the 100 MB/s or more a single stream from Cloud
Storage should give, the module check, the copy to `bots/`) is about 5
minutes; the task's timeout is an hour, and the runner takes no new
submission after 50 minutes (`OBGL_FINALIZE_STOP_AFTER=3000`). A runner
stopped part way loses nothing: the next one checks the submission again,
and after 3 starts it is rejected as interrupted.

Create the job (IMAGE the service's image, the same tag):

    gcloud run jobs replace deploy/cloudrun/finalize-job.yaml --project PROJECT --region REGION

By hand, for a submission that waits:

    gcloud run jobs execute FINALIZE_JOB --project PROJECT --region REGION

The hourly limit (one submission per account per hour) counts each
finalize when it is asked, before the submission is queued: a submission
the check refuses still counts, and does not give its hour back. Over it,
a coach is told "You can submit one bot an hour, refused ones included:
try again at HH:MM UTC", and `bot submit` adds to a refusal that a refused
submission counts toward the hourly limit. It is counted in the account's
record (`oauth_account-bots`, `asked`) with a conditional write, so it
holds with any number of instances. An account has one submission being
checked at a time. The one-open-submission rule is enforced under a lock
in the server's memory, which is sound only with one instance: another
reason to keep `--max-instances 1` (CLOUDRUN.md).

## Deleting an account

`delete_account.py` deletes the account's bot records (`oauth_account-bots`,
`oauth_bot-submissions`) with the rest, and prints the command for its
files:

    gcloud storage rm -r gs://tmg-obgl-bots/bots/ACCOUNT/ gs://tmg-obgl-bots/uploads/ACCOUNT/
