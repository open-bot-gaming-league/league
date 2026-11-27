#!/usr/bin/env python3
"""image.py: build the runtime image's filesystem on this box, from the
repo's own Dockerfile (there is no docker here).

The Dockerfile stays the source of truth: this reads it and performs its
instructions against directories instead of layers, so a change to the
image (a new COPY, a new ENV) shows up locally without anyone editing a
second script. It understands only the instructions the Dockerfile uses
today -- FROM, ARG, ENV, WORKDIR, COPY (incl. --from=<stage>), ENTRYPOINT,
and RUN for exactly three commands (go build [-buildmode=...], pip install -r, chmod) -- and
FAILS on anything else rather than guess. When the Dockerfile grows
something new, extend this file (and deploy/local/README.md).

  image.py --context DIR --out DIR --venvs DIR [--build-arg K=V]...

--context  the build context (a checkout of the sha; the repo root)
--out      where the stage filesystems are made: OUT/<stage>/<container path>
--venvs    cache of Python venvs, keyed by the sha256 of requirements.txt
Writes OUT/image.env, a shell fragment run.sh sources: the image's ENV, a
PATH with the venv first (the image's `python`), IMAGE_WORKDIR, and
IMAGE_ENTRYPOINT.
"""

import argparse
import glob
import hashlib
import json
import os
import re
import shlex
import shutil
import subprocess
import sys


def die(msg):
    sys.exit(f"image.py: {msg}")


def logical_lines(text):
    """Dockerfile instructions: comments dropped, backslash continuations joined."""
    out, cur = [], ""
    for raw in text.splitlines():
        line = raw.strip()
        if not cur and (not line or line.startswith("#")):
            continue
        if line.endswith("\\"):
            cur += line[:-1] + " "
            continue
        out.append((cur + line).strip())
        cur = ""
    if cur:
        out.append(cur.strip())
    return out


class Stage:
    def __init__(self, name, root):
        self.name, self.root, self.workdir = name, root, "/"
        self.env = {}
        os.makedirs(root, exist_ok=True)

    def path(self, p):
        """A container path (absolute, or relative to WORKDIR) -> a host path."""
        if not p.startswith("/"):
            p = os.path.join(self.workdir, p)
        return os.path.join(self.root, os.path.normpath(p).lstrip("/"))


class Image:
    def __init__(self, context, out, venvs, build_args):
        self.context, self.out, self.venvs = context, out, venvs
        self.args = dict(build_args)  # values passed on the command line
        self.stages = {}
        self.cur = None
        self.entrypoint = None
        self.venv_bin = None
        self.arg_vars = {}

    def expand(self, s):
        stage_env = {**self.arg_vars, **(self.cur.env if self.cur else {})}
        return re.sub(r"\$\{?(\w+)\}?", lambda m: stage_env.get(m.group(1), ""), s)

    # --- instructions -------------------------------------------------
    def FROM(self, rest):
        m = re.fullmatch(r"(\S+)(?:\s+AS\s+(\S+))?", rest, re.I)
        if not m:
            die(f"cannot parse FROM {rest!r}")
        name = m.group(2) or f"stage{len(self.stages)}"
        self.cur = Stage(name, os.path.join(self.out, name))
        self.stages[name] = self.cur
        self.arg_vars = {}

    def ARG(self, rest):
        k, _, default = rest.partition("=")
        self.arg_vars[k] = self.args.pop(k, default)

    def ENV(self, rest):
        for tok in shlex.split(rest):
            k, eq, v = tok.partition("=")
            if not eq:
                die(f"ENV without '=': {rest!r}")
            self.cur.env[k] = self.expand(v)

    def WORKDIR(self, rest):
        self.cur.workdir = os.path.normpath(os.path.join(self.cur.workdir, rest))
        os.makedirs(self.cur.path("."), exist_ok=True)

    def ENTRYPOINT(self, rest):
        self.entrypoint = json.loads(rest)
        self.entry_stage = self.cur

    def COPY(self, rest):
        toks = shlex.split(rest)
        src_root = None
        while toks and toks[0].startswith("--"):
            flag = toks.pop(0)
            if not flag.startswith("--from="):
                die(f"unsupported COPY flag {flag}")
            src_root = self.stages[flag[len("--from="):]]
        *srcs, dest = toks
        if not srcs:
            die(f"COPY needs a source and a destination: {rest!r}")
        dest_is_dir = dest.endswith("/") or len(srcs) > 1
        dest_host = self.cur.path(dest)
        for src in srcs:
            if src_root is None:
                pattern = os.path.join(self.context, src)
            else:
                pattern = src_root.path(src)
            matches = sorted(glob.glob(pattern))
            if not matches:
                die(f"COPY source matches nothing: {src!r} ({pattern})")
            for m in matches:
                dest_is_dir_here = dest_is_dir or len(matches) > 1
                if os.path.isdir(m) and not os.path.islink(m):
                    # Docker copies a directory's CONTENTS into the destination.
                    # Symlinks stay symlinks (the staged bot data is symlinks into
                    # the shared data dir; copying 850 MB per sha is the one
                    # deliberate difference from `docker build`).
                    shutil.copytree(m, dest_host, symlinks=True, dirs_exist_ok=True)
                else:
                    target = os.path.join(dest_host, os.path.basename(m)) if dest_is_dir_here else dest_host
                    os.makedirs(os.path.dirname(target), exist_ok=True)
                    shutil.copy2(m, target, follow_symlinks=False)

    def RUN(self, rest):
        st = self.cur
        toks = shlex.split(rest)
        if toks[:2] == ["go", "build"]:
            flags = [t for t in toks[2:-3] if t.startswith("-buildmode=")]
            if toks[-3] != "-o" or len(toks) != 5 + len(flags) or flags != toks[2:-3]:
                die(f"unsupported go build form: {rest!r}")
            env = dict(os.environ, GOTOOLCHAIN="local")  # what the golang image sets
            env.update(st.env)
            out = st.path(toks[-2])
            os.makedirs(os.path.dirname(out), exist_ok=True)
            self.sh(["go", "build", *flags, "-o", out, toks[-1]], cwd=st.path("."), env=env)
        elif toks[:2] == ["pip", "install"]:
            if toks[2:4] != ["--no-cache-dir", "-r"] or len(toks) != 5:
                die(f"unsupported pip install form: {rest!r}")
            req = st.path(toks[4])
            h = hashlib.sha256(open(req, "rb").read()).hexdigest()[:16]
            venv = os.path.join(self.venvs, h)
            if not os.path.exists(os.path.join(venv, ".installed")):
                if os.path.exists(venv):
                    die(f"{venv} is a half-made venv (no .installed marker): remove it and rerun")
                self.sh([sys.executable, "-m", "venv", venv])
                self.sh([os.path.join(venv, "bin", "pip"), "install", "--no-cache-dir", "--progress-bar", "off", "-r", req])
                open(os.path.join(venv, ".installed"), "w").close()
            self.venv_bin = os.path.join(venv, "bin")
        elif toks[:2] == ["chmod", "+x"]:
            for p in toks[2:]:
                host = st.path(p)
                os.chmod(host, os.stat(host).st_mode | 0o111)
        else:
            die(f"unsupported RUN (extend image.py): {rest!r}")

    def sh(self, cmd, **kw):
        print("image.py: $", " ".join(shlex.quote(c) for c in cmd), flush=True)
        subprocess.run(cmd, check=True, **kw)

    # --- driver -------------------------------------------------------
    def build(self, dockerfile):
        for line in logical_lines(open(dockerfile).read()):
            op, _, rest = line.partition(" ")
            fn = getattr(self, op.upper(), None)
            if fn is None or not op.isupper():
                die(f"unsupported Dockerfile instruction {op!r} (extend image.py)")
            fn(rest.strip())
        for k in sorted(self.args):  # docker warns, it does not fail
            print(f"image.py: warning: --build-arg {k} is not declared by any ARG in this Dockerfile", file=sys.stderr)
        if self.entrypoint is None:
            die("no ENTRYPOINT")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--context", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--venvs", required=True)
    ap.add_argument("--build-arg", action="append", default=[])
    a = ap.parse_args()
    build_args = dict(kv.split("=", 1) for kv in a.build_arg)
    img = Image(a.context, a.out, a.venvs, build_args)
    img.build(os.path.join(a.context, "deploy", "cloudrun", "Dockerfile"))
    st = img.entry_stage
    lines = []
    for k, v in st.env.items():
        lines.append(f"export {k}={shlex.quote(v)}")
    if img.venv_bin:
        lines.append(f"export PATH={shlex.quote(img.venv_bin)}:\"$PATH\"")
    lines.append(f"IMAGE_WORKDIR={shlex.quote(st.path('.'))}")
    lines.append("IMAGE_ENTRYPOINT=(" + " ".join(shlex.quote(e) for e in img.entrypoint) + ")")
    with open(os.path.join(a.out, "image.env"), "w") as f:
        f.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    main()
