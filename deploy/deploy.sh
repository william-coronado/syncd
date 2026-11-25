#!/usr/bin/env bash
# deploy.sh - build syncd, install it on a configurable set of machines,
# restart any syncd watch service running there so it picks up the new
# binary, and wait for each restarted daemon's first full cycle.
#
# Usage: deploy/deploy.sh [options]
#
#   --host DEST       deploy to DEST instead of the hosts file's targets
#                     (repeatable). DEST is `local` (this machine) or an ssh
#                     destination: user@host, or a Host alias from ~/.ssh/config
#   --hosts-file F    read the targets from F
#   --local-only      deploy to the `local` target only
#   --remote-only     deploy to every target except `local`
#   --no-restart      install the binaries, but leave running services alone
#   --no-wait         restart services, but don't wait for their first cycle
#   --test            run `go vet` and `go test` before building
#   --list            print the targets and exit
#
# Targets: one per line in the hosts file. It is $SYNCD_DEPLOY_HOSTS if set,
# else ${XDG_CONFIG_HOME:-~/.config}/syncd/deploy-hosts. Each line is
#
#     DEST  [OS/ARCH]  [BIN_DIR]
#
#   DEST     `local`, or an ssh destination
#   OS/ARCH  Go platform of the target (linux/amd64, darwin/arm64, ...);
#            `auto`, the default, asks the target with `uname`
#   BIN_DIR  install directory on the target (default: ~/.local/bin, in the
#            target user's home)
#
# Blank lines and `#` comments are ignored; see deploy/deploy-hosts.example.
# With no hosts file and no --host, the only target is `local`.
#
# A target may be macOS (launchd agents com.syncd.watch*, and the template's
# com.user.syncd*) or Linux (systemd user units syncd-watch*). Remote
# targets need ssh (keys or an agent; each is authenticated once per run)
# and bash. Only services whose binary is the installed BIN_DIR/syncd are
# restarted; others are reported and left alone.
#
# Environment:
#   SYNCD_REPO          source checkout (default: the checkout holding this script)
#   SYNCD_DEPLOY_HOSTS  hosts file (default above)
#   SYNCD_WAIT_TIMEOUT  seconds to allow for the first cycle, on top of the
#                       service's --remote-poll interval (default: 300)
#
# Why a restart: a running `syncd watch` keeps executing the binary it was
# started from; it only picks up the new code when the service restarts.
# Reinstalling the service is NOT needed - the launchd plist / systemd unit
# only names the binary's path and its watch flags. Re-run `syncd service
# install` only to change those flags.
#
# Waiting: a restarted daemon runs its first whole-tree cycle at its first
# --remote-poll tick (30s by default) and logs "watch: first full cycle
# complete" when that succeeds; any error it logs first fails the deploy.
# Targets are waited on in parallel. The script exits non-zero if any daemon
# errors, never starts, or times out.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="${SYNCD_REPO:-$(cd "$script_dir/.." && pwd)}"
HOSTS_FILE="${SYNCD_DEPLOY_HOSTS:-${XDG_CONFIG_HOME:-$HOME/.config}/syncd/deploy-hosts}"
# A hosts file named explicitly must exist; only the default may be absent.
hosts_explicit=0
[[ -n "${SYNCD_DEPLOY_HOSTS:-}" ]] && hosts_explicit=1
WAIT_TIMEOUT="${SYNCD_WAIT_TIMEOUT:-300}"
if [[ ! "$WAIT_TIMEOUT" =~ ^[0-9]+$ ]]; then
	echo "SYNCD_WAIT_TIMEOUT must be a whole number of seconds, not '$WAIT_TIMEOUT'" >&2
	exit 2
fi
DEFAULT_BIN_DIR="~/.local/bin" # expanded on the target

only=all # all | local | remote
restart=1
wait_cycle=1
run_tests=0
list_only=0
cli_hosts=()

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--host)
		[[ $# -ge 2 ]] || { echo "--host needs a destination" >&2; exit 2; }
		cli_hosts+=("$2")
		shift
		;;
	--host=*) cli_hosts+=("${1#*=}") ;;
	--hosts-file)
		[[ $# -ge 2 ]] || { echo "--hosts-file needs a path" >&2; exit 2; }
		HOSTS_FILE="$2" hosts_explicit=1
		shift
		;;
	--hosts-file=*)
		HOSTS_FILE="${1#*=}" hosts_explicit=1
		;;
	--local-only) [[ $only == remote ]] && only=none || only=local ;;
	--remote-only) [[ $only == local ]] && only=none || only=remote ;;
	--no-restart) restart=0 ;;
	--no-wait) wait_cycle=0 ;;
	--test) run_tests=1 ;;
	--list) list_only=1 ;;
	-h | --help) usage; exit 0 ;;
	*) echo "unknown option: $1 (see --help)" >&2; exit 2 ;;
	esac
	shift
done
if [[ $hosts_explicit -eq 1 && ${#cli_hosts[@]} -eq 0 && ! -f "$HOSTS_FILE" ]]; then
	echo "no such hosts file: $HOSTS_FILE" >&2
	exit 2
fi
if [[ $only == none ]]; then
	echo "--local-only and --remote-only together leave nothing to do" >&2
	exit 2
fi
[[ $restart -eq 1 ]] || wait_cycle=0

step() { printf '\n==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf '%s\n' "$*" >&2; exit 1; }

# --- targets: parallel arrays, one entry per machine ---------------------------
t_dest=() t_plat=() t_bin=()
add_target() { # DEST [OS/ARCH] [BIN_DIR]
	local dest="$1" plat="${2:-auto}" bin="${3:-$DEFAULT_BIN_DIR}"
	# ssh and scp would read a leading '-' as an option, not a host.
	[[ "$dest" != -* ]] || die "target '$dest': a destination cannot start with '-'"
	case "$plat" in
	auto | */*) ;;
	*) die "target $dest: platform '$plat' is not OS/ARCH (e.g. linux/amd64) or auto" ;;
	esac
	case "$only" in
	local) [[ "$dest" == local ]] || return 0 ;;
	remote) [[ "$dest" != local ]] || return 0 ;;
	esac
	t_dest+=("$dest") t_plat+=("$plat") t_bin+=("$bin")
}

hosts_source=""
if [[ ${#cli_hosts[@]} -gt 0 ]]; then
	hosts_source="--host"
	for h in "${cli_hosts[@]}"; do add_target "$h"; done
elif [[ -f "$HOSTS_FILE" ]]; then
	hosts_source="$HOSTS_FILE"
	lineno=0
	while IFS= read -r line || [[ -n "$line" ]]; do
		lineno=$((lineno + 1))
		line="${line%%#*}"
		read -r dest plat bin extra <<<"$line" || true
		[[ -n "${dest:-}" ]] || continue
		[[ -z "${extra:-}" ]] || die "$HOSTS_FILE:$lineno: too many fields (DEST [OS/ARCH] [BIN_DIR])"
		add_target "$dest" "${plat:-}" "${bin:-}"
	done <"$HOSTS_FILE"
else
	hosts_source="default (no $HOSTS_FILE)"
	add_target local
fi
[[ ${#t_dest[@]} -gt 0 ]] || die "no targets to deploy to (from $hosts_source)"

if [[ $list_only -eq 1 ]]; then
	echo "targets (from $hosts_source):"
	for i in "${!t_dest[@]}"; do
		printf '  %-28s %-14s %s\n' "${t_dest[$i]}" "${t_plat[$i]}" "${t_bin[$i]}"
	done
	exit 0
fi

command -v go >/dev/null || die "go is not installed (needed to build syncd)"
[[ -f "$REPO/go.mod" ]] || die "not a syncd checkout: $REPO (set SYNCD_REPO)"

# --- scripts run on each target (locally, or over ssh with bash -s) ------------

# The first-cycle wait.
read -r -d '' WAIT_LIB <<'WAIT_LIB_EOF' || true
FIRST_CYCLE_MSG="watch: first full cycle complete"

# dur_seconds 30s|5m|1h30m|1500ms -> whole seconds, rounded up
dur_seconds() {
	awk -v d="$1" 'BEGIN {
		t = 0
		while (match(d, /^[0-9.]+(ms|h|m|s)/)) {
			tok = substr(d, 1, RLENGTH); d = substr(d, RLENGTH + 1)
			n = tok + 0; u = tok; sub(/^[0-9.]+/, "", u)
			t += (u == "h") ? n * 3600 : (u == "m") ? n * 60 : (u == "s") ? n : n / 1000
		}
		printf "%d\n", (t == int(t)) ? t : int(t) + 1
	}'
}

# watch_arg NAME ARGS... -> the value of --NAME (or --NAME=value) in ARGS
watch_arg() {
	local name="--$1"
	shift
	while [[ $# -gt 0 ]]; do
		case "$1" in
		"$name") printf '%s\n' "${2-}"; return 0 ;;
		"$name"=*) printf '%s\n' "${1#*=}"; return 0 ;;
		esac
		shift
	done
}

# json_string S -> S escaped as Go's json.Marshal writes it inside a string
# (the log is JSON, and Marshal also escapes &, < and > for HTML safety).
json_string() {
	local s="$1"
	s="${s//\\/\\\\}"
	s="${s//\"/\\\"}"
	s="${s//&/\\u0026}"
	s="${s//</\\u003c}"
	s="${s//>/\\u003e}"
	printf '%s' "$s"
}

# cycle_state LOG LOCAL SINCE -> first line: none | started | done, judged
# from the newest startup line for LOCAL logged at or after SINCE (UTC);
# then any error lines logged after that startup line. LOG.old, the rotated
# log, is read first, so a rotation between startup and the first cycle
# loses neither line. Values reach awk through its environment: -v would
# expand the backslashes in them.
cycle_state() {
	local -a files
	files=("$1")
	[[ -f "$1.old" ]] && files=("$1.old" "$1")
	CS_START="\"syncd watch: local=$(json_string "$2") remote=" CS_SINCE="$3" CS_DONE="$FIRST_CYCLE_MSG" \
		awk '
		BEGIN { start = ENVIRON["CS_START"]; since = ENVIRON["CS_SINCE"]; done = ENVIRON["CS_DONE"] }
		{ ts = $0; sub(/^[^"]*"ts":"/, "", ts); sub(/".*$/, "", ts) }
		index($0, start) && ts >= since { status = "started"; errs = ""; next }
		status == "started" && index($0, done) { status = "done"; next }
		status == "started" && index($0, "\"level\":\"error\"") { errs = errs $0 "\n" }
		END { print (status == "" ? "none" : status); printf "%s", errs }' \
		"${files[@]}"
}

# wait_first_cycle NAME SINCE TIMEOUT WATCH-ARGS...
wait_first_cycle() {
	local name="$1" since="$2" extra="$3"
	shift 3
	local local_root log_path state_dir poll timeout deadline f out state errs
	local -a logs
	local_root="$(watch_arg local "$@")"
	if ! local_root="$(cd "$local_root" 2>/dev/null && pwd -P)"; then
		echo "  $name: cannot resolve its --local root; not waiting" >&2
		return 1
	fi
	log_path="$(watch_arg log-path "$@")"
	state_dir="$(watch_arg state-dir "$@")"
	state_dir="${state_dir:-$HOME/.local/state/syncd}"
	poll="$(watch_arg remote-poll "$@")"
	poll="$(dur_seconds "${poll:-30s}")"
	timeout=$((poll + extra))
	deadline=$(($(date +%s) + timeout))
	echo "  $name: waiting for its first full cycle (up to ${timeout}s)"
	state=none
	while (($(date +%s) < deadline)); do
		if [[ -n "$log_path" ]]; then
			logs=("$log_path")
		else
			logs=("$state_dir"/sync-*.log)
		fi
		state=none errs=""
		for f in "${logs[@]}"; do
			[[ -f "$f" ]] || continue
			out="$(cycle_state "$f" "$local_root" "$since")"
			state="$(printf '%s\n' "$out" | head -n 1)"
			errs="$(printf '%s\n' "$out" | tail -n +2)"
			[[ "$state" == none ]] || break
		done
		if [[ -n "$errs" ]]; then
			echo "  $name: logged an error before completing a full cycle ($f):" >&2
			printf '%s\n' "$errs" | cut -c1-300 | sed 's/^/    /' >&2
			return 1
		fi
		if [[ "$state" == done ]]; then
			echo "  $name: first full cycle complete"
			return 0
		fi
		sleep 3
	done
	if [[ "$state" == none ]]; then
		echo "  $name: no startup in its log within ${timeout}s (is the remote mounted? --wait-for-remote holds startup until it is)" >&2
	else
		echo "  $name: started, but no full cycle completed within ${timeout}s" >&2
	fi
	return 1
}
WAIT_LIB_EOF
eval "$WAIT_LIB"

# Probe: prints "OS/ARCH" for the target, in Go's names.
read -r -d '' PROBE_SCRIPT <<'PROBE_EOF' || true
set -eu
os="$(uname -s)" arch="$(uname -m)"
case "$os" in Darwin) os=darwin ;; Linux) os=linux ;; *) os="unknown-$os" ;; esac
case "$arch" in x86_64 | amd64) arch=amd64 ;; arm64 | aarch64) arch=arm64 ;; *) arch="unknown-$arch" ;; esac
echo "$os/$arch"
PROBE_EOF

# Prepare: creates BIN_DIR (a leading ~ is the target user's home) and prints
# its absolute path.
read -r -d '' PREPARE_SCRIPT <<'PREPARE_EOF' || true
set -eu
bin_dir="$1"
case "$bin_dir" in "~") bin_dir="$HOME" ;; "~/"*) bin_dir="$HOME/${bin_dir#\~/}" ;; esac
mkdir -p "$bin_dir"
cd "$bin_dir" && pwd -P
PREPARE_EOF

# Install: verifies the copied staging file, renames it over syncd, then
# restarts the watch services that run it (launchd on macOS, systemd on
# Linux) and waits for their first cycle.
read -r -d '' INSTALL_SCRIPT <<'INSTALL_EOF' || true
set -euo pipefail
bin_dir="$1" stage="$2" want="$3" restart="$4" wait_cycle="$5" wait_timeout="$6"
sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}
new="$bin_dir/$stage"
got="$(sha256 "$new")"
if [[ "$got" != "$want" ]]; then
	rm -f "$new"
	echo "checksum mismatch after copy; nothing installed" >&2
	exit 1
fi
# Rename over the old binary rather than overwrite it: overwriting a running
# executable in place can get the process killed on macOS (its code
# signature no longer matches) and fails with "Text file busy" on Linux.
chmod +x "$new"
mv -f "$new" "$bin_dir/syncd"
echo "installed $bin_dir/syncd: ${got:0:16}…"

[[ "$restart" == 1 ]] || exit 0
deployed="$bin_dir/syncd"
status=0

restart_launchd() {
	local found=0 plist label target i arg bin since
	local -a args
	shopt -s nullglob
	# com.syncd.watch*: `syncd service install`; com.user.syncd*: the
	# hand-edited template in deploy/launchd.
	for plist in "$HOME"/Library/LaunchAgents/com.syncd.watch*.plist "$HOME"/Library/LaunchAgents/com.user.syncd*.plist; do
		found=1
		label="$(basename "$plist" .plist)"
		target="gui/$(id -u)/$label"
		args=()
		i=0
		while arg="$(/usr/libexec/PlistBuddy -c "Print :ProgramArguments:$i" "$plist" 2>/dev/null)"; do
			args+=("$arg")
			i=$((i + 1))
		done
		bin="${args[0]:-}"
		if [[ "$bin" != "$deployed" ]]; then
			echo "warning: $label runs ${bin:-?}, not $deployed; leaving it alone" >&2
		elif launchctl print "$target" >/dev/null 2>&1; then
			since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
			launchctl kickstart -k "$target"
			echo "restarted $label"
			if [[ "$wait_cycle" == 1 ]]; then
				wait_first_cycle "$label" "$since" "$wait_timeout" "${args[@]:1}" || status=1
			fi
		else
			echo "$label is installed but not loaded (or not in this login session); it will use the new binary when started"
		fi
	done
	shopt -u nullglob
	[[ $found -eq 1 ]] || echo "no syncd launchd service installed"
}

# running_argv PID -> the process's argv, one per line, exactly as it was
# started. systemctl's ExecStart property joins argv with spaces and no
# quoting, so a path with a space in it cannot be told from two arguments.
running_argv() {
	[[ -r "/proc/$1/cmdline" ]] || return 1
	tr '\0' '\n' <"/proc/$1/cmdline"
}

restart_systemd() {
	local units unit pid exec_bin since line
	local -a argv
	units="$(systemctl --user list-unit-files 'syncd-watch*' --no-legend 2>/dev/null | awk '{print $1}')"
	if [[ -z "$units" ]]; then
		echo "no syncd systemd service installed"
		return 0
	fi
	for unit in $units; do
		pid="$(systemctl --user show -p MainPID --value "$unit" 2>/dev/null || echo 0)"
		if [[ -z "$pid" || "$pid" == 0 ]] || ! systemctl --user is-active --quiet "$unit"; then
			echo "$unit is installed but not running; it will use the new binary when started"
			continue
		fi
		argv=()
		while IFS= read -r line; do argv+=("$line"); done < <(running_argv "$pid")
		exec_bin="${argv[0]:-}"
		if [[ "$exec_bin" != "$deployed" ]]; then
			echo "warning: $unit runs ${exec_bin:-?}, not $deployed; leaving it alone" >&2
		else
			since="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
			systemctl --user restart "$unit"
			sleep 2
			if ! systemctl --user is-active --quiet "$unit"; then
				echo "warning: $unit is not active after restart:" >&2
				systemctl --user status "$unit" --no-pager -n 10 >&2 || true
				status=1
				continue
			fi
			echo "restarted $unit (active)"
			if [[ "$wait_cycle" == 1 ]]; then
				wait_first_cycle "$unit" "$since" "$wait_timeout" "${argv[@]:1}" || status=1
			fi
		fi
	done
}

case "$(uname -s)" in
Darwin) restart_launchd ;;
Linux) restart_systemd ;;
*) echo "warning: don't know how to restart services on $(uname -s)" >&2 ;;
esac
exit "$status"
INSTALL_EOF

# --- running things on a target ------------------------------------------------
# Under /tmp, not $TMPDIR: the ssh control sockets live here, and a socket
# path must fit in 104 bytes; macOS's $TMPDIR alone takes about 50.
work_dir="$(mktemp -d /tmp/syncd-deploy.XXXXXX)"
ssh_opts=(-o ControlMaster=auto -o "ControlPath=$work_dir/ssh-%C" -o ControlPersist=120 -o ConnectTimeout=10)
cleanup() {
	local d
	for d in ${t_dest[@]+"${t_dest[@]}"}; do
		[[ "$d" == local ]] || ssh "${ssh_opts[@]}" -O exit "$d" >/dev/null 2>&1 || true
	done
	rm -rf "$work_dir"
}
trap cleanup EXIT

# run_on DEST SCRIPT ARGS... -> runs SCRIPT with bash on DEST
run_on() {
	local dest="$1" script="$2"
	shift 2
	if [[ "$dest" == local ]]; then
		printf '%s\n' "$script" | bash -s -- "$@"
	else
		local cmd="bash -s --" a
		for a in "$@"; do cmd+=" $(printf '%q' "$a")"; done
		printf '%s\n' "$script" | ssh "${ssh_opts[@]}" "$dest" "$cmd"
	fi
}

# copy_to DEST SRC PATH -> copies the local file SRC to PATH on DEST
copy_to() {
	if [[ "$1" == local ]]; then
		cp "$2" "$3"
	else
		scp -q "${ssh_opts[@]}" "$2" "$1:$3"
	fi
}

sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }

# --- what is being deployed -----------------------------------------------------
rev="$(git -C "$REPO" describe --always --dirty 2>/dev/null || echo unknown)"
branch="$(git -C "$REPO" branch --show-current 2>/dev/null || true)"
step "Deploying syncd ${rev} (branch: ${branch:-detached}) to ${#t_dest[@]} target(s) from $hosts_source"
[[ "$rev" == *-dirty ]] && warn "the checkout has uncommitted changes; they are included in this build"
[[ -n "$branch" && "$branch" != "main" ]] && warn "building from '$branch', not main"

if [[ $run_tests -eq 1 ]]; then
	step "Vetting and testing"
	(cd "$REPO" && go vet ./... && go test ./...)
fi

# --- connect, and learn each target's platform and bin dir ------------------------
step "Connecting to targets"
t_dir=()
for i in "${!t_dest[@]}"; do
	dest="${t_dest[$i]}"
	if [[ "${t_plat[$i]}" == auto ]]; then
		t_plat[$i]="$(run_on "$dest" "$PROBE_SCRIPT")" || die "$dest: cannot reach it to detect its platform"
	fi
	[[ "${t_plat[$i]}" != *unknown-* ]] || die "$dest: unsupported platform ${t_plat[$i]}; set OS/ARCH for it in the hosts file"
	t_dir[$i]="$(run_on "$dest" "$PREPARE_SCRIPT" "${t_bin[$i]}")" || die "$dest: cannot create ${t_bin[$i]}"
	echo "  $dest: ${t_plat[$i]}, installing to ${t_dir[$i]}/syncd"
done

# --- build, once per platform -----------------------------------------------------
for i in "${!t_dest[@]}"; do
	plat="${t_plat[$i]}"
	out="$work_dir/syncd-${plat%/*}-${plat#*/}"
	[[ -f "$out" ]] && continue
	step "Building for $plat"
	# modernc.org/sqlite is pure Go, so every build is static and cgo-free.
	(cd "$REPO" && GOOS="${plat%/*}" GOARCH="${plat#*/}" CGO_ENABLED=0 go build -trimpath -o "$out" ./cmd/syncd)
done

# --- copy, then install / restart / wait on every target in parallel ------------
step "Copying binaries"
# A staging name per target and run: two targets can share a directory (one
# host reached by two names), and so can two deploys running at once.
stage_base=".syncd.new.$(hostname -s 2>/dev/null || echo host).$$"
for i in "${!t_dest[@]}"; do
	dest="${t_dest[$i]}" plat="${t_plat[$i]}"
	copy_to "$dest" "$work_dir/syncd-${plat%/*}-${plat#*/}" "${t_dir[$i]}/$stage_base.$i" || die "$dest: copy failed; nothing installed there"
	echo "  $dest: copied"
done

step "Installing$([[ $restart -eq 1 ]] && echo ", restarting services")$([[ $wait_cycle -eq 1 ]] && echo " and waiting for their first cycle")"
pids=()
for i in "${!t_dest[@]}"; do
	dest="${t_dest[$i]}" plat="${t_plat[$i]}"
	want="$(sha256 "$work_dir/syncd-${plat%/*}-${plat#*/}")"
	(
		run_on "$dest" "$WAIT_LIB"$'\n'"$INSTALL_SCRIPT" \
			"${t_dir[$i]}" "$stage_base.$i" "$want" "$restart" "$wait_cycle" "$WAIT_TIMEOUT" 2>&1 |
			sed -e "s|^|[$dest] |"
	) &
	pids[$i]=$!
done

failed=()
for i in "${!t_dest[@]}"; do
	wait "${pids[$i]}" || failed+=("${t_dest[$i]}")
done

if [[ ${#failed[@]} -gt 0 ]]; then
	step "Deployed syncd ${rev}, but needs attention on: ${failed[*]} (see above)"
	exit 1
fi
step "Done: syncd ${rev}"
