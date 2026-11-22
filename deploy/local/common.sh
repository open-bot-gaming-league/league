# common.sh: sourced by run.sh and stage-data.sh. Not executable on its own.
#
# Box-specific paths are never defaulted in the repo. Set them in the
# environment, or in deploy/local/local.env (gitignored; plain VAR=value
# lines, sourced here):
#   OBGL_LOCAL_ROOT   directory for trees/, venvs/, data/, state/ (created if missing)
#   OBGL_BOT_SRC      directory holding the trained bot files (stage-data.sh only)

_COMMON_DIR=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)
if [ -f "$_COMMON_DIR/local.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$_COMMON_DIR/local.env"
  set +a
fi

need() {  # need VAR: fail with a clear message if VAR is unset or empty
  eval "[ -n \"\${$1:-}\" ]" || {
    echo "deploy/local: $1 is not set (export it, or put $1=... in $_COMMON_DIR/local.env; see README.md)" >&2
    exit 1
  }
}
