package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
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

// confirmer is a chain that can say whether a transaction is in a block.
type confirmer interface {
	confirmed(txid chainhash.Hash) (bool, error)
}

// confirmedOn returns a check of whether a transaction is in a block on c,
// or nil if c can't tell. An error counts as not known to be confirmed.
func confirmedOn(c chain) func(string) bool {
	cc, ok := c.(confirmer)
	if !ok {
		return nil
	}
	return func(txid string) bool {
		h, err := chainhash.NewHashFromStr(txid)
		if err != nil {
			return false
		}
		yes, err := cc.confirmed(*h)
		return err == nil && yes
	}
}

// confirmed reports whether the wallet has txid in a block.
func (c *btcChain) confirmed(txid chainhash.Hash) (bool, error) {
	var t struct {
		Confirmations int64 `json:"confirmations"`
	}
	err := c.rpc.callNamed(&t, "gettransaction", map[string]any{"txid": txid.String()})
	if isRPCCode(err, errNotWalletTx) {
		return false, nil
	}
	return t.Confirmations > 0, err
}

// confirmed reports whether BTCVM has txid in a block (its node keeps a
// transaction index).
func (c *vmChain) confirmed(txid chainhash.Hash) (bool, error) {
	var t struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := c.rpc.call(&t, "getrawtransaction", txid.String(), 1); err != nil {
		return false, err
	}
	return t.Confirmations > 0, nil
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
// bridge watches. The node can be pruned and needs no -txindex.
type btcChain struct {
	rpc *rpcClient // the wallet's endpoint, .../wallet/NAME

	mu      sync.Mutex
	known   map[string]*knownTx // wallet transactions read so far, by txid
	watched map[string]bool     // addresses this process has had the wallet watch
}

// watchOnce has the wallet watch address from now on, unless this process
// already has: calling it on every registration, not only new ones,
// retries an import that failed.
func (c *btcChain) watchOnce(address btcutil.Address) error {
	c.mu.Lock()
	done := c.watched[address.EncodeAddress()]
	c.mu.Unlock()
	if done {
		return nil
	}
	if err := c.watch(address, false); err != nil {
		return err
	}
	c.mu.Lock()
	if c.watched == nil {
		c.watched = map[string]bool{}
	}
	c.watched[address.EncodeAddress()] = true
	c.mu.Unlock()
	return nil
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
// address late see a payment made to it before then. A pruned node without
// -txindex finds the transaction only in the block named by blockHash, a
// hint from the coordinator that this node checks; it must still hold that
// block.
func (c *btcChain) importTx(txid chainhash.Hash, blockHash string) error {
	args := []any{txid.String(), 0}
	proofArgs := []any{[]string{txid.String()}}
	if blockHash != "" {
		args = append(args, blockHash)
		proofArgs = append(proofArgs, blockHash)
	}
	var raw string
	if err := c.rpc.call(&raw, "getrawtransaction", args...); err != nil {
		return err
	}
	var proof string
	if err := c.rpc.call(&proof, "gettxoutproof", proofArgs...); err != nil {
		return err
	}
	if err := c.rpc.call(nil, "importprunedfunds", raw, proof); err != nil {
		return err
	}
	return nil
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
		Success  bool      `json:"success"`
		Error    *rpcError `json:"error"`
		Warnings []string  `json:"warnings"`
	}
	req := []map[string]any{{"desc": info.Descriptor, "timestamp": timestamp, "label": "btcvm"}}
	// The bridge, web server and monitor share the wallet, and start
	// together: while one's import rescans, the wallet turns the others
	// away. Wait for it.
	deadline := time.Now().Add(5 * time.Minute)
	for {
		err := c.rpc.call(&results, "importdescriptors", req)
		if err == nil {
			break
		}
		if !isRPCCode(err, errWalletBusy) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(2 * time.Second)
	}
	// A pruned node rescans only the blocks it still has, and says so in a
	// warning: payments older than that are not found.
	for _, r := range results {
		for _, w := range r.Warnings {
			log.Printf("watching %s: %s", address.EncodeAddress(), w)
		}
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

// prevOut returns the output op names if it is in a block and unspent
// there (it may be spent by a transaction in the mempool), from the UTXO
// set, which needs no -txindex. Otherwise it returns nil and false.
func (c *btcChain) prevOut(op wire.OutPoint) (*wire.TxOut, bool, error) {
	var out *struct {
		Confirmations int64   `json:"confirmations"`
		Value         float64 `json:"value"`
		ScriptPubKey  struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	}
	if err := c.rpc.call(&out, "gettxout", op.Hash.String(), op.Index, false); err != nil {
		return nil, false, err
	}
	if out == nil || out.Confirmations < 1 {
		return nil, false, nil
	}
	script, err := hex.DecodeString(out.ScriptPubKey.Hex)
	if err != nil {
		return nil, false, err
	}
	return wire.NewTxOut(int64(math.Round(out.Value*satPerBTC)), script), true, nil
}

func (c *btcChain) txsFor(addresses []btcutil.Address) ([]chainTx, error) {
	scripts := scriptSet(addresses)
	var entries []struct {
		TxID             string   `json:"txid"`
		Confirmations    int64    `json:"confirmations"`
		Time             int64    `json:"time"`
		MempoolConflicts []string `json:"mempoolconflicts"`
	}
	const maxEntries = 1_000_000
	if err := c.rpc.callNamed(&entries, "listtransactions", map[string]any{"count": maxEntries}); err != nil {
		return nil, err
	}
	// The listing is newest first: at the cap, older entries (a payout, say)
	// may be missing, and reading them as absent could pay twice.
	if len(entries) >= maxEntries {
		return nil, fmt.Errorf("the wallet lists %d or more transactions; its history can't be read in full", maxEntries)
	}

	seen := map[string]bool{}
	var txs []chainTx
	for _, e := range entries {
		if seen[e.TxID] {
			continue
		}
		seen[e.TxID] = true
		// A transaction that conflicts with one in a block (negative
		// confirmations), or that a transaction in the mempool replaced,
		// will not confirm. Only the signers can spend peg outputs, so the
		// replacement is theirs, and it is listed in its place.
		if e.Confirmations < 0 || (e.Confirmations == 0 && len(e.MempoolConflicts) > 0) {
			continue
		}
		known, err := c.walletTx(e.TxID)
		if err != nil {
			return nil, err
		}
		if known.touches(scripts) {
			txs = append(txs, chainTx{tx: known.tx, confirmations: e.Confirmations, time: e.Time})
		}
	}
	return txs, nil
}

// errNotWalletTx is Bitcoin Core's code for a transaction the wallet
// doesn't have.
const errNotWalletTx = -5

// walletRaw reads a transaction from the wallet; ours is false if the wallet
// doesn't have it, which needs no -txindex to know.
func (c *btcChain) walletRaw(txid string) (tx *wire.MsgTx, ours bool, err error) {
	var t struct {
		Hex string `json:"hex"`
	}
	err = c.rpc.callNamed(&t, "gettransaction", map[string]any{"txid": txid})
	if isRPCCode(err, errNotWalletTx) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	tx, err = decodeTx(t.Hex)
	return tx, err == nil, err
}

// blockOf is the hash of the block holding wallet transaction txid, or ""
// if it is unconfirmed.
func (c *btcChain) blockOf(txid string) (string, error) {
	var t struct {
		BlockHash string `json:"blockhash"`
	}
	err := c.rpc.callNamed(&t, "gettransaction", map[string]any{"txid": txid})
	return t.BlockHash, err
}

// knownTx is a wallet transaction, read from the node once: its bytes never
// change. Its confirmations and conflicts come fresh from each listing.
type knownTx struct {
	tx *wire.MsgTx
}

// touches reports whether the transaction pays to one of scripts or spends
// an output paying to one. A spend is known by its witness script, whose
// P2WSH hash consensus has checked against the output spent, so it needs no
// lookup of the transaction that made the output. (Every signer transaction
// also pays the peg address, so it is found by its outputs too.)
func (k *knownTx) touches(scripts map[string]bool) bool {
	for _, out := range k.tx.TxOut {
		if scripts[string(out.PkScript)] {
			return true
		}
	}
	for _, in := range k.tx.TxIn {
		if len(in.Witness) > 0 && scripts[string(p2wshScript(in.Witness[len(in.Witness)-1]))] {
			return true
		}
	}
	return false
}

// walletTx reads a wallet transaction, from the cache or the node.
func (c *btcChain) walletTx(txid string) (*knownTx, error) {
	c.mu.Lock()
	known, ok := c.known[txid]
	c.mu.Unlock()
	if ok {
		return known, nil
	}
	tx, ours, err := c.walletRaw(txid)
	if err != nil {
		return nil, err
	}
	if !ours {
		return nil, fmt.Errorf("%s is not a wallet transaction", txid)
	}
	known = &knownTx{tx: tx}
	c.mu.Lock()
	if c.known == nil {
		c.known = map[string]*knownTx{}
	}
	c.known[txid] = known
	c.mu.Unlock()
	return known, nil
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
// is where a refund of it should normally go. It needs the funding
// transaction, which a pruned node without -txindex usually no longer has:
// then the refund needs an address given explicitly.
func (c *btcChain) sender(txid chainhash.Hash) (destination, error) {
	deposit, err := c.walletTx(txid.String())
	if err != nil {
		return destination{}, err
	}
	prev := deposit.tx.TxIn[0].PreviousOutPoint
	prevTx, err := c.rawTx(prev.Hash)
	if err != nil {
		return destination{}, fmt.Errorf("this node can't find the deposit's sender (%v); give the refund address with -to", err)
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
