#!/bin/sh
# Start and stop kitbashd the way the OpenRC service does, on a runner that has
# no OpenRC: as root, in the background, with the default socket and a store the
# job chose, and up only once the health path answers.
#
#   daemon.sh start <binary> <store> <log>
#   daemon.sh stop
#
# Run as root. The health path is the same one the service script's healthcheck
# runs every 60 s, see spec/kitbashd-api.yaml.
set -eu

socket=/run/kitbash/kitbashd.sock
pidfile=/run/kitbash-e2e.pid

log() { printf '\033[1;36m[e2e]\033[0m %s\n' "$*"; }
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 1; }

health() {
  curl --silent --fail --max-time 5 --unix-socket "$socket" http://localhost/kitbash/v1/health
}

case "${1:?usage: daemon.sh start <binary> <store> <log> | stop}" in
  start)
    binary=${2:?the kitbashd to start}
    store=${3:?the store to open}
    logfile=${4:?the log to write}
    mkdir -p "$(dirname "$store")" "$(dirname "$logfile")"
    log "starting $binary on $store"
    # setsid, so the daemon outlives the step that started it.
    setsid "$binary" -store "$store" >>"$logfile" 2>&1 &
    echo $! > "$pidfile"
    i=0
    while [ "$i" -lt 60 ]; do
      if answer=$(health 2>/dev/null); then
        log "up: $answer"
        exit 0
      fi
      kill -0 "$(cat "$pidfile")" 2>/dev/null || { log "kitbashd exited at start"; tail -40 "$logfile"; exit 1; }
      i=$((i + 1))
      sleep 0.5
    done
    log "kitbashd did not answer on $socket within 30 seconds"
    tail -40 "$logfile"
    exit 1
    ;;
  stop)
    [ -f "$pidfile" ] || { log "no daemon to stop"; exit 0; }
    pid=$(cat "$pidfile")
    log "stopping kitbashd $pid"
    kill "$pid" 2>/dev/null || true
    i=0
    while [ "$i" -lt 40 ]; do
      kill -0 "$pid" 2>/dev/null || break
      i=$((i + 1))
      sleep 0.5
    done
    if kill -0 "$pid" 2>/dev/null; then
      log "kitbashd did not stop; killing it"
      kill -9 "$pid" 2>/dev/null || true
    fi
    rm -f "$pidfile" "$socket"
    ;;
  *)
    echo "usage: daemon.sh start <binary> <store> <log> | stop" >&2
    exit 1
    ;;
esac
