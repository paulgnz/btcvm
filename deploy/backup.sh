#!/usr/bin/env bash
# Encrypted backups of everything a BTCVM mainnet host can't rebuild from
# the chains: the peg signer set, the P-Chain key, the validator's staking
# identity, the deposit address registry, the watch-only Bitcoin wallet, and
# the service configuration. Run as root on the host:
#
#   deploy/backup.sh setup AGE_RECIPIENT...   # install a daily backup
#   deploy/backup.sh run                      # back up now
#   deploy/backup.sh list
#
# Backups are encrypted with age to the recipients' public keys. The host
# holds no decryption key, so it can write backups but not read them. Keep
# the age identity (private key) offline, e.g. in a password manager.
#
# Each backup lands in $BACKUP_DIR; the newest $KEEP are kept. If
# /etc/btcvm-backup/offsite holds an rsync destination (for example a
# Hetzner Storage Box, user@host:dir), each backup is copied there too.
# scripts/pull-backups.sh copies them to another machine, and
# scripts/restore-check.sh proves one restores.
set -euo pipefail

STATE=/var/lib/metal-main
SECRETS=$STATE/secrets
BTC_DIR=/var/lib/bitcoin-main
CONF=/etc/btcvm-backup
BACKUP_DIR=${BACKUP_DIR:-/var/backups/btcvm}
KEEP=${KEEP:-30}

log() { echo "backup: $*"; }

cmd_setup() {
  [[ $# -ge 1 ]] || { echo "usage: $0 setup AGE_RECIPIENT..." >&2; exit 2; }
  command -v age >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -yq age >/dev/null
  install -d -m 700 "$CONF" "$BACKUP_DIR"
  printf '%s\n' "$@" >"$CONF/recipients"
  install -m 755 "$(readlink -f "$0")" /usr/local/sbin/btcvm-backup
  cat >/etc/systemd/system/btcvm-backup.service <<'UNIT'
[Unit]
Description=Encrypted backup of BTCVM keys and configuration

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/btcvm-backup run
UNIT
  cat >/etc/systemd/system/btcvm-backup.timer <<'UNIT'
[Unit]
Description=Daily BTCVM backup

[Timer]
OnCalendar=*-*-* 03:17:00 UTC
RandomizedDelaySec=20m
Persistent=true

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now btcvm-backup.timer >/dev/null
  log "daily backups to $BACKUP_DIR, encrypted to $# recipient(s)"
  cmd_run
}

cmd_run() {
  [[ -s "$CONF/recipients" ]] || { echo "no recipients; run: $0 setup AGE_RECIPIENT" >&2; exit 1; }
  local stamp out
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  # Global, so the exit trap can still see it.
  WORK=$(mktemp -d)
  trap 'rm -rf "$WORK"' EXIT
  local work=$WORK
  mkdir -p "$work/btcvm"
  local root=$work/btcvm

  # Files, copied with their paths.
  local files=(
    "$SECRETS"
    "$STATE/chain.json" "$STATE/genesis.json" "$STATE/chain-configs"
    "$STATE/node/staking"
    "$BTC_DIR/bitcoin.conf"
    /etc/caddy/Caddyfile
  )
  for unit in /etc/systemd/system/{metal-mainnet,bitcoind-main,btcvm-bridge-main,btcvm-web-main,btcvm-monitor-main,btcvm-signer-1,btcvm-signer-2,btcvm-signer-3}.service; do
    [[ -f $unit ]] && files+=("$unit")
  done
  for f in "${files[@]}"; do
    [[ -e $f ]] || { log "missing $f"; continue; }
    mkdir -p "$root$(dirname "$f")"
    cp -a "$f" "$root$(dirname "$f")/"
  done

  # The watch-only wallet, copied consistently by Bitcoin Core itself.
  # Rebuilding it instead means a rescan of the whole chain.
  # Bitcoin Core 1.14 writes wallet backups into its own backups/ folder,
  # whatever path it is given.
  local wallet=btcvm-wallet-$stamp.dat
  if sudo -u btcvm /opt/bitcoin/bin/bitcoin-cli -datadir="$BTC_DIR" backupwallet "$wallet" 2>/dev/null &&
    [[ -f "$BTC_DIR/backups/$wallet" ]]; then
    mkdir -p "$root$BTC_DIR"
    mv "$BTC_DIR/backups/$wallet" "$root$BTC_DIR/wallet.dat"
  else
    log "could not back up the Bitcoin wallet (is bitcoind running?)"
  fi

  # A manifest, so a restore can be checked against what was live.
  local node_id peg
  node_id=$(curl -s -m 10 -X POST -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' http://127.0.0.1:9660/ext/info | jq -r '.result.nodeID // empty')
  peg=$(jq -r '.bitcoinPegAddress // empty' "$SECRETS/signers.out" 2>/dev/null || true)
  (cd "$root" && find . -type f ! -name MANIFEST.json -print0 | sort -z | xargs -0 sha256sum) >"$work/sums"
  jq -n --arg host "$(hostname)" --arg time "$stamp" --arg nodeID "$node_id" --arg peg "$peg" \
    --rawfile sums "$work/sums" \
    '{host: $host, time: $time, nodeID: $nodeID, bitcoinPegAddress: $peg, sha256: $sums}' >"$root/MANIFEST.json"

  out=$BACKUP_DIR/btcvm-$stamp.tar.age
  install -d -m 700 "$BACKUP_DIR"
  local recipients=()
  while read -r r; do [[ -n $r && $r != \#* ]] && recipients+=(-r "$r"); done <"$CONF/recipients"
  tar -C "$work" -czf - btcvm | age "${recipients[@]}" -o "$out.partial"
  chmod 600 "$out.partial"
  mv "$out.partial" "$out"
  log "wrote $out ($(du -h "$out" | cut -f1))"

  # Keep the newest $KEEP.
  ls -1t "$BACKUP_DIR"/btcvm-*.tar.age | tail -n +$((KEEP + 1)) | xargs -r rm -f

  if [[ -s "$CONF/offsite" ]]; then
    rsync -a --chmod=F600 "$out" "$(cat "$CONF/offsite")/" && log "copied offsite"
  fi
}

cmd_list() {
  ls -lh "$BACKUP_DIR"/btcvm-*.tar.age 2>/dev/null || echo "no backups yet"
  systemctl list-timers btcvm-backup.timer --no-pager 2>/dev/null | head -2
}

case "${1:-}" in
  setup) shift; cmd_setup "$@" ;;
  run) cmd_run ;;
  list) cmd_list ;;
  *) echo "usage: $0 setup AGE_RECIPIENT... | run | list" >&2; exit 2 ;;
esac
