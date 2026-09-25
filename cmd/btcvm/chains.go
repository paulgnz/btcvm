package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// chainTx is a transaction and how many blocks confirm it (0 in mempool).
type chainTx struct {
	tx            *wire.MsgTx
	confirmations int64
	time          int64 // unix seconds: block time, or when first seen
}

// utxo is an unspent output.
type utxo struct {
	outPoint      wire.OutPoint
	value         int64
	pkScript      []byte
	confirmations int64
}

// chain is what the bridge and wallet need from either ledger.
type chain interface {
	// txsFor returns every transaction, confirmed or in the mempool, that
	// pays to or spends from any of addresses.
	txsFor(addresses []btcutil.Address) ([]chainTx, error)
	// unspent returns the addresses' unspent outputs with at least minConf
	// confirmations.
	unspent(addresses []btcutil.Address, minConf int64) ([]utxo, error)
	// send broadcasts tx.
	send(tx *wire.MsgTx) (chainhash.Hash, error)
}

func decodeTx(hexTx string) (*wire.MsgTx, error) {
	raw, err := hex.DecodeString(hexTx)
	if err != nil {
		return nil, err
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return &tx, nil
}

func encodeTx(tx *wire.MsgTx) string {
	var buf bytes.Buffer
	_ = tx.Serialize(&buf)
	return hex.EncodeToString(buf.Bytes())
}

// vmChain reads BTCVM through its btcd JSON-RPC. The node must run with
// txIndex and addrIndex enabled.
type vmChain struct {
	rpc *rpcClient
}

func (c *vmChain) txsFor(addresses []btcutil.Address) ([]chainTx, error) {
	seen := map[chainhash.Hash]bool{}
	var txs []chainTx
	for _, address := range addresses {
		found, err := c.addressTxs(address)
		if err != nil {
			return nil, err
		}
		for _, t := range found {
			if hash := t.tx.TxHash(); !seen[hash] {
				seen[hash] = true
				txs = append(txs, t)
			}
		}
	}
	return txs, nil
}

func (c *vmChain) addressTxs(address btcutil.Address) ([]chainTx, error) {
	const pageSize = 500
	var txs []chainTx
	for skip := 0; ; skip += pageSize {
		var page []struct {
			Hex           string `json:"hex"`
			Confirmations int64  `json:"confirmations"`
			Time          int64  `json:"time"`
		}
		err := c.rpc.call(&page, "searchrawtransactions", address.EncodeAddress(), 1, skip, pageSize)
		if isRPCCode(err, errNoAddressInfo) {
			return txs, nil
		}
		if err != nil {
			return nil, err
		}
		for _, r := range page {
			tx, err := decodeTx(r.Hex)
			if err != nil {
				return nil, err
			}
			txs = append(txs, chainTx{tx: tx, confirmations: r.Confirmations, time: r.Time})
		}
		if len(page) < pageSize {
			return txs, nil
		}
	}
}

func (c *vmChain) unspent(addresses []btcutil.Address, minConf int64) ([]utxo, error) {
	txs, err := c.txsFor(addresses)
	if err != nil {
		return nil, err
	}
	scripts := scriptSet(addresses)

	var utxos []utxo
	for _, t := range txs {
		if t.confirmations < minConf {
			continue
		}
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if !scripts[string(out.PkScript)] {
				continue
			}
			// gettxout returns null once the output is spent, including
			// by a transaction still in the mempool.
			var result *struct {
				Confirmations int64 `json:"confirmations"`
			}
			if err := c.rpc.call(&result, "gettxout", hash.String(), i, true); err != nil {
				return nil, err
			}
			if result == nil {
				continue
			}
			utxos = append(utxos, utxo{
				outPoint:      wire.OutPoint{Hash: hash, Index: uint32(i)},
				value:         out.Value,
				pkScript:      out.PkScript,
				confirmations: t.confirmations,
			})
		}
	}
	return utxos, nil
}

func (c *vmChain) send(tx *wire.MsgTx) (chainhash.Hash, error) {
	return sendRaw(c.rpc, tx)
}

func sendRaw(rpc *rpcClient, tx *wire.MsgTx) (chainhash.Hash, error) {
	var txid string
	if err := rpc.call(&txid, "sendrawtransaction", encodeTx(tx)); err != nil {
		return chainhash.Hash{}, err
	}
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return *hash, nil
}

// btcChain reads Bitcoin through Bitcoin Core's JSON-RPC, using a
// watch-only descriptor wallet (ensureWallet) that holds the addresses the
// bridge watches. The node needs -txindex.
type btcChain struct {
	rpc     *rpcClient // the wallet's endpoint, .../wallet/NAME
	indexed atomic.Bool
}

// walletName is the wallet part of an RPC URL ending in /wallet/NAME.
func walletName(url string) (base, name string) {
	if i := strings.LastIndex(url, "/wallet/"); i >= 0 {
		return url[:i], url[i+len("/wallet/"):]
	}
	return url, ""
}

// errWalletNotFound is Bitcoin Core's code for a wallet that is not loaded.
const errWalletNotFound = -18

// ensureWallet loads the bridge's wallet, creating it the first time as a
// blank, watch-only descriptor wallet: it holds addresses, never keys. The
// bridge and web server start together, so another process may be loading
// or creating it at the same moment: then it waits for that to finish.
func (c *btcChain) ensureWallet() error {
	base, name := walletName(c.rpc.url)
	if name == "" {
		return nil // the node's default wallet
	}
	node := newRPCClient(base, c.rpc.user, c.rpc.pass)
	deadline := time.Now().Add(time.Minute)
	for {
		err := c.rpc.call(nil, "getwalletinfo")
		if !isRPCCode(err, errWalletNotFound) {
			return err // loaded, or a real error
		}
		err = node.call(nil, "loadwallet", name)
		if isRPCCode(err, errWalletNotFound) {
			err = node.callNamed(nil, "createwallet", map[string]any{
				"wallet_name": name, "disable_private_keys": true, "blank": true, "descriptors": true,
			})
		}
		switch {
		case err == nil, isRPCCode(err, errWalletAlreadyLoaded):
			return nil
		case isRPCCode(err, errWalletBusy) && time.Now().Before(deadline):
			time.Sleep(500 * time.Millisecond) // being loaded or created by another process
		default:
			return err
		}
	}
}

// Bitcoin Core's codes for a wallet another request has just loaded, or is
// loading or creating now.
const (
	errWalletAlreadyLoaded = -35
	errWalletBusy          = -4
)

// importTx adds one transaction, already in a block, to the wallet without a
// rescan: this node proves it is in the chain (gettxoutproof) and the wallet
// takes it (importprunedfunds). It lets a signer that started watching an
// address late see a payment made to it before then.
func (c *btcChain) importTx(txid chainhash.Hash) error {
	var raw string
	if err := c.rpc.call(&raw, "getrawtransaction", txid.String(), 0); err != nil {
		return err
	}
	var proof string
	if err := c.rpc.call(&proof, "gettxoutproof", []string{txid.String()}); err != nil {
		return err
	}
	return c.rpc.call(nil, "importprunedfunds", raw, proof)
}

// watch adds address to the wallet as a watch-only descriptor. With rescan
// the wallet looks for past payments to it from the genesis block, which
// takes hours on mainnet; without, from now on.
func (c *btcChain) watch(address btcutil.Address, rescan bool) error {
	var info struct {
		Descriptor string `json:"descriptor"`
	}
	if err := c.rpc.call(&info, "getdescriptorinfo", "addr("+address.EncodeAddress()+")"); err != nil {
		return err
	}
	var timestamp any = "now"
	if rescan {
		timestamp = 0
	}
	var results []struct {
		Success bool      `json:"success"`
		Error   *rpcError `json:"error"`
	}
	req := []map[string]any{{"desc": info.Descriptor, "timestamp": timestamp, "label": "btcvm"}}
	if err := c.rpc.call(&results, "importdescriptors", req); err != nil {
		return err
	}
	if len(results) != 1 || !results[0].Success {
		if len(results) == 1 && results[0].Error != nil {
			return fmt.Errorf("importdescriptors %s: %w", address.EncodeAddress(), results[0].Error)
		}
		return fmt.Errorf("importdescriptors %s failed", address.EncodeAddress())
	}
	return nil
}

// estimateFeeRate is Bitcoin Core's estimate, in sat/vB rounded up, for a
// transaction to confirm within two blocks.
func (c *btcChain) estimateFeeRate() (int64, error) {
	var est struct {
		FeeRate float64  `json:"feerate"` // BTC/kvB
		Errors  []string `json:"errors"`
	}
	if err := c.rpc.call(&est, "estimatesmartfee", 2); err != nil {
		return 0, err
	}
	if est.FeeRate <= 0 {
		return 0, fmt.Errorf("no fee estimate yet: %s", strings.Join(est.Errors, "; "))
	}
	return int64(math.Ceil(est.FeeRate * satPerBTC / 1000)), nil
}

// prevOut returns the output op names, and whether it is in a block and
// unspent there (it may be spent by a transaction in the mempool).
func (c *btcChain) prevOut(op wire.OutPoint) (*wire.TxOut, bool, error) {
	tx, err := c.rawTx(op.Hash)
	if err != nil {
		return nil, false, err
	}
	if int(op.Index) >= len(tx.TxOut) {
		return nil, false, fmt.Errorf("%v has no output %d", op.Hash, op.Index)
	}
	var out *struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := c.rpc.call(&out, "gettxout", op.Hash.String(), op.Index, false); err != nil {
		return nil, false, err
	}
	return tx.TxOut[op.Index], out != nil && out.Confirmations > 0, nil
}

func (c *btcChain) txsFor(addresses []btcutil.Address) ([]chainTx, error) {
	if err := c.requireTxIndex(); err != nil {
		return nil, err
	}
	scripts := scriptSet(addresses)
	var entries []struct {
		TxID    string `json:"txid"`
		Address string `json:"address"`
	}
	if err := c.rpc.callNamed(&entries, "listtransactions", map[string]any{"count": 1_000_000}); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var txs []chainTx
	for _, e := range entries {
		if seen[e.TxID] {
			continue
		}
		seen[e.TxID] = true

		var t struct {
			Hex              string   `json:"hex"`
			Confirmations    int64    `json:"confirmations"`
			Time             int64    `json:"time"`
			MempoolConflicts []string `json:"mempoolconflicts"`
		}
		if err := c.rpc.callNamed(&t, "gettransaction", map[string]any{"txid": e.TxID}); err != nil {
			return nil, err
		}
		// A transaction that conflicts with one in a block (negative
		// confirmations), or that a transaction in the mempool replaced,
		// will not confirm. Only the signers can spend peg outputs, so the
		// replacement is theirs, and it is listed in its place.
		if t.Confirmations < 0 || (t.Confirmations == 0 && len(t.MempoolConflicts) > 0) {
			continue
		}
		tx, err := decodeTx(t.Hex)
		if err != nil {
			return nil, err
		}
		ok, err := touches(tx, scripts, c)
		if err != nil {
			return nil, err
		}
		if ok {
			txs = append(txs, chainTx{tx: tx, confirmations: t.Confirmations, time: t.Time})
		}
	}
	return txs, nil
}

// touches reports whether tx pays to one of scripts or spends an output
// paying to one. The wallet lists only transactions that do one or the
// other, so a transaction whose inputs can't be read fails rather than
// being dropped: dropping a peg payout would hide that it was paid.
func touches(tx *wire.MsgTx, scripts map[string]bool, c *btcChain) (bool, error) {
	for _, out := range tx.TxOut {
		if scripts[string(out.PkScript)] {
			return true, nil
		}
	}
	for _, in := range tx.TxIn {
		if in.PreviousOutPoint.Hash == (chainhash.Hash{}) {
			continue // a coinbase
		}
		// Needs Bitcoin Core's -txindex for outputs the wallet did not
		// create; requireTxIndex checks it is on.
		var prevHex string
		if err := c.rpc.call(&prevHex, "getrawtransaction", in.PreviousOutPoint.Hash.String(), 0); err != nil {
			return false, fmt.Errorf("reading %v, which wallet transaction %v spends: %w", in.PreviousOutPoint.Hash, tx.TxHash(), err)
		}
		prevTx, err := decodeTx(prevHex)
		if err != nil {
			return false, err
		}
		if int(in.PreviousOutPoint.Index) < len(prevTx.TxOut) &&
			scripts[string(prevTx.TxOut[in.PreviousOutPoint.Index].PkScript)] {
			return true, nil
		}
	}
	return false, nil
}

// requireTxIndex fails unless Bitcoin Core runs with a synced -txindex,
// which reading the peg's spends depends on. Once it has passed it is not
// asked again.
func (c *btcChain) requireTxIndex() error {
	if c.indexed.Load() {
		return nil
	}
	var info map[string]struct {
		Synced bool `json:"synced"`
	}
	if err := c.rpc.call(&info, "getindexinfo"); err != nil {
		return err
	}
	idx, ok := info["txindex"]
	switch {
	case !ok:
		return errors.New("Bitcoin Core must run with -txindex")
	case !idx.Synced:
		return errors.New("Bitcoin Core is still building its transaction index (-txindex)")
	}
	c.indexed.Store(true)
	return nil
}

func (c *btcChain) unspent(addresses []btcutil.Address, minConf int64) ([]utxo, error) {
	encoded := make([]string, len(addresses))
	for i, a := range addresses {
		encoded[i] = a.EncodeAddress()
	}
	var entries []struct {
		TxID          string  `json:"txid"`
		Vout          uint32  `json:"vout"`
		Amount        float64 `json:"amount"`
		ScriptPubKey  string  `json:"scriptPubKey"`
		Confirmations int64   `json:"confirmations"`
	}
	err := c.rpc.call(&entries, "listunspent", minConf, 9_999_999, encoded)
	if err != nil {
		return nil, err
	}
	var utxos []utxo
	for _, e := range entries {
		hash, err := chainhash.NewHashFromStr(e.TxID)
		if err != nil {
			return nil, err
		}
		script, err := hex.DecodeString(e.ScriptPubKey)
		if err != nil {
			return nil, err
		}
		utxos = append(utxos, utxo{
			outPoint: wire.OutPoint{Hash: *hash, Index: e.Vout},
			// Bitcoin Core reports amounts as JSON numbers in BTC.
			value:         int64(math.Round(e.Amount * satPerBTC)),
			pkScript:      script,
			confirmations: e.Confirmations,
		})
	}
	return utxos, nil
}

// sender returns the destination that funded the first input of txid, which
// is where a refund of it should normally go.
func (c *btcChain) sender(txid chainhash.Hash) (destination, error) {
	var txHex string
	if err := c.rpc.call(&txHex, "getrawtransaction", txid.String(), 0); err != nil {
		return destination{}, err
	}
	tx, err := decodeTx(txHex)
	if err != nil {
		return destination{}, err
	}
	prev := tx.TxIn[0].PreviousOutPoint
	var prevHex string
	if err := c.rpc.call(&prevHex, "getrawtransaction", prev.Hash.String(), 0); err != nil {
		return destination{}, err
	}
	prevTx, err := decodeTx(prevHex)
	if err != nil {
		return destination{}, err
	}
	if int(prev.Index) >= len(prevTx.TxOut) {
		return destination{}, errors.New("malformed input")
	}
	return destinationOfScript(prevTx.TxOut[prev.Index].PkScript)
}

func (c *btcChain) send(tx *wire.MsgTx) (chainhash.Hash, error) {
	return sendRaw(c.rpc, tx)
}

// scriptSet returns the output scripts paying to addresses, as map keys.
func scriptSet(addresses []btcutil.Address) map[string]bool {
	scripts := make(map[string]bool, len(addresses))
	for _, a := range addresses {
		scripts[string(destinationScript(a))] = true
	}
	return scripts
}

// destinationScript returns the output script paying to address.
func destinationScript(address btcutil.Address) []byte {
	d, err := destinationOf(address)
	if err != nil {
		panic(fmt.Sprintf("destinationScript: %v", err))
	}
	return d.pkScript()
}
