#!/usr/bin/env bash
# Install or upgrade mailstatus-collector from an extracted release bundle.
#
#   sudo ./install.sh              first install, or upgrade (restarts a running service)
#   sudo ./install.sh --no-restart upgrade the files only; restart yourself later
#
# Installs the binary, the systemd unit, the docs and (first install only) the
# settings file, and creates the service user. It does nothing with Postfix
# (no changes, no checks): Postfix is set up by hand, docs/INSTALL.md Step 1.
# It never changes RabbitMQ either. Step-by-step guide: docs/INSTALL.md.
#
# DESTDIR=/tmp/root ./install.sh   install the files under /tmp/root only
#                                  (no user, no systemd; for packaging and tests)
set -euo pipefail

BINARY=mailstatus-collector
SERVICE_USER=mailstatus-collector
PREFIX=/usr/local
DESTDIR=${DESTDIR:-}
RESTART=1

for arg in "$@"; do
	case $arg in
	--no-restart) RESTART=0 ;;
	-h | --help)
		sed -n '2,13p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "unknown option: $arg (see --help)" >&2
		exit 2
		;;
	esac
done

cd "$(dirname "$0")"
say() { printf '==> %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# ------------------------------------------------------------------ checks
[ -x "bin/$BINARY" ] || die "bin/$BINARY not found: run this from the extracted bundle"
if [ -z "$DESTDIR" ]; then
	[ "$(id -u)" -eq 0 ] || die "run as root (sudo ./install.sh)"
	command -v systemctl >/dev/null || die "systemd is required"
	command -v journalctl >/dev/null || die "journalctl is required"
fi
if [ -f SHA256SUMS ]; then
	sha256sum --quiet -c SHA256SUMS || die "checksum mismatch: the bundle is damaged"
fi
case "$(uname -m)" in
x86_64 | amd64) want=x86-64 ;;
aarch64 | arm64) want=aarch64 ;;
*) want=unknown ;;
esac
if command -v file >/dev/null && [ "$want" != unknown ] &&
	! file "bin/$BINARY" | grep -qi -- "$want"; then
	die "bin/$BINARY was built for another CPU than this server ($(uname -m)); build with GOARCH"
fi

new_version=$("bin/$BINARY" -version | awk '{print $2}')
old_version=$("$DESTDIR$PREFIX/bin/$BINARY" -version 2>/dev/null | awk '{print $2}' || true)
if [ -n "$old_version" ]; then
	say "upgrading $BINARY $old_version -> $new_version"
else
	say "installing $BINARY $new_version"
fi

# ------------------------------------------------------------------ user
if [ -z "$DESTDIR" ]; then
	if ! id "$SERVICE_USER" >/dev/null 2>&1; then
		useradd --system --no-create-home --home-dir /nonexistent \
			--shell /usr/sbin/nologin "$SERVICE_USER"
		say "created system user $SERVICE_USER"
	fi
	getent group systemd-journal >/dev/null ||
		die "group systemd-journal is missing; the service could not read the journal"
fi

# ------------------------------------------------------------------ files
# install(1) writes a new file and renames it into place, so this works while
# the old binary is running ("Text file busy" only affects writing in place).
install -D -m 0755 "bin/$BINARY" "$DESTDIR$PREFIX/bin/$BINARY"
install -D -m 0644 "deploy/$BINARY.service" "$DESTDIR/etc/systemd/system/$BINARY.service"
docdir="$DESTDIR$PREFIX/share/doc/$BINARY"
for doc in README.md LICENSE docs/INSTALL.md docs/BENCHMARK.md; do
	[ -f "$doc" ] && install -D -m 0644 "$doc" "$docdir/$(basename "$doc")"
done
install -D -m 0644 deploy/env.example "$docdir/env.example"
say "installed $PREFIX/bin/$BINARY, the systemd unit, and docs in $PREFIX/share/doc/$BINARY"

env_file="$DESTDIR/etc/$BINARY/env"
if [ -e "$env_file" ]; then
	say "kept existing settings $env_file"
else
	install -D -m 0600 deploy/env.example "$env_file"
	say "created $env_file"
fi

# Settings that need attention. Warnings only: the file belongs to the admin.
if ! grep -q '^MAILSTATUS_RABBITMQ_URL=.' "$env_file"; then
	warn "$env_file has no MAILSTATUS_RABBITMQ_URL; the service will not start
         until it is set (docs/INSTALL.md, Step 4)."
elif grep -q '^MAILSTATUS_RABBITMQ_[A-Z_]*=.*CHANGE_ME' "$env_file"; then
	warn "$env_file still has CHANGE_ME in the RabbitMQ settings (docs/INSTALL.md, Step 4)"
fi

[ -n "$DESTDIR" ] && { say "files installed under $DESTDIR (no user / systemd changes)"; exit 0; }

systemctl daemon-reload

# ------------------------------------------------------------------ service
if systemctl is-active --quiet "$BINARY"; then
	if [ "$RESTART" -eq 1 ]; then
		say "restarting $BINARY"
		systemctl restart "$BINARY"
		sleep 3
		if systemctl is-active --quiet "$BINARY"; then
			say "running: $("$PREFIX/bin/$BINARY" -version)"
		else
			journalctl -u "$BINARY" -n 15 --no-pager >&2 || true
			die "$BINARY did not stay up after the upgrade (see the log above).
       Fix the cause (docs/INSTALL.md, Troubleshooting), then: systemctl restart $BINARY"
		fi
	else
		say "not restarted (--no-restart); run: systemctl restart $BINARY"
	fi
	exit 0
fi

# Not running: first install, or installed but stopped.
cat <<EOF

Next steps (the service is installed but NOT started):

  1. Postfix settings and log rate limit:  docs/INSTALL.md, Step 1
  2. RabbitMQ queue and account:           docs/INSTALL.md, Step 2
  3. Settings (RabbitMQ URL and queue):    vi /etc/$BINARY/env
  4. Dry run on today's log:               docs/INSTALL.md, Step 5
  5. Start it:                             systemctl enable --now $BINARY
  6. Watch it:                             tail -f /var/log/$BINARY/collector.log

EOF
