# Running and testing BTCVM

## Mainnet

BTCVM runs as an L1 on Metal mainnet, pegged to Bitcoin mainnet, set up with [`deploy/mainnet.sh`](../deploy/mainnet.sh) and [`cmd/btcvm-l1`](../cmd/btcvm-l1). The bridge needs its own Bitcoin Core node with `-txindex` (about 700 GB), fully synced, before it can credit or pay anything. Once it has synced, the first step is a small round trip: see [FIRST-ROUND-TRIP.md](FIRST-ROUND-TRIP.md).

The beta is capped (a largest deposit and a most-circulating total, shown on the site and at `/api/info`), and at first one server holds every peg signer key. Once launched, the web wallet, explorer and JSON-RPC (`/rpc`) are served at **https://metalbtc.com**. Once the L1 is created, anyone can check it on the P-Chain, for example with `platform.getL1Validator`.

## Local setup

This runs everything on your machine for development: a Metal node running BTCVM, Bitcoin Core on regtest, and a BTC round trip through the peg. Regtest coins have no value; it's a way to exercise the code, not a network.

### What you need

- Go 1.24+, `jq`, `openssl`.
- **metalgo v1.13.5.** The plugin speaks rpcchainvm protocol 43 and will not load in other versions.
  ```bash
  git clone --depth 1 --branch v1.13.5 https://github.com/MetalBlockchain/metalgo
  (cd metalgo && ./scripts/build.sh)          # -> metalgo/build/metalgo
  ```
- **Bitcoin Core**, from [bitcoincore.org](https://bitcoincore.org/en/download/). Check the download against the release's `SHA256SUMS` and its signatures. Run it on regtest with a transaction index, and make a wallet with some mature coins:
  ```bash
  bitcoind -regtest -daemon -txindex=1 -fallbackfee=0.0001
  alias bcli='bitcoin-cli -regtest'
  bcli createwallet dev
  bcli -generate 101                            # mature some regtest coins
  ```
  Regtest's RPC port is 18443, and the RPC login is Bitcoin Core's cookie file unless you set `-rpcuser`/`-rpcpassword`.

### 1. Start a BTCVM devnet

```bash
METALGO=/path/to/metalgo/build/metalgo scripts/devnet.sh start
eval "$(scripts/devnet.sh env)"
```

This builds the plugin from [`cmd/btcvm-plugin`](../cmd/btcvm-plugin) under its VM ID (`kMtihm7W3KssmcJb9mzwZfC6gkiPrJhWaa5KMLHdEB9R8Q4pp`) and creates a 2-of-3 peg signer set. It then starts a single metalgo node on a local network with sybil protection off, and creates a subnet and chain with a one-block (20,999,000 BTC) peg reserve. `scripts/devnet.sh stop` stops the node, and deleting its state directory starts over.

### 2. Connect

BTCVM speaks btcd's JSON-RPC (Bitcoin Core-style) at:

```
http://127.0.0.1:9650/ext/bc/<chainID>/rpc      (HTTP basic auth: BTCVM_RPC_USER / BTCVM_RPC_PASS)
```

```bash
curl -s -u "$BTCVM_RPC_USER:$BTCVM_RPC_PASS" -H 'content-type: application/json' \
  -d '{"jsonrpc":"1.0","id":1,"method":"getblockcount","params":[]}' "$BTCVM_RPC"
```

RPC credentials and indexes are node settings, in the chain's config file (`chain-configs/<chainID>/config.json` in the devnet's state directory), not in the public genesis. The node needs `txIndex` and `addrIndex` for the wallet and bridge. For a shared node, set `rpcLimitUser`/`rpcLimitPass` there: a limited user can read and broadcast but not administer.

### 3. Wallets

Keys and addresses are Bitcoin's, so one key has the same address on both chains. No existing Bitcoin wallet can talk to BTCVM yet, because it is not reachable over Bitcoin's P2P network or Electrum. Use the web wallet (`btcvm serve`) or the `btcvm` CLI:

```bash
go build -o btcvm ./cmd/btcvm
export BITCOIN_RPC=http://127.0.0.1:18443 BITCOIN_NETWORK=regtest
export BITCOIN_RPC_USER=<your rpcuser> BITCOIN_RPC_PASS=<your rpcpassword>

./btcvm keygen                                   # key, with BTCVM and Bitcoin addresses
./btcvm balance -address <btcvm address>
./btcvm send -key <key> -to <btcvm address> -amount 0.1
```

`keygen` prints the private key as a Bitcoin WIF too, so the same key can be imported into a Bitcoin Core descriptor wallet.

### 4. Peg in and out

```bash
SIGNERS=<devnet state directory>/signers.json

# Run the bridge (in its own terminal); it polls both chains.
./btcvm bridge -signers $SIGNERS -rescan

# Peg in: deposit 1 BTC from the Bitcoin Core wallet, credited to a BTCVM address.
./btcvm peg-in -signers $SIGNERS -from-wallet dev -to <btcvm address> -amount 1
bcli -generate 6                                 # confirmations; the bridge then credits 0.99999 (less the bridge fee)

# Peg out: send 0.5 BTC back to a Bitcoin address.
./btcvm peg-out -signers $SIGNERS -key <key> -to <bitcoin address> -amount 0.5
bcli -generate 1                                 # the bridge pays 0.5 BTC less the Bitcoin network fee

# Check the peg is fully backed.
./btcvm audit -signers $SIGNERS
```

The bridge logs each credit and payout, and the audit should report `"solvent": true`, with `locked` covering `circulating` plus anything pending.

See [BRIDGE.md](BRIDGE.md) for how the peg works and its trust model.

## Automated tests

```bash
go test ./vm/ ./cmd/... ./btcd/ ./btcd/mempool/
```

- `vm/`: block lifecycle, peg reserve consensus and a multisig reserve release, run in-process against a real btcd.
- `cmd/btcvm/`: bridge logic against in-memory chains that run btcd's script engine on every signed input: exactly-once crediting, spoofed tags, personal SegWit deposit addresses, fee rates and replaceable payouts, a round trip, and halting when insolvent. Also checks the web wallet's `chain.js` against the Go code under Node, when Node is installed.
