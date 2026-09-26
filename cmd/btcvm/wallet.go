package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/MetalBlockchain/btcvm/btcd/btcec/v2"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/txscript"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// vmFee is what the command-line wallet pays for a BTCVM transaction of
// vsize: the node's relay minimum of 1 sat/kvB, and never less than a
// satoshi (btcd's rule), so a payment costs 1 sat.
func vmFee(vsize int64) int64 {
	return max(vsize/1000, 1)
}

func newKey() (*btcec.PrivateKey, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	key, _ := btcec.PrivKeyFromBytes(secret[:])
	return key, nil
}

// keyReport shows one key in both chains' encodings.
type keyReport struct {
	PrivateKeyHex string `json:"privateKeyHex"`
	BTCVMWIF      string `json:"btcvmWIF"`
	BTCVMAddr     string `json:"btcvmAddress"`
	BitcoinWIF    string `json:"bitcoinWIF"`
	BitcoinAddr   string `json:"bitcoinAddress"`
}

func describeKey(key *btcec.PrivateKey, vmParams, btcParams *chaincfg.Params) (keyReport, error) {
	r := keyReport{PrivateKeyHex: hex.EncodeToString(key.Serialize())}
	for _, enc := range []struct {
		params    *chaincfg.Params
		wif, addr *string
	}{
		{vmParams, &r.BTCVMWIF, &r.BTCVMAddr},
		{btcParams, &r.BitcoinWIF, &r.BitcoinAddr},
	} {
		wif, err := btcutil.NewWIF(key, enc.params, true)
		if err != nil {
			return r, err
		}
		addr, err := keyAddress(key, enc.params)
		if err != nil {
			return r, err
		}
		*enc.wif, *enc.addr = wif.String(), addr.EncodeAddress()
	}
	return r, nil
}

// keyAddress is key's native SegWit (P2WPKH, bc1q...) address, the
// wallet's default on both chains.
func keyAddress(key *btcec.PrivateKey, params *chaincfg.Params) (*btcutil.AddressWitnessPubKeyHash, error) {
	return btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(key.PubKey().SerializeCompressed()), params)
}

// parseKey accepts a WIF for either chain or a hex private key.
func parseKey(s string) (*btcec.PrivateKey, error) {
	if wif, err := btcutil.DecodeWIF(s); err == nil {
		return wif.PrivKey, nil
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("key must be a WIF or 32-byte hex private key")
	}
	key, _ := btcec.PrivKeyFromBytes(raw)
	return key, nil
}

// estimateVSize is a conservative virtual size for a transaction spending
// P2WPKH inputs to outputs of up to 43 bytes each, plus an OP_RETURN of
// opReturnBytes, used to set its fee before signing.
func estimateVSize(inputs, outputs int, opReturnBytes int) int64 {
	// Per input: 41 bytes, plus a witness of a 73-byte signature and a
	// 33-byte key, 108 weight units.
	weight := 4*(11+41*inputs+43*outputs) + 2 + 108*inputs
	if opReturnBytes > 0 {
		weight += 4 * (9 + opReturnBytes)
	}
	return int64((weight + 3) / 4)
}

// payFromKey sends amount to script from key's P2WPKH outputs on c, adding
// extra (an OP_RETURN) if given, with change back to the key.
func payFromKey(c chain, params *chaincfg.Params, key *btcec.PrivateKey,
	script []byte, amount int64, extra *wire.TxOut) (chainhash.Hash, error) {

	from, err := keyAddress(key, params)
	if err != nil {
		return chainhash.Hash{}, err
	}
	utxos, err := c.unspent([]btcutil.Address{from}, 1)
	if err != nil {
		return chainhash.Hash{}, err
	}

	opReturnBytes := 0
	if extra != nil {
		opReturnBytes = len(extra.PkScript)
	}
	// Grow the input set until it covers the amount and its own fee.
	var inputs []utxo
	var total, fee int64
	for n := 1; ; n++ {
		fee = vmFee(estimateVSize(n, 2, opReturnBytes))
		inputs, total, err = selectUTXOs(utxos, amount+fee)
		if err != nil {
			return chainhash.Hash{}, fmt.Errorf("%s: %w", from.EncodeAddress(), err)
		}
		if len(inputs) <= n {
			break
		}
	}

	fromScript := destinationScript(from)
	tx := wire.NewMsgTx(2)
	fetcher := txscript.NewMultiPrevOutFetcher(nil)
	for _, u := range inputs {
		tx.AddTxIn(wire.NewTxIn(&u.outPoint, nil, nil))
		fetcher.AddPrevOut(u.outPoint, wire.NewTxOut(u.value, fromScript))
	}
	tx.AddTxOut(wire.NewTxOut(amount, script))
	if extra != nil {
		tx.AddTxOut(extra)
	}
	if change := total - amount - fee; change > 0 {
		tx.AddTxOut(wire.NewTxOut(change, fromScript))
	}

	hashes := txscript.NewTxSigHashes(tx, fetcher)
	for i, u := range inputs {
		witness, err := txscript.WitnessSignature(tx, hashes, i, u.value, fromScript, txscript.SigHashAll, key, true)
		if err != nil {
			return chainhash.Hash{}, err
		}
		tx.TxIn[i].Witness = witness
	}
	return c.send(tx)
}

// balance sums address's unspent outputs.
func balance(c chain, address btcutil.Address) (confirmed, pending int64, err error) {
	utxos, err := c.unspent([]btcutil.Address{address}, 0)
	if err != nil {
		return 0, 0, err
	}
	for _, u := range utxos {
		if u.confirmations > 0 {
			confirmed += u.value
		} else {
			pending += u.value
		}
	}
	return confirmed, pending, nil
}

// depositFromBitcoinCore sends amount from the Bitcoin Core wallet behind
// rpc to the peg address, tagged to credit dest on BTCVM.
func depositFromBitcoinCore(rpc *rpcClient, pegAddr btcutil.Address, dest destination, amount int64) (string, error) {
	outputs := map[string]any{
		pegAddr.EncodeAddress(): json.Number(formatBTC(amount)),
		"data":                  hex.EncodeToString(encodeDestination(tagDeposit, dest)),
	}
	var raw string
	if err := rpc.call(&raw, "createrawtransaction", []any{}, outputs); err != nil {
		return "", err
	}
	var funded struct {
		Hex string `json:"hex"`
	}
	// The bridge's fee estimate would do; Bitcoin Core picks its own.
	if err := rpc.call(&funded, "fundrawtransaction", raw); err != nil {
		return "", err
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := rpc.call(&signed, "signrawtransactionwithwallet", funded.Hex); err != nil {
		return "", err
	}
	if !signed.Complete {
		return "", fmt.Errorf("Bitcoin Core could not sign the deposit (is its wallet unlocked?)")
	}
	var txid string
	err := rpc.call(&txid, "sendrawtransaction", signed.Hex)
	return txid, err
}
