package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// fakeBTC is a Bitcoin chain for the index: blocks of transactions, a
// mempool, and nothing else.
type fakeBTC struct {
	blocks [][]*wire.MsgTx // blocks[h] are block h's transactions, coinbase first
	hashes []chainhash.Hash
	pool   []*wire.MsgTx
	fork   byte // changes block hashes, to simulate a reorganisation
	coins  uint32
}

func newFakeBTC() *fakeBTC {
	f := &fakeBTC{}
	f.mine() // genesis
	return f
}

func (f *fakeBTC) coinbase() *wire.MsgTx {
	f.coins++
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{}, wire.MaxPrevOutIndex), []byte{byte(f.coins), byte(f.coins >> 8), f.fork}, nil))
	tx.AddTxOut(wire.NewTxOut(10_000*btc, []byte{0x51}))
	return tx
}

// mine makes a block of the mempool's transactions.
func (f *fakeBTC) mine() {
	txs := append([]*wire.MsgTx{f.coinbase()}, f.pool...)
	f.pool = nil
	f.blocks = append(f.blocks, txs)
	var h chainhash.Hash
	h[0], h[1], h[2] = byte(len(f.blocks)), byte(len(f.blocks)>>8), f.fork
	f.hashes = append(f.hashes, h)
}

// reorg replaces the last n blocks with new ones holding the same
// transactions (their coinbases differ) and, if drop, loses the final
// block's other transactions back to the mempool.
func (f *fakeBTC) reorg(n int, drop bool) {
	f.fork++
	kept := f.blocks[:len(f.blocks)-n]
	old := f.blocks[len(f.blocks)-n:]
	f.blocks, f.hashes = kept, f.hashes[:len(kept)]
	for i, b := range old {
		if drop && i == len(old)-1 {
			// The replacement block leaves them out: back to the mempool.
			f.pool = nil
			f.mine()
			f.pool = append(f.pool, b[1:]...)
			continue
		}
		f.pool = b[1:]
		f.mine()
	}
}

func (f *fakeBTC) pay(from *wire.OutPoint, value int64, script []byte) *wire.MsgTx {
	tx := wire.NewMsgTx(1)
	if from == nil {
		f.coins++
		from = &wire.OutPoint{Hash: chainhash.Hash{0xee, byte(f.coins)}}
	}
	tx.AddTxIn(wire.NewTxIn(from, []byte{1}, nil))
	tx.AddTxOut(wire.NewTxOut(value, script))
	f.pool = append(f.pool, tx)
	return tx
}

func (f *fakeBTC) synced() (bool, error)      { return true, nil }
func (f *fakeBTC) blockCount() (int64, error) { return int64(len(f.blocks) - 1), nil }
func (f *fakeBTC) blockHash(h int64) (chainhash.Hash, error) {
	if h < 0 || int(h) >= len(f.hashes) {
		return chainhash.Hash{}, errors.New("no such block")
	}
	return f.hashes[h], nil
}
func (f *fakeBTC) blockInfo(hash chainhash.Hash) (chainhash.Hash, int64, []*wire.MsgTx, error) {
	for h, x := range f.hashes {
		if x == hash {
			var prev chainhash.Hash
			if h > 0 {
				prev = f.hashes[h-1]
			}
			return prev, int64(1_700_000_000 + h*600), f.blocks[h], nil
		}
	}
	return chainhash.Hash{}, 0, nil, errors.New("no such block")
}
func (f *fakeBTC) rawTx(txid chainhash.Hash) (*wire.MsgTx, error) {
	for _, b := range f.blocks {
		for _, tx := range b {
			if tx.TxHash() == txid {
				return tx, nil
			}
		}
	}
	for _, tx := range f.pool {
		if tx.TxHash() == txid {
			return tx, nil
		}
	}
	return nil, errors.New("no such transaction")
}

// txOut is an output unspent in a block (the fake's UTXO set: mempool
// spends don't count, as gettxout with include_mempool false).
func (f *fakeBTC) txOut(op wire.OutPoint) (int64, []byte, int64, bool, error) {
	for h, b := range f.blocks {
		for _, tx := range b {
			if tx.TxHash() != op.Hash || int(op.Index) >= len(tx.TxOut) {
				continue
			}
			for _, later := range f.blocks[h:] {
				for _, spender := range later {
					for _, in := range spender.TxIn {
						if in.PreviousOutPoint == op {
							return 0, nil, 0, false, nil
						}
					}
				}
			}
			out := tx.TxOut[op.Index]
			return out.Value, out.PkScript, int64(len(f.blocks) - h), true, nil
		}
	}
	return 0, nil, 0, false, nil
}
func (f *fakeBTC) mempool() ([]chainhash.Hash, error) {
	var ids []chainhash.Hash
	for _, tx := range f.pool {
		ids = append(ids, tx.TxHash())
	}
	return ids, nil
}

func balanceOf(x *btcIndex, script []byte) (confirmed, pending int64, history int) {
	utxos, hist, pend := x.view(script)
	for _, u := range utxos {
		confirmed += u.value
	}
	return confirmed, pend, len(hist)
}

func TestBTCIndex(t *testing.T) {
	require := require.New(t)
	chain := newFakeBTC()
	path := filepath.Join(t.TempDir(), "index")
	x, err := openBTCIndex(path, chain)
	require.NoError(err)

	alice := destination{kind: destP2PKH, hash: [32]byte{1}}.pkScript()
	bob := destination{kind: destP2PKH, hash: [32]byte{2}}.pkScript()

	// Paid before the index or the address existed: not seen.
	chain.pay(nil, 5*btc, alice)
	chain.mine()
	require.NoError(x.sync())
	_, err = x.watch(alice)
	require.NoError(err)

	// A payment in the mempool is pending, then confirmed.
	in := chain.pay(nil, 100*btc, alice)
	require.NoError(x.refreshMempool())
	c, p, _ := balanceOf(x, alice)
	require.Equal(int64(0), c)
	require.Equal(int64(100*btc), p)
	chain.mine()
	require.NoError(x.sync())
	require.NoError(x.refreshMempool())
	c, p, n := balanceOf(x, alice)
	require.Equal(int64(100*btc), c)
	require.Zero(p)
	require.Equal(1, n)

	// Alice pays Bob 30 (not watched) with 70 change back to herself.
	spend := wire.NewMsgTx(1)
	spend.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: in.TxHash()}, []byte{1}, nil))
	spend.AddTxOut(wire.NewTxOut(30*btc, bob))
	spend.AddTxOut(wire.NewTxOut(70*btc, alice))
	chain.pool = append(chain.pool, spend)
	require.NoError(x.refreshMempool())
	c, p, _ = balanceOf(x, alice)
	require.Zero(c, "the spent output no longer counts")
	require.Equal(int64(-30*btc), p)
	chain.mine()
	require.NoError(x.sync())
	require.NoError(x.refreshMempool())
	c, _, n = balanceOf(x, alice)
	require.Equal(int64(70*btc), c)
	require.Equal(2, n)

	// Created and spent within one block.
	first := chain.pay(nil, 10*btc, alice)
	second := wire.NewMsgTx(1)
	second.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: first.TxHash()}, []byte{1}, nil))
	second.AddTxOut(wire.NewTxOut(10*btc, bob))
	chain.pool = append(chain.pool, second)
	chain.mine()
	require.NoError(x.sync())
	c, _, _ = balanceOf(x, alice)
	require.Equal(int64(70*btc), c)

	// A reorganisation that drops the last block's transactions back to the
	// mempool, then mines them again.
	late := chain.pay(nil, 1*btc, alice)
	chain.mine()
	require.NoError(x.sync())
	c, _, _ = balanceOf(x, alice)
	require.Equal(int64(71*btc), c)
	chain.reorg(2, true)
	require.NoError(x.sync())
	require.NoError(x.refreshMempool())
	c, p, _ = balanceOf(x, alice)
	require.Equal(int64(70*btc), c, "the reorganised block's payment is unconfirmed again")
	require.Equal(int64(1*btc), p)
	chain.mine()
	require.NoError(x.sync())
	require.NoError(x.refreshMempool())
	c, p, _ = balanceOf(x, alice)
	require.Equal(int64(71*btc), c)
	require.Zero(p)
	_ = late

	// The 5 BTC paid before registration, added by transaction ID.
	var early chainhash.Hash
	for _, tx := range chain.blocks[1][1:] {
		early = tx.TxHash()
	}
	added, err := x.importTx(alice, early)
	require.NoError(err)
	require.Equal(1, added)
	c, _, _ = balanceOf(x, alice)
	require.Equal(int64(76*btc), c)
	_, err = x.importTx(alice, in.TxHash())
	require.ErrorContains(err, "no unspent output", "already spent")
	_, err = x.importTx(bob, early)
	require.ErrorContains(err, "not registered")

	// Reopening keeps everything, including which addresses are watched.
	require.NoError(x.close())
	x, err = openBTCIndex(path, chain)
	require.NoError(err)
	defer x.close()
	_, watched := x.isWatched(alice)
	require.True(watched)
	c, _, _ = balanceOf(x, alice)
	require.Equal(int64(76*btc), c)
}
