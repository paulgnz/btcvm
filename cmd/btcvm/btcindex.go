package main

// btcIndex gives the web wallet Bitcoin balances: the unspent outputs and
// history of the Bitcoin addresses users register, found by following each
// new block. Bitcoin Core has no address index, and adding thousands of
// user addresses to the wallet the bridge reads would slow the bridge, so the
// wallet has this index of its own.
//
// It indexes from the block an address was registered at. Payments made
// before then are not found by following blocks; a user can add one by
// transaction ID (importTx), which this node checks against its UTXO set.
// It keeps the raw bytes of each transaction paying a watched address, so a
// wallet can check its coins even after a pruned node has dropped their
// blocks.

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"

	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// btcSource is what the index needs from Bitcoin Core.
type btcSource interface {
	synced() (bool, error)
	blockCount() (int64, error)
	blockHash(height int64) (chainhash.Hash, error)
	// blockInfo returns a block's parent, time and transactions, in one call:
	// a Bitcoin block holds thousands.
	blockInfo(hash chainhash.Hash) (prev chainhash.Hash, unix int64, txs []*wire.MsgTx, err error)
	rawTx(txid chainhash.Hash) (*wire.MsgTx, error) // mempool transactions, at least
	// txOut is an output unspent in a block: its value, script and
	// confirmations, or found false.
	txOut(op wire.OutPoint) (value int64, script []byte, confirmations int64, found bool, err error)
	mempool() ([]chainhash.Hash, error)
}

// Keys in the index database. Heights are big-endian so keys sort by height.
const (
	keyWatched = 'w' // w script -> registered height
	keyUTXO    = 'u' // u script outpoint -> value, height
	keyOwner   = 'o' // o outpoint -> script
	keyHistory = 'h' // h script height txid -> net, unix
	keyUndo    = 'b' // b height -> undo record
	keyTip     = 't' // t -> height, hash
	keyRaw     = 'r' // r txid -> raw transaction paying a watched address
)

const undoDepth = 288 // blocks of undo kept for reorganisations

func outPointBytes(op wire.OutPoint) []byte {
	b := make([]byte, 36)
	copy(b, op.Hash[:])
	binary.LittleEndian.PutUint32(b[32:], op.Index)
	return b
}

func parseOutPointBytes(b []byte) wire.OutPoint {
	var op wire.OutPoint
	copy(op.Hash[:], b[:32])
	op.Index = binary.LittleEndian.Uint32(b[32:36])
	return op
}

func u64(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

func i64(b []byte) int64 { return int64(binary.BigEndian.Uint64(b)) }

func key(prefix byte, parts ...[]byte) []byte {
	k := []byte{prefix}
	for _, p := range parts {
		k = append(k, p...)
	}
	return k
}

// scriptKey prefixes a script with its length, so one script's keys never
// share a prefix with a longer script's.
func scriptKey(script []byte) []byte { return append([]byte{byte(len(script))}, script...) }

type undoRecord struct {
	Hash    chainhash.Hash `json:"hash"`
	Put     [][]byte       `json:"put"`     // keys this block added
	Deleted [][2][]byte    `json:"deleted"` // keys and values this block removed
}

type btcIndex struct {
	db  *leveldb.DB
	src btcSource

	mu      sync.RWMutex
	watched map[string]int64 // script -> registered height

	// Unconfirmed transactions: those touching watched addresses, and nil
	// for the rest, so each is fetched once. Bitcoin's mempool holds
	// hundreds of thousands.
	mempoolMu  sync.RWMutex
	mempoolTxs map[chainhash.Hash]*wire.MsgTx
	pending    map[string][]pendingEntry
	pendingIn  map[wire.OutPoint]bool
}

type pendingEntry struct {
	txid chainhash.Hash
	net  int64
}

func openBTCIndex(path string, src btcSource) (*btcIndex, error) {
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		return nil, err
	}
	x := &btcIndex{db: db, src: src, watched: map[string]int64{},
		mempoolTxs: map[chainhash.Hash]*wire.MsgTx{}, pending: map[string][]pendingEntry{}, pendingIn: map[wire.OutPoint]bool{}}
	it := db.NewIterator(util.BytesPrefix([]byte{keyWatched}), nil)
	for it.Next() {
		k := it.Key()
		x.watched[string(k[2:])] = i64(it.Value())
	}
	it.Release()
	return x, it.Error()
}

func (x *btcIndex) close() error { return x.db.Close() }

// tip returns the last indexed block, or -1 before the first.
func (x *btcIndex) tip() (int64, chainhash.Hash) {
	v, err := x.db.Get([]byte{keyTip}, nil)
	if err != nil {
		return -1, chainhash.Hash{}
	}
	var h chainhash.Hash
	copy(h[:], v[8:])
	return i64(v[:8]), h
}

func (x *btcIndex) isWatched(script []byte) (int64, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	h, ok := x.watched[string(script)]
	return h, ok
}

// watch starts indexing script from the next block.
func (x *btcIndex) watch(script []byte) (int64, error) {
	if h, ok := x.isWatched(script); ok {
		return h, nil
	}
	height, _ := x.tip()
	if height < 0 {
		return 0, errors.New("the Bitcoin index has not started; the node may still be syncing")
	}
	if err := x.db.Put(key(keyWatched, scriptKey(script)), u64(height+1), nil); err != nil {
		return 0, err
	}
	x.mu.Lock()
	x.watched[string(script)] = height + 1
	x.mu.Unlock()
	return height + 1, nil
}

// run follows the chain until stop is closed.
func (x *btcIndex) run(stop <-chan struct{}) {
	for n := 0; ; n++ {
		if err := x.sync(); err != nil {
			log.Printf("bitcoin index: %v", err)
		}
		// The mempool is large; a full pass every ten seconds is enough, as
		// the wallet's own payments are added the moment they are sent.
		if n%5 == 0 {
			if err := x.refreshMempool(); err != nil {
				log.Printf("bitcoin index mempool: %v", err)
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(2 * time.Second): // a new block reaches wallets within seconds
		}
	}
}

// sync indexes every block up to the node's tip, undoing blocks a
// reorganisation replaced. It waits for the node to finish syncing, and then
// starts at its tip: addresses are only registered from then on.
func (x *btcIndex) sync() error {
	ok, err := x.src.synced()
	if err != nil || !ok {
		return err
	}
	for {
		height, hash := x.tip()
		best, err := x.src.blockCount()
		if err != nil {
			return err
		}
		if height < 0 {
			h, err := x.src.blockHash(best)
			if err != nil {
				return err
			}
			return x.db.Put([]byte{keyTip}, append(u64(best), h[:]...), nil)
		}
		if height >= best {
			// Still on the best chain?
			h, err := x.src.blockHash(height)
			if err != nil {
				return err
			}
			if h != hash {
				if err := x.undo(height); err != nil {
					return err
				}
				continue
			}
			return nil
		}
		next, err := x.src.blockHash(height + 1)
		if err != nil {
			return err
		}
		prev, unix, txs, err := x.src.blockInfo(next)
		if err != nil {
			return err
		}
		if prev != hash {
			if err := x.undo(height); err != nil {
				return err
			}
			continue
		}
		if err := x.apply(height+1, next, unix, txs); err != nil {
			return err
		}
	}
}

// apply indexes one block.
func (x *btcIndex) apply(height int64, hash chainhash.Hash, unix int64, txs []*wire.MsgTx) error {
	batch := new(leveldb.Batch)
	undo := undoRecord{Hash: hash}
	put := func(k, v []byte) {
		batch.Put(k, v)
		undo.Put = append(undo.Put, k)
	}
	del := func(k []byte) error {
		v, err := x.db.Get(k, nil)
		if err != nil {
			return err
		}
		batch.Delete(k)
		undo.Deleted = append(undo.Deleted, [2][]byte{k, v})
		return nil
	}
	// Outputs to watched addresses created earlier in this block, which
	// later transactions in it may spend.
	type newOutput struct {
		script []byte
		value  int64
	}
	created := map[wire.OutPoint]newOutput{}

	for i, tx := range txs {
		txid := tx.TxHash()
		net := map[string]int64{}
		if i > 0 { // the coinbase spends nothing
			for _, in := range tx.TxIn {
				op := in.PreviousOutPoint
				if c, ok := created[op]; ok {
					// Created and spent within this block: it never exists.
					delete(created, op)
					batch.Delete(key(keyUTXO, scriptKey(c.script), outPointBytes(op)))
					batch.Delete(key(keyOwner, outPointBytes(op)))
					net[string(c.script)] -= c.value
					continue
				}
				script, err := x.db.Get(key(keyOwner, outPointBytes(op)), nil)
				if err != nil {
					continue // not a watched output
				}
				uk := key(keyUTXO, scriptKey(script), outPointBytes(op))
				v, err := x.db.Get(uk, nil)
				if err != nil {
					continue
				}
				if err := del(uk); err != nil {
					return err
				}
				if err := del(key(keyOwner, outPointBytes(op))); err != nil {
					return err
				}
				net[string(script)] -= i64(v[:8])
			}
		}
		stored := false
		for vout, out := range tx.TxOut {
			if _, ok := x.isWatched(out.PkScript); !ok {
				continue
			}
			if !stored {
				var buf bytes.Buffer
				if err := tx.Serialize(&buf); err != nil {
					return err
				}
				put(key(keyRaw, txid[:]), buf.Bytes())
				stored = true
			}
			op := wire.OutPoint{Hash: txid, Index: uint32(vout)}
			put(key(keyUTXO, scriptKey(out.PkScript), outPointBytes(op)), append(u64(out.Value), u64(height)...))
			put(key(keyOwner, outPointBytes(op)), out.PkScript)
			created[op] = newOutput{script: out.PkScript, value: out.Value}
			net[string(out.PkScript)] += out.Value
		}
		for script, n := range net {
			put(key(keyHistory, scriptKey([]byte(script)), u64(height), txid[:]), append(u64(n), u64(unix)...))
		}
	}

	raw, _ := json.Marshal(undo)
	batch.Put(key(keyUndo, u64(height)), raw)
	batch.Put([]byte{keyTip}, append(u64(height), hash[:]...))
	if height > undoDepth {
		batch.Delete(key(keyUndo, u64(height-undoDepth)))
	}
	return x.db.Write(batch, nil)
}

// undo removes the block at height, restoring what it spent.
func (x *btcIndex) undo(height int64) error {
	raw, err := x.db.Get(key(keyUndo, u64(height)), nil)
	if err != nil {
		return fmt.Errorf("reorganisation deeper than %d blocks: rebuild the Bitcoin index", undoDepth)
	}
	var u undoRecord
	if err := json.Unmarshal(raw, &u); err != nil {
		return err
	}
	batch := new(leveldb.Batch)
	for _, k := range u.Put {
		batch.Delete(k)
	}
	for _, kv := range u.Deleted {
		batch.Put(kv[0], kv[1])
	}
	batch.Delete(key(keyUndo, u64(height)))
	prev, err := x.src.blockHash(height - 1)
	if err != nil {
		return err
	}
	batch.Put([]byte{keyTip}, append(u64(height-1), prev[:]...))
	log.Printf("bitcoin index: undid block %d (%v) after a reorganisation", height, u.Hash)
	return x.db.Write(batch, nil)
}

// refreshMempool notes unconfirmed transactions touching watched addresses.
func (x *btcIndex) refreshMempool() error {
	if tip, _ := x.tip(); tip < 0 {
		return nil
	}
	txids, err := x.src.mempool()
	if err != nil {
		return err
	}
	x.mempoolMu.RLock()
	known := x.mempoolTxs
	x.mempoolMu.RUnlock()
	txs := make(map[chainhash.Hash]*wire.MsgTx, len(txids))
	for _, id := range txids {
		if tx, ok := known[id]; ok {
			txs[id] = tx
			continue
		}
		tx, err := x.src.rawTx(id)
		if err != nil {
			continue // mined or dropped since
		}
		if x.touchesWatched(tx) {
			txs[id] = tx
		} else {
			txs[id] = nil
		}
	}
	pending := map[string][]pendingEntry{}
	spent := map[wire.OutPoint]bool{}
	for id, tx := range txs {
		if tx == nil {
			continue
		}
		net := map[string]int64{}
		for _, in := range tx.TxIn {
			script, err := x.db.Get(key(keyOwner, outPointBytes(in.PreviousOutPoint)), nil)
			if err != nil {
				continue
			}
			v, err := x.db.Get(key(keyUTXO, scriptKey(script), outPointBytes(in.PreviousOutPoint)), nil)
			if err != nil {
				continue
			}
			net[string(script)] -= i64(v[:8])
			spent[in.PreviousOutPoint] = true
		}
		for _, out := range tx.TxOut {
			if _, ok := x.isWatched(out.PkScript); ok {
				net[string(out.PkScript)] += out.Value
			}
		}
		for script, n := range net {
			pending[script] = append(pending[script], pendingEntry{txid: id, net: n})
		}
	}
	x.mempoolMu.Lock()
	x.mempoolTxs, x.pending, x.pendingIn = txs, pending, spent
	x.mempoolMu.Unlock()
	return nil
}

// touchesWatched reports whether tx pays a watched address or spends an
// indexed output.
func (x *btcIndex) touchesWatched(tx *wire.MsgTx) bool {
	for _, out := range tx.TxOut {
		if _, ok := x.isWatched(out.PkScript); ok {
			return true
		}
	}
	for _, in := range tx.TxIn {
		if ok, _ := x.db.Has(key(keyOwner, outPointBytes(in.PreviousOutPoint)), nil); ok {
			return true
		}
	}
	return false
}

// importTx adds a payment to a watched address made before it was
// registered: each output of txid that pays the address and is unspent in a
// block, as this node's UTXO set has it (it needs no -txindex). Outputs are
// looked for among the first maxImportOutputs.
func (x *btcIndex) importTx(script []byte, txid chainhash.Hash) (int, error) {
	if _, ok := x.isWatched(script); !ok {
		return 0, errors.New("address is not registered")
	}
	tip, _ := x.tip()
	batch := new(leveldb.Batch)
	var added int
	var net, height int64
	for vout := uint32(0); vout < maxImportOutputs; vout++ {
		op := wire.OutPoint{Hash: txid, Index: vout}
		value, outScript, confs, found, err := x.src.txOut(op)
		if err != nil {
			return 0, err
		}
		if !found || !bytes.Equal(outScript, script) {
			continue
		}
		if confs < 1 {
			return 0, errors.New("the transaction is not in a block yet; it will appear on its own once it is")
		}
		height = tip - confs + 1
		batch.Put(key(keyUTXO, scriptKey(script), outPointBytes(op)), append(u64(value), u64(height)...))
		batch.Put(key(keyOwner, outPointBytes(op)), script)
		net += value
		added++
	}
	if added == 0 {
		return 0, errors.New("no unspent output of that transaction pays this address")
	}
	batch.Put(key(keyHistory, scriptKey(script), u64(height), txid[:]), append(u64(net), u64(0)...))
	return added, x.db.Write(batch, nil)
}

// maxImportOutputs bounds the outputs importTx looks at.
const maxImportOutputs = 64

// storedTx returns the raw transaction txid if the index kept it.
func (x *btcIndex) storedTx(txid chainhash.Hash) ([]byte, bool) {
	raw, err := x.db.Get(key(keyRaw, txid[:]), nil)
	return raw, err == nil
}

type indexedUTXO struct {
	op     wire.OutPoint
	value  int64
	height int64
}

type indexedTx struct {
	txid   chainhash.Hash
	net    int64
	height int64 // 0: unconfirmed
	unix   int64
}

// view returns script's unspent outputs (less those spent by unconfirmed
// transactions) and history, newest first.
func (x *btcIndex) view(script []byte) (utxos []indexedUTXO, history []indexedTx, pending int64) {
	sk := scriptKey(script)
	x.mempoolMu.RLock()
	spent := x.pendingIn
	for _, p := range x.pending[string(script)] {
		pending += p.net
		history = append(history, indexedTx{txid: p.txid, net: p.net})
	}
	x.mempoolMu.RUnlock()

	it := x.db.NewIterator(util.BytesPrefix(key(keyUTXO, sk)), nil)
	for it.Next() {
		op := parseOutPointBytes(it.Key()[1+len(sk):])
		if spent[op] {
			continue
		}
		v := it.Value()
		utxos = append(utxos, indexedUTXO{op: op, value: i64(v[:8]), height: i64(v[8:16])})
	}
	it.Release()

	it = x.db.NewIterator(util.BytesPrefix(key(keyHistory, sk)), nil)
	var confirmed []indexedTx
	for it.Next() {
		k := it.Key()[1+len(sk):]
		var txid chainhash.Hash
		copy(txid[:], k[8:40])
		v := it.Value()
		confirmed = append(confirmed, indexedTx{txid: txid, height: i64(k[:8]), net: i64(v[:8]), unix: i64(v[8:16])})
	}
	it.Release()
	sort.Slice(confirmed, func(i, j int) bool { return confirmed[i].height > confirmed[j].height })
	history = append(history, confirmed...)
	if len(history) > 50 {
		history = history[:50]
	}
	return utxos, history, pending
}

// --- Bitcoin Core as the index's source --------------------------------------

func (c *btcChain) synced() (bool, error) {
	var info struct {
		Blocks  int64 `json:"blocks"`
		Headers int64 `json:"headers"`
	}
	if err := c.rpc.call(&info, "getblockchaininfo"); err != nil {
		return false, err
	}
	return info.Headers > 0 && info.Blocks >= info.Headers-1, nil
}

func (c *btcChain) blockCount() (int64, error) {
	var n int64
	return n, c.rpc.call(&n, "getblockcount")
}

func (c *btcChain) blockHash(height int64) (chainhash.Hash, error) {
	var s string
	if err := c.rpc.call(&s, "getblockhash", height); err != nil {
		return chainhash.Hash{}, err
	}
	h, err := chainhash.NewHashFromStr(s)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return *h, nil
}

func (c *btcChain) blockInfo(hash chainhash.Hash) (chainhash.Hash, int64, []*wire.MsgTx, error) {
	var raw string
	if err := c.rpc.call(&raw, "getblock", hash.String(), 0); err != nil {
		return chainhash.Hash{}, 0, nil, err
	}
	b, err := hex.DecodeString(raw)
	if err != nil {
		return chainhash.Hash{}, 0, nil, err
	}
	var block wire.MsgBlock
	if err := block.Deserialize(bytes.NewReader(b)); err != nil {
		return chainhash.Hash{}, 0, nil, err
	}
	if block.BlockHash() != hash {
		return chainhash.Hash{}, 0, nil, fmt.Errorf("block %v came back as %v", hash, block.BlockHash())
	}
	return block.Header.PrevBlock, block.Header.Timestamp.Unix(), block.Transactions, nil
}

func (c *btcChain) rawTx(txid chainhash.Hash) (*wire.MsgTx, error) {
	var s string
	if err := c.rpc.call(&s, "getrawtransaction", txid.String(), 0); err != nil {
		return nil, err
	}
	return decodeTx(s)
}

func (c *btcChain) mempool() ([]chainhash.Hash, error) {
	var ids []string
	if err := c.rpc.call(&ids, "getrawmempool", false); err != nil {
		return nil, err
	}
	out := make([]chainhash.Hash, 0, len(ids))
	for _, s := range ids {
		h, err := chainhash.NewHashFromStr(s)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, nil
}

func (c *btcChain) txOut(op wire.OutPoint) (int64, []byte, int64, bool, error) {
	var out *struct {
		Confirmations int64   `json:"confirmations"`
		Value         float64 `json:"value"`
		ScriptPubKey  struct {
			Hex string `json:"hex"`
		} `json:"scriptPubKey"`
	}
	if err := c.rpc.call(&out, "gettxout", op.Hash.String(), op.Index, false); err != nil || out == nil {
		return 0, nil, 0, false, err
	}
	script, err := hex.DecodeString(out.ScriptPubKey.Hex)
	if err != nil {
		return 0, nil, 0, false, err
	}
	return int64(math.Round(out.Value * satPerBTC)), script, out.Confirmations, true, nil
}
