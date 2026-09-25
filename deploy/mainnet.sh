#!/usr/bin/env bash
# Runs BTCVM as an L1 on Metal mainnet, pegged to Bitcoin mainnet.
# Run as root on a host prepared by deploy/provision.sh:
#
#   deploy/mainnet.sh status    # what is synced and funded
#   deploy/mainnet.sh launch    # create the L1 and start everything
#
# Before launch:
#   - the Metal mainnet node (metal-mainnet.service) has synced the P-Chain;
#   - the P-Chain key in $SECRETS/p-chain-key.json holds enough METAL: about
#     5 METAL prepays the validator's continuous fee for several months;
#   - the peg signer set is in $SECRETS/signers.json.
# Bitcoin Core (bitcoind-main.service) may still be syncing: deposits are
# credited once it has caught up. It runs pruned (deploy/provision.sh):
# about 100 GB of disk; the first sync takes a day or more.
#
# Alerts go to the Slack or Discord webhook URL in $SECRETS/alert-webhook, and
# to Telegram if $SECRETS/telegram-token and $SECRETS/telegram-chat exist; https://<domain>/api/health serves the same checks for uptime
# monitors.
#
# Launch is safe to re-run; it reuses the chain it created. It caps what the
# bridge credits (MAX_DEPOSIT and MAX_CIRCULATING, in BTC) because one
# process holds every peg signer key.
set -euo pipefail

STATE=/var/lib/metal-main
SECRETS=$STATE/secrets
BIN=/opt/btcvm/bin
BTC_CONF=/var/lib/bitcoin-main/bitcoin.conf
DOMAIN=${DOMAIN:-metalbtc.com}
NODE_API=http://127.0.0.1:9660
MAX_DEPOSIT=${MAX_DEPOSIT:-0.01}
MAX_CIRCULATING=${MAX_CIRCULATING:-0.1}
CONFIRMATIONS=${CONFIRMATIONS:-6}
TIERS=${TIERS:-0.001:2,0.005:3}
VALIDATOR_BALANCE=${VALIDATOR_BALANCE:-5}

as_btcvm() { sudo -u btcvm env HOME=/opt/btcvm "$@"; }
satoshis() { awk -v d="$1" 'BEGIN { printf "%.0f", d * 100000000 }'; }
info_call() {
  curl -s -X POST -H 'content-type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":${2:-{\}}}" "$NODE_API/ext/$3"
}

cmd_status() {
  echo "Metal mainnet node:  $(info_call info.getNodeID '{}' info | jq -r .result.nodeID)"
  echo "P-Chain synced:      $(info_call info.isBootstrapped '{"chain":"P"}' info | jq -r .result.isBootstrapped)"
  echo "P-Chain key:         $(jq -r .pChainAddress "$SECRETS/p-chain-key.json")"
  echo "P-Chain balance:     $(as_btcvm "$BIN/btcvm-l1" balance -key "$SECRETS/p-chain-key.json" -uri "$NODE_API" 2>&1)"
  local d="as_btcvm /opt/bitcoin/bin/bitcoin-cli -datadir=/var/lib/bitcoin-main"
  echo "Bitcoin mainnet:    block $($d getblockcount) of $($d getblockchaininfo | jq .headers)"
  echo "Peg address:         $(jq -r .bitcoinPegAddress "$SECRETS/signers.out")"
  [[ -f "$STATE/chain.json" ]] && echo "L1:                  $(jq -c . "$STATE/chain.json")"
  return 0
}

cmd_launch() {
  [[ "$(info_call info.isBootstrapped '{"chain":"P"}' info | jq -r .result.isBootstrapped)" == true ]] ||
    { echo "the Metal node has not synced the P-Chain yet"; exit 1; }

  local reserve builder chain subnet
  reserve=$(jq -r .btcvmReserve "$SECRETS/signers.out")
  [[ -f "$SECRETS/builder.json" ]] || as_btcvm "$BIN/btcvm" keygen -vm-network mainnet -btc-network mainnet >"$SECRETS/builder.json"
  builder=$(jq -r .btcvmAddress "$SECRETS/builder.json")

  if [[ ! -f "$STATE/chain.json" ]]; then
    jq -n --arg reserve "$reserve" \
      '{config: {mainNet: true, pegReserveAddress: $reserve, pegReserveBlocks: 1}}' >"$STATE/genesis.json"
    as_btcvm "$BIN/btcvm-l1" create -key "$SECRETS/p-chain-key.json" -genesis "$STATE/genesis.json" \
      -node-uri "$NODE_API" -validator-balance "$VALIDATOR_BALANCE" >"$STATE/chain.json.tmp"
    mv "$STATE/chain.json.tmp" "$STATE/chain.json"
  fi
  chain=$(jq -r .chainID "$STATE/chain.json")
  subnet=$(jq -r .subnetID "$STATE/chain.json")

  # Node-local chain config: private RPC credentials, indexes, block builder.
  [[ -f "$SECRETS/rpc-password" ]] || openssl rand -hex 24 >"$SECRETS/rpc-password"
  install -d -o btcvm -g btcvm "$STATE/chain-configs/$chain"
  jq -n --arg pass "$(cat "$SECRETS/rpc-password")" --arg builder "$builder" \
    --arg data "$STATE/chaindata" --arg logs "$STATE/chainlogs" \
    '{rpcUser: "btcvm", rpcPass: $pass, rpcLimitUser: "public", rpcLimitPass: "public",
      txIndex: true, addrIndex: true, miningAddrs: [$builder], dataDir: $data, logDir: $logs}' \
    >"$STATE/chain-configs/$chain/config.json"
  chown -R btcvm:btcvm "$STATE" && chmod 700 "$SECRETS"

  # Track the L1's subnet.
  if ! grep -q -- "--track-subnets=$subnet" /etc/systemd/system/metal-mainnet.service; then
    sed -i "s|--public-ip=|--track-subnets=$subnet --public-ip=|" /etc/systemd/system/metal-mainnet.service
  fi
  # deploy/provision.sh builds the plugin into $STATE/plugins; re-run it to
  # update BTCVM.

  cat >"$SECRETS/bridge.env" <<ENV
BTCVM_RPC=$NODE_API/ext/bc/$chain/rpc
BTCVM_RPC_USER=btcvm
BTCVM_RPC_PASS=$(cat "$SECRETS/rpc-password")
BTCVM_NETWORK=mainnet
BITCOIN_RPC=http://127.0.0.1:8332
BITCOIN_RPC_USER=btcvm
BITCOIN_RPC_PASS=$(sed -n 's/^rpcpassword=//p' "$BTC_CONF")
BITCOIN_NETWORK=mainnet
ENV
  chown btcvm:btcvm "$SECRETS/bridge.env" && chmod 600 "$SECRETS/bridge.env"

  # The web server and monitor never sign, so they get the signer set
  # without its private keys: only the bridge holds those.
  jq 'del(.privateKeys)' "$SECRETS/signers.json" >"$SECRETS/signers.public.json"
  chown btcvm:btcvm "$SECRETS/signers.public.json"
  local policy="-signers $SECRETS/signers.json -confirmations $CONFIRMATIONS \
-max-deposit $(satoshis "$MAX_DEPOSIT") -max-circulating $(satoshis "$MAX_CIRCULATING") -min-fee-rate 1 -max-fee-rate 50 -confirmation-tiers $TIERS"
  local health="-validation-id $(jq -r .validationID "$STATE/chain.json") -pchain-uri $NODE_API/ext/bc/P"
  [[ -f "$SECRETS/alert-webhook" ]] && health="$health -webhook $(cat "$SECRETS/alert-webhook")"
  local alerts=""
  [[ -f "$SECRETS/telegram-token" && -f "$SECRETS/telegram-chat" ]] &&
    alerts="-telegram-token-file $SECRETS/telegram-token -telegram-chat $(cat "$SECRETS/telegram-chat")"
  for unit in bridge web monitor; do
    local exec="$BIN/btcvm bridge $policy -interval 30s"
    [[ $unit == web ]] && exec="$BIN/btcvm serve ${policy/signers.json/signers.public.json} ${health% -webhook*} -btc-index $STATE/btcindex -chain-id $chain -listen 127.0.0.1:8081"
    [[ $unit == monitor ]] && exec="$BIN/btcvm monitor ${policy/signers.json/signers.public.json} $health $alerts"
    cat >"/etc/systemd/system/btcvm-$unit-main.service" <<UNIT
[Unit]
Description=BTCVM $unit (Metal mainnet, Bitcoin mainnet)
After=metal-mainnet.service bitcoind-main.service

[Service]
User=btcvm
Environment=HOME=/opt/btcvm
EnvironmentFile=$SECRETS/bridge.env
ExecStart=$exec
Restart=always
RestartSec=15

[Install]
WantedBy=multi-user.target
UNIT
  done

  # metalbtc.com serves mainnet; the testnet stack is retired.
  cat >/etc/caddy/Caddyfile <<CADDY
$DOMAIN {
	header Strict-Transport-Security "max-age=31536000"
	# The macOS wallet's DMGs and signed update feed (bitcoin-vm-wallet's
	# publish-update.sh uploads them).
	handle_path /download/* {
		root * /var/www/metalbtc-downloads
		file_server
	}
	handle /rpc {
		rewrite * /ext/bc/$chain/rpc
		reverse_proxy 127.0.0.1:9660 {
			header_up Host localhost
		}
	}
	handle {
		reverse_proxy 127.0.0.1:8081
	}
}
CADDY
  systemctl disable --now btcvm-node btcvm-bridge btcvm-web >/dev/null 2>&1 || true
  systemctl daemon-reload
  systemctl restart metal-mainnet
  systemctl enable --now btcvm-bridge-main btcvm-web-main btcvm-monitor-main >/dev/null
  systemctl restart btcvm-bridge-main btcvm-web-main btcvm-monitor-main caddy
  cmd_status
}

case "${1:-}" in
  status) cmd_status ;;
  launch) cmd_launch ;;
  *) echo "usage: $0 status|launch" >&2; exit 2 ;;
esac
