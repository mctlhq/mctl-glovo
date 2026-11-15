#!/usr/bin/env bash
# glovo-relogin.sh: sign in to Glovo on this machine (a laptop with a
# browser) and install the new session on the glovo-home host.
#
# 1. Runs glovo-skill's login into a temporary session file inside a 0700
#    temporary directory.
# 2. Streams the file over ssh into a temporary file next to the target
#    (mode 0600, directory 0700) and moves it into place atomically.
# 3. Deletes the local copy, also on failure or Ctrl-C.
#
# 4. Touches <session>.reload on the host; glovo-home notices the marker
#    (size/mtime) and restarts glovo-skill, while glovo-home keeps running.
#    The session file itself is not watched, because glovo-skill rewrites
#    it on every token refresh. The session file is never printed.
#
# Environment:
#   GLOVO_HOME_HOST            required: ssh destination of the glovo-home host
#   GLOVO_SKILL_DIR            glovo-skill checkout on this machine
#                              (default ~/.local/share/glovo-skill)
#   GLOVO_NODE                 node binary (default: node on PATH)
#   GLOVO_HOME_REMOTE_SESSION  session path on the host, relative to the
#                              remote home directory (default .glovo/session.json)
set -euo pipefail

host="${GLOVO_HOME_HOST:-}"
skill_dir="${GLOVO_SKILL_DIR:-$HOME/.local/share/glovo-skill}"
node="${GLOVO_NODE:-node}"
remote="${GLOVO_HOME_REMOTE_SESSION:-.glovo/session.json}"

die() { echo "glovo-relogin: $*" >&2; exit 1; }

[ -n "$host" ] || die "set GLOVO_HOME_HOST to the ssh destination of the glovo-home host"
[ -f "$skill_dir/scripts/login.mjs" ] || die "glovo-skill not found at $skill_dir (set GLOVO_SKILL_DIR)"
command -v "$node" >/dev/null || die "node not found (set GLOVO_NODE)"
# The remote path goes into a remote shell command: allow a plain relative
# path only.
if ! [[ "$remote" =~ ^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$ ]] || [[ "$remote" == *..* ]]; then
  die "GLOVO_HOME_REMOTE_SESSION must be a plain path relative to the remote home"
fi
remote_dir="$(dirname "$remote")"

umask 077
tmp="$(mktemp -d "${TMPDIR:-/tmp}/glovo-relogin.XXXXXX")"
chmod 700 "$tmp"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT INT TERM HUP

session="$tmp/session.json"
echo "glovo-relogin: opening the Glovo sign-in in a browser..."
# login.mjs prints only a summary (signed_in, days_left), never the session.
GLOVO_SESSION_PATH="$session" "$node" "$skill_dir/scripts/login.mjs"
[ -s "$session" ] || die "login did not produce a session file"

echo "glovo-relogin: installing the session on the host..."
# The file travels on stdin only. Remotely: 0700 directory, 0600 temp file
# in the same directory, then an atomic rename over the old session, then
# the reload marker (its content is only a timestamp).
# shellcheck disable=SC2029 # $remote and $remote_dir are validated above and expand locally on purpose
ssh "$host" "set -eu; umask 077; mkdir -p '$remote_dir'; chmod 700 '$remote_dir'; t=\$(mktemp '$remote.XXXXXX'); trap 'rm -f \"\$t\"' EXIT; cat > \"\$t\"; [ -s \"\$t\" ]; chmod 600 \"\$t\"; mv -f \"\$t\" '$remote'; trap - EXIT; date > '$remote.reload'" < "$session"

echo "glovo-relogin: done. glovo-home restarts glovo-skill within about 15 seconds."
