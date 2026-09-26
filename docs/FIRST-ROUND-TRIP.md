# First round trip, then separate signers

What to do once the bridge's Bitcoin node has caught up: prove a deposit
and a withdrawal end to end on mainnet with a small amount, then move the
bridge onto separate signer services. BTCVM hasn't done this yet; this is
the plan for it. Commands run on the server, as root.

The paths and service names below follow the mainnet host set up by
`deploy/mainnet.sh`: state in `/var/lib/metal-main`, Bitcoin Core's data in
`/var/lib/bitcoin-main`, services run as the `btcvm` user.

## 0. The node has caught up

Bitcoin Core runs pruned (about 100 GB of disk) and takes about a day to
sync: it downloads and checks every block, keeping only recent ones. The monitor sends "all checks OK" to Telegram when the `bitcoin` check
passes. To check by hand:

```sh
curl -s https://metalbtc.com/api/health | jq '.status, (.checks[] | select(.ok | not))'
```

`status` should be `ok`, with nothing failing. The Bitcoin index then
catches up too: the web wallet's Bitcoin balance appears.

If the sync ran with a large `dbcache` in
`/var/lib/bitcoin-main/bitcoin.conf`, give the memory back: a synced node
doesn't need it on a server shared with metalgo and the bridge. Set
`dbcache` back down and restart Bitcoin Core at a quiet moment
(`systemctl restart bitcoind-main`; it takes a minute or two to come back).

## 1. A small deposit

From the web wallet (Deposit tab), move a small amount, above the minimum
deposit shown there, from your Bitcoin balance, or send it to your deposit
address from any Bitcoin wallet. It is credited once it has the
confirmations the Deposit tab shows for its size (6 by default, about an
hour; smaller deposits may need fewer), less the bridge fee.

- In the wallet: the BTCVM balance shows the amount less the bridge fee, and
  the Deposit tab lists the deposit as credited.
- In the explorer (`/explorer`): the deposit appears under Bridge activity
  with a transaction on each chain, and Proof of reserves lists the locked
  output.
- `curl -s https://metalbtc.com/api/status | jq .audit`: `locked` covers
  `circulating` plus anything pending, and the monitor's `peg` check passes.

If it isn't credited within a few minutes of reaching its confirmations, see
"A deposit that hasn't arrived" in [RUNBOOK.md](RUNBOOK.md).

## 2. A withdrawal back to Bitcoin

From the web wallet's Withdraw tab, withdraw at least the minimum to your
own Bitcoin address. Check the review screen: the amount to the bridge, and
the Bitcoin address it pays.

- The withdrawal is final on BTCVM in seconds; the bridge pays it on its
  next pass, less the Bitcoin network fee at the current fee rate. The
  payout confirms in the next Bitcoin block it makes, usually about 10
  minutes. If it is still unconfirmed after 30 minutes and fees have risen,
  the bridge replaces it with one paying the current rate.
- The explorer shows the withdrawal with both transactions; the Bitcoin one
  links to a public explorer.
- `/api/status` audit still balances, and `/api/health` stays `ok`.

## 3. Record it

Note the round trip's transaction IDs (deposit, credit, withdrawal, payout),
then update the roadmap: "Where it stands", the "Next: the first round
trip" section, and the "Prove the round trip" phase. That's the end-to-end
proof.

## 4. Separate signers

This moves the bridge onto the separate-signer design on this server: three
signer services, one key each, each checking every transaction against the
nodes before signing, and a coordinator that holds no keys. The peg address
doesn't change. It's a rehearsal of the protocol with live funds; the
security gain comes when signers move to other operators' machines
([SIGNERS.md](SIGNERS.md)).

Do it at a quiet moment: nothing pending in either direction
(`/api/status` audit `pendingPegIns` and `pendingPegOuts` are 0).

### Stage

```sh
cd /opt/btcvm/src && sudo -u btcvm git pull -q
deploy/stage-signers.sh
```

It copies each key to its own signer directory under
`/var/lib/metal-main/secrets/separate-signers`, assembles and joins the set,
checks the keys and their order match the live set's, and writes a service
file per signer. It starts nothing. `rm -r` the directory undoes it.

Before installing the signers, refresh each one's copy of the deposit
registry, since addresses registered after staging aren't in it:

```sh
S=/var/lib/metal-main/secrets/separate-signers
for n in 1 2 3; do [ -f /var/lib/metal-main/secrets/deposits.json ] && sudo -u btcvm cp /var/lib/metal-main/secrets/deposits.json $S/signer$n/deposits.json; done
```

### Install and start the signers

```sh
deploy/install-signers.sh
```

It gives each signer its own system user (`btcvm-signer-N`), moves its
directory to `/var/lib/btcvm-signer-N` (mode 700), gives it its own
watch-only Bitcoin Core wallet, runs it from a root-owned copy of the binary
(`/usr/local/lib/btcvm/btcvm`, which `deploy/provision.sh` keeps up to date),
starts it, and runs `signer-setup check`. Every check should pass, including
that each signer refuses an unsigned request; only "still syncing" is
tolerated while Bitcoin Core catches up.

### Switch the bridge

In `/etc/systemd/system/btcvm-bridge-main.service`, change `-signers` in
`ExecStart` and add the coordinator flags, keeping the policy flags as they
are:

```
-signers /var/lib/metal-main/secrets/separate-signers/signers.json
-cosigners /var/lib/metal-main/secrets/separate-signers/cosigners.json
-coordinator-key-file /var/lib/metal-main/secrets/separate-signers/coordinator/coordinator.key
```

Point the web and monitor services' `-signers` at the same public set, and
give all three `-deposits /var/lib/metal-main/secrets/deposits.json`, so the
registry stays where it was (by default it sits next to `-signers`).
`deploy/mainnet.sh launch` writes these units itself once the staged set
exists. Then:

```sh
systemctl daemon-reload
systemctl restart btcvm-bridge-main btcvm-web-main btcvm-monitor-main
journalctl -u btcvm-bridge-main -u btcvm-signer-1 -u btcvm-signer-2 -u btcvm-signer-3 -f
```

### Prove it

Make a small deposit and a small withdrawal again. Each signer's log shows
it checking and signing; `signing-log.json` in each signer directory records
what it signed. Health stays `ok`.

### Retire the combined key file

Move `/var/lib/metal-main/secrets/signers.json` (and `signers.public.json`)
out of service at once, to a root-only directory such as
`/root/retired-keys`, so no command can run against the old set by mistake:
a pause written next to it wouldn't reach the bridge. Once a backup has run
with the signer directories in it (`btcvm-backup run`, then
`scripts/restore-check.sh` on your Mac), delete it, and let old backups that
hold it age out. From then on each key exists only in its signer's directory
and in the backups.

### Roll back

Stop the signers, move the retired `signers.json` and `signers.public.json`
back, put the old `-signers` paths back in the three services,
drop the coordinator flags, and restart. The peg address never changed, so
nothing on either chain needs to move.

```sh
systemctl disable --now btcvm-signer-1 btcvm-signer-2 btcvm-signer-3
```
