// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package btcd

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/txscript"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// activeNetParams is a pointer to the parameters specific to the
// currently active BTCVM network.
var activeNetParams = &btcVMTestNetParams

// params is used to group parameters for various networks such as the main
// network and test networks.
type params struct {
	*chaincfg.Params
	rpcPort string
}

const (
	// Network magics for BTCVM. These are deliberately different from
	// Bitcoin Core's (mainnet f9beb4d9, testnet3 0b110907) so that a BTCVM
	// node can never be mistaken for a Bitcoin proof-of-work peer.
	btcVMMainNet wire.BitcoinNet = 0xb7c0e7a1
	btcVMTestNet wire.BitcoinNet = 0xb7c0e7a2
)

var (
	bigOne           = big.NewInt(1)
	btcVMPowLimit    = new(big.Int).Sub(new(big.Int).Lsh(bigOne, 255), bigOne)
	btcVMGenesisTime = time.Unix(1790294400, 0) // 2026-09-25T00:00:00Z
)

// newGenesisBlock returns a BTCVM genesis block. The coinbase pays nothing
// to a provably unspendable script: there is no premine, and BTC only enters
// the BTCVM ledger by being locked on Bitcoin mainnet.
func newGenesisBlock(message string) *wire.MsgBlock {
	pkScript, err := txscript.NullDataScript(nil)
	if err != nil {
		panic(err)
	}

	coinbase := &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
			SignatureScript:  []byte(message),
			Sequence:         wire.MaxTxInSequenceNum,
		}},
		TxOut: []*wire.TxOut{{Value: 0, PkScript: pkScript}},
	}

	return &wire.MsgBlock{
		Header: wire.BlockHeader{
			Version:    1,
			MerkleRoot: coinbase.TxHash(),
			Timestamp:  btcVMGenesisTime,
			Bits:       0x1d00ffff,
		},
		Transactions: []*wire.MsgTx{coinbase},
	}
}

var (
	btcVMMainNetGenesisBlock = newGenesisBlock("BTCVM genesis - mainnet")
	btcVMMainNetGenesisHash  = btcVMMainNetGenesisBlock.BlockHash()

	btcVMTestNetGenesisBlock = newGenesisBlock("BTCVM genesis - testnet")
	btcVMTestNetGenesisHash  = btcVMTestNetGenesisBlock.BlockHash()
)

// activeFromBlockOne returns a deployment active from block 1, without
// signalling, as Bitcoin's buried deployments are.
func activeFromBlockOne(bit uint8) chaincfg.ConsensusDeployment {
	return chaincfg.ConsensusDeployment{
		BitNumber: bit,
		DeploymentStarter: chaincfg.NewMedianTimeDeploymentStarter(
			time.Time{}, // Always available for vote
		),
		DeploymentEnder: chaincfg.NewMedianTimeDeploymentEnder(
			time.Time{}, // Never expires
		),
		AlwaysActiveHeight: 1,
	}
}

// newBTCVMParams returns the consensus parameters shared by every BTCVM
// network. Blocks are ordered by Snowman, not proof of work, so the
// difficulty fields only need to be self-consistent; the PoW check itself is
// disabled in blockchain.checkProofOfWork.
func newBTCVMParams() chaincfg.Params {
	return chaincfg.Params{
		DNSSeeds: []chaincfg.DNSSeed{}, // NOTE: There must NOT be any seeds.

		// Chain parameters
		PowLimit:                 btcVMPowLimit,
		PowLimitBits:             0x1d00ffff,
		BIP0034Height:            0, // Always active
		BIP0065Height:            0, // Always active
		BIP0066Height:            0, // Always active
		CoinbaseMaturity:         0,
		SubsidyReductionInterval: 210000,
		TargetTimespan:           time.Hour * 24 * 14, // 14 days
		TargetTimePerBlock:       time.Minute * 10,    // 10 minutes
		RetargetAdjustmentFactor: 4,                   // 25% less, 400% more
		ReduceMinDifficulty:      true,
		MinDiffReductionTime:     time.Minute * 20, // TargetTimePerBlock * 2
		GenerateSupported:        true,

		// BTCVM never mints BTC through the coinbase: Bitcoin's own
		// issuance continues on Bitcoin, and mirroring it here would
		// unback the peg.
		NoBlockSubsidy: true,

		// Checkpoints ordered from oldest to newest.
		Checkpoints: nil,

		// Consensus rule change deployments. Every rule Bitcoin has
		// buried is active from block 1, so BTCVM runs today's Bitcoin
		// rules from the start with no signalling window.
		RuleChangeActivationThreshold: 75, // 75% of MinerConfirmationWindow
		MinerConfirmationWindow:       100,
		Deployments: [chaincfg.DefinedDeployments]chaincfg.ConsensusDeployment{
			chaincfg.DeploymentTestDummy: {
				BitNumber: 28,
				DeploymentStarter: chaincfg.NewMedianTimeDeploymentStarter(
					time.Time{}, // Always available for vote
				),
				DeploymentEnder: chaincfg.NewMedianTimeDeploymentEnder(
					time.Time{}, // Never expires
				),
			},
			chaincfg.DeploymentTestDummyMinActivation: {
				BitNumber:                 22,
				CustomActivationThreshold: 50,  // Only needs 50% hash rate.
				MinActivationHeight:       600, // Can only activate after height 600.
				DeploymentStarter: chaincfg.NewMedianTimeDeploymentStarter(
					time.Time{}, // Always available for vote
				),
				DeploymentEnder: chaincfg.NewMedianTimeDeploymentEnder(
					time.Time{}, // Never expires
				),
			},
			chaincfg.DeploymentCSV:                   activeFromBlockOne(0),
			chaincfg.DeploymentSegwit:                activeFromBlockOne(1),
			chaincfg.DeploymentTaproot:               activeFromBlockOne(2),
			chaincfg.DeploymentTestDummyAlwaysActive: activeFromBlockOne(29),
		},

		// Mempool parameters. Standardness (Bitcoin Core's dust and
		// script rules) is enforced on every network.
		RelayNonStdTxs: false,
	}
}

// BTCVMMainNetParams defines the network parameters for BTCVM mainnet.
// Address and key encodings are identical to Bitcoin mainnet, so a 1..., 3...
// or bc1... address is the same address on both ledgers.
var BTCVMMainNetParams = func() chaincfg.Params {
	p := newBTCVMParams()
	p.Name = "btcvm"
	p.Net = btcVMMainNet
	// BTCVM does not listen for P2P connections; ports sit next to Bitcoin
	// Core's (8333/8332) without colliding with them.
	p.DefaultPort = "8336"
	p.GenesisBlock = btcVMMainNetGenesisBlock
	p.GenesisHash = &btcVMMainNetGenesisHash

	// Address encoding magics (Bitcoin Core chainparams.cpp, CMainParams)
	p.Bech32HRPSegwit = "bc"
	p.PubKeyHashAddrID = 0x00 // starts with 1
	p.ScriptHashAddrID = 0x05 // starts with 3
	p.PrivateKeyID = 0x80     // starts with 5 (uncompressed) or K/L (compressed)
	p.WitnessPubKeyHashAddrID = 0x06
	p.WitnessScriptHashAddrID = 0x0A

	// BIP32 hierarchical deterministic extended key magics
	p.HDPrivateKeyID = [4]byte{0x04, 0x88, 0xad, 0xe4} // starts with xprv
	p.HDPublicKeyID = [4]byte{0x04, 0x88, 0xb2, 0x1e}  // starts with xpub

	// SLIP-0044 coin type for Bitcoin.
	p.HDCoinType = 0
	return p
}()

// BTCVMTestNetParams defines the network parameters for BTCVM testnet,
// which shares Bitcoin testnet's address and key encodings.
var BTCVMTestNetParams = func() chaincfg.Params {
	p := newBTCVMParams()
	p.Name = "btcvmtestnet"
	p.Net = btcVMTestNet
	p.DefaultPort = "18336"
	p.GenesisBlock = btcVMTestNetGenesisBlock
	p.GenesisHash = &btcVMTestNetGenesisHash

	// Address encoding magics (Bitcoin Core chainparams.cpp, CTestNetParams)
	p.Bech32HRPSegwit = "tb"
	p.PubKeyHashAddrID = 0x6f // starts with m or n
	p.ScriptHashAddrID = 0xc4 // starts with 2
	p.PrivateKeyID = 0xef     // starts with 9 (uncompressed) or c (compressed)
	p.WitnessPubKeyHashAddrID = 0x03
	p.WitnessScriptHashAddrID = 0x28

	// BIP32 hierarchical deterministic extended key magics
	p.HDPrivateKeyID = [4]byte{0x04, 0x35, 0x83, 0x94} // starts with tprv
	p.HDPublicKeyID = [4]byte{0x04, 0x35, 0x87, 0xcf}  // starts with tpub

	// SLIP-0044 coin type for all testnets.
	p.HDCoinType = 1
	return p
}()

// PegReserveAmountPerBlock is what each peg reserve coinbase pays into the
// reserve: 20,999,000 BTC, all the bitcoin there will ever be less 1,000,
// leaving room under the 21 million BTC per-transaction limit for the
// block's fees.
const PegReserveAmountPerBlock = 20_999_000 * 1e8

// withPegReserve returns a copy of p whose chain params lock blocks worth of
// PegReserveAmountPerBlock to address.
func withPegReserve(p *params, address string, blocks int32) (*params, error) {
	if address == "" || blocks < 1 {
		return nil, errors.New("pegReserveAddress and a positive " +
			"pegReserveBlocks must be set together")
	}
	addr, err := btcutil.DecodeAddress(address, p.Params)
	if err != nil {
		return nil, fmt.Errorf("invalid pegReserveAddress: %w", err)
	}
	if !addr.IsForNet(p.Params) {
		return nil, fmt.Errorf("pegReserveAddress %s is not for %s",
			address, p.Name)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}

	chainParams := *p.Params
	chainParams.PegReserve = &chaincfg.PegReserve{
		PkScript: pkScript,
		Amount:   PegReserveAmountPerBlock,
		Blocks:   blocks,
	}
	return &params{Params: &chainParams, rpcPort: p.rpcPort}, nil
}

var btcVMMainNetParams = params{
	Params:  &BTCVMMainNetParams,
	rpcPort: "8335",
}

var btcVMTestNetParams = params{
	Params:  &BTCVMTestNetParams,
	rpcPort: "18335",
}

// netName returns the name used when referring to a BTCVM network, which
// is also the data and log directory name.
func netName(chainParams *params) string {
	return chainParams.Name
}

// Register the BTCVM networks so address and extended key lookups recognise
// their encodings. They share Bitcoin's, and differ only in network magic.
func init() {
	for _, p := range []*chaincfg.Params{&BTCVMMainNetParams, &BTCVMTestNetParams} {
		if err := chaincfg.Register(p); err != nil {
			panic(err)
		}
	}
}
