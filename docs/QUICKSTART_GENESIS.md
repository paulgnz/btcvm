# Quick start: BTCVM genesis

A BTCVM genesis is a small JSON document, not a block: the genesis block itself is fixed in code and pays nothing. The genesis names the peg reserve, the only place BTC comes into existence on BTCVM. [GENESIS_SETUP.md](GENESIS_SETUP.md) has the details.

## 1. Make the signer set

```bash
go build -o btcvm ./cmd/btcvm
./btcvm signers -required 2 -total 3 -out signers.json -vm-network mainnet -btc-network mainnet
```

The output's `genesisConfigSnippet` holds the reserve address. `signers.json` contains private keys: never commit or share it, and back it up offline.

## 2. Write the genesis

```bash
RESERVE=$(./btcvm signers-check -signers signers.json -vm-network mainnet -btc-network mainnet | jq -r .btcvmReserve)
jq -n --arg reserve "$RESERVE" \
  '{config: {mainNet: true, pegReserveAddress: $reserve, pegReserveBlocks: 1}}' > genesis.json
```

## 3. Create the chain

```bash
go build -o btcvm-l1 ./cmd/btcvm-l1
./btcvm-l1 create -key p-chain-key.json -genesis genesis.json -node-uri http://127.0.0.1:9650
```

This creates the subnet and the BTCVM chain, and converts them to an L1 validated by your node. For a local devnet, `scripts/devnet.sh start` does all of this ([TESTING.md](TESTING.md)).

## Parameters

| Key | Description |
|---|---|
| `mainNet` / `testNet` | Which Bitcoin address and key encodings the chain uses |
| `pegReserveAddress` | The signers' P2WSH multisig address, the same on Bitcoin and BTCVM |
| `pegReserveBlocks` | Reserve blocks, each paying 20,999,000 BTC; 1 is enough |

## Security checklist

- Keys made on a machine you trust
- Private keys backed up offline
- No private key, WIF, password or token in any file you commit
- Tested with small amounts first

## Need help?

- **Full guide:** [GENESIS_SETUP.md](GENESIS_SETUP.md)
- **Running and testing:** [TESTING.md](TESTING.md)
- **The peg:** [BRIDGE.md](BRIDGE.md)
