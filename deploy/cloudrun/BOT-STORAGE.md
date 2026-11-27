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
trusted until finalize: the server reads every new object back, checks
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

## IAM

The service account (`SERVICE_ACCOUNT`, CLOUDRUN.md) gets
`roles/storage.objectAdmin` **on the bucket only**: it starts upload
sessions (object create), reads objects back, copies them (rewrite) and
deletes them. No `iam.serviceAccountTokenCreator`: there are no signed
URLs. The arena's runner, when it reads bots' files, needs
`roles/storage.objectViewer` on the same bucket.

## Turning it on

The image already has the module check (`/app/arena/bin/botcheck`,
`OBGL_BOTCHECK`). The deploy sets:

    OBGL_BOT_STORE=gcs:tmg-obgl-bots
    OBGL_BOT_SUBMITS_PER_HOUR=1    # per account; the default; 0 is no limit

and raises the request timeout (`gcloud run deploy ... --timeout 900`).
Finalize is one request that reads every new file back to hash it: 16 GB
at the 100-200 MB/s a single stream from Cloud Storage should give is
80-170 s, before the module check; the default 300 s is too close.
Without `OBGL_BOT_STORE` the submission routes answer 503.

The hourly limit (one finalized submission per account per hour; over
it a coach is told "You can submit one bot an hour: try again at HH:MM
UTC") is checked in the account's record (`oauth_account-bots`) with a
conditional write, so it holds with any number of instances. The
one-open-submission rule is enforced under a lock in the server's memory,
which is sound only with one instance: another reason to keep
`--max-instances 1` (CLOUDRUN.md).

## Deleting an account

`delete_account.py` deletes the account's bot records (`oauth_account-bots`,
`oauth_bot-submissions`) with the rest, and prints the command for its
files:

    gcloud storage rm -r gs://tmg-obgl-bots/bots/ACCOUNT/ gs://tmg-obgl-bots/uploads/ACCOUNT/
