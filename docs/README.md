## Overview

This repository uses a Makefile to automate building and running a local Metal network environment with the `metal-network-runner`. It also handles log output, cleanup, and the secret scanning hooks.

## Targets

- **build**  
  Builds the BTCVM plugin (from `cmd/btcvm-plugin`) into metalgo's plugin directory, under its VM ID.  
  ```bash
  make build
  ```

- **run_local**  
  Runs the local network with five nodes. With `filter` or `logs`, it captures logs per node and shortens IDs in the console output.  
  ```bash
  make run_local
  ```

- **cleanup**  
  Stops the network runner, metalgo and the plugin, and removes the built plugin.  
  ```bash
  make cleanup
  ```

- **hooks**  
  Installs the git hooks that refuse commits and pushes containing secrets.  
  ```bash
  make hooks
  ```

- **secretscan**  
  Scans every commit on every branch for secrets.  
  ```bash
  make secretscan
  ```

Use `make <target>` to run the commands above. For a single-node devnet with the bridge, see [TESTING.md](TESTING.md).


## How it works
- For detailed instructions, see `https://build.avax.network/docs/virtual-machines/golang-vms/complex-golang-vm`
- The VM is in `vm/vm.go`; it initializes the VM and runs the network.

- This code defines a Bitcoin virtual machine (BTCVM) running on the Metal Blockchain platform.
- It uses Metal's Snowman consensus, embedding a local btcd instance to hold the chain's state and apply Bitcoin's rules.
- The VM struct manages node context, database, mempool, block building, gossiping, and network interactions.
- Core methods include Initialize, Shutdown, SetState, LastAccepted, and block parsing/verification routines.
- This gives a self-contained environment where Bitcoin transactions and blocks are ordered by Metal's consensus and network layers.
- It builds a block only when transactions are waiting, at most about every 2 seconds, so a payment is final within seconds.

# Notes
- If building on Linux use `export CGO_ENABLED=1`
