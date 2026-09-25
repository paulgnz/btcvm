# Genesis setup

This guide covers what goes into a BTCVM chain's genesis, and how to make one.

## What the genesis holds

BTCVM's genesis block is fixed in code ([`btcd/params.go`](../btcd/params.go)), one per network. Its coinbase pays nothing, to an unspendable script: there is no premine and no mining reward. BTC comes into existence only in the peg reserve, which consensus creates in the chain's first blocks and locks to the peg signers.

So the genesis data given to `platform.createBlockchain` is not a block. It is a small JSON document with the settings every node must agree on:

```json
{
  "config": {
    "mainNet": true,
    "pegReserveAddress": "bc1q…",
    "pegReserveBlocks": 1
  }
}
```

| Key | Meaning |
|---|---|
| `mainNet` / `testNet` | The network: Bitcoin mainnet or testnet address and key encodings. |
| `pegReserveAddress` | The signers' P2WSH multisig address. Blocks 1..`pegReserveBlocks` must each pay the reserve to it. |
| `pegReserveBlocks` | How many reserve blocks, each paying 20,999,000 BTC. One is enough: a single output can hold at most 21 million BTC, Bitcoin's whole supply. |

These are consensus settings: a node's own chain config can't override them. Node settings such as RPC credentials and indexes go in the chain config instead ([TESTING.md](TESTING.md)).

## Making a genesis

### 1. Make the signer set

```bash
go build -o btcvm ./cmd/btcvm
./btcvm signers -required 2 -total 3 -out signers.json -vm-network mainnet -btc-network mainnet
```

It prints the reserve address on BTCVM, the peg address on Bitcoin (the same `bc1q…` address) and a `genesisConfigSnippet` to paste into the genesis. `signers.json` holds the signers' private keys: keep it off any repository and back it up offline. For separate signers, each operator makes their own key with `btcvm signer-key` and you pass only the public keys with `-public-keys`, so the file holds no private keys ([SIGNERS.md](SIGNERS.md)).

### 2. Write the genesis

```bash
RESERVE=$(./btcvm signers-check -signers signers.json -vm-network mainnet -btc-network mainnet | jq -r .btcvmReserve)
jq -n --arg reserve "$RESERVE" \
  '{config: {mainNet: true, pegReserveAddress: $reserve, pegReserveBlocks: 1}}' > genesis.json
```

### 3. Create the chain

On a Metal network, [`cmd/btcvm-l1`](../cmd/btcvm-l1) creates the subnet and the chain from `genesis.json`, and converts them to an L1 validated by your node:

```bash
go build -o btcvm-l1 ./cmd/btcvm-l1
./btcvm-l1 create -key p-chain-key.json -genesis genesis.json -node-uri http://127.0.0.1:9650
```

For a local devnet, `scripts/devnet.sh start` does all three steps ([TESTING.md](TESTING.md)).

## Verifying the genesis

After the chain starts, ask the node for its first blocks:

```bash
curl -s -u "$BTCVM_RPC_USER:$BTCVM_RPC_PASS" -H 'content-type: application/json' \
  -d '{"jsonrpc":"1.0","id":1,"method":"getblockhash","params":[0]}' "$BTCVM_RPC"
```

Check that:
- block 0's coinbase pays nothing;
- block 1 (and each reserve block) pays the reserve to your `pegReserveAddress`.

## The genesis-generator tool

[`cmd/genesis-generator`](../cmd/genesis-generator) is a development utility. BTCVM doesn't use the blocks it makes, since its genesis is fixed in code, but two of its commands are still handy:

```bash
cd cmd/genesis-generator
go run main.go -generate          # a new key pair, with its addresses
go run main.go -hex2go <hex>      # hex as a Go byte array, with ASCII comments
```

`-generate` prints the private key to the terminal. Run it on a machine you trust, don't redirect its output into a file you might commit, and never paste the key into an issue, a chat or a document.

`-hex2go` turns a hex string into a Go byte array with eight bytes per line and a comment showing the printable characters, which is useful for test vectors and scripts:

```bash
go run main.go -hex2go "425443564d2067656e65736973"    # "BTCVM genesis"
```

## Security

- Make keys on a machine you trust, and back them up offline.
- Never commit a private key, signer set with private keys, WIF, password or token. The repository's hooks and CI scan every commit and push for them ([SIGNERS.md](SIGNERS.md#secrets)).
- Never send a private key over the internet or share it with anyone.
- Start with small amounts.
