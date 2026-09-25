package main

// The web wallet's Bitcoin side: balances, history and sending for the
// user's own Bitcoin address, from the same key as their BTCVM
// address. Addresses are registered with the index (dogeindex.go) first.

import (
	"encoding/hex"
	"log"
	"net/http"
	"strconv"

	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
)

func (srv *server) btcAddress(s string) (btcutil.Address, []byte, error) {
	addr, err := btcutil.DecodeAddress(s, srv.b.btcParams)
	if err != nil || !addr.IsForNet(srv.b.btcParams) {
		return nil, nil, badRequest("not a Bitcoin %s address: %q", srv.b.btcParams.Name, s)
	}
	dest, err := destinationOf(addr)
	if err != nil {
		return nil, nil, badRequest("%v", err)
	}
	return addr, dest.pkScript(), nil
}

func (srv *server) requireIndex() error {
	if srv.btcIdx == nil {
		return &apiError{http.StatusNotFound, "this bridge does not serve Bitcoin balances"}
	}
	if tip, _ := srv.btcIdx.tip(); tip < 0 {
		return &apiError{http.StatusServiceUnavailable, "the bridge's Bitcoin node is still catching up; Bitcoin balances appear once it has"}
	}
	return nil
}

// btcWatch registers a Bitcoin address with the index.
func (srv *server) btcWatch(r *http.Request) (any, error) {
	if err := srv.requireIndex(); err != nil {
		return nil, err
	}
	var body struct {
		Address string `json:"address"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	addr, script, err := srv.btcAddress(body.Address)
	if err != nil {
		return nil, err
	}
	if _, watched := srv.btcIdx.isWatched(script); !watched && !srv.registerLimit.allow(clientIP(r)) {
		return nil, &apiError{http.StatusTooManyRequests, "too many new addresses from this IP; try later"}
	}
	from, err := srv.btcIdx.watch(script)
	if err != nil {
		return nil, err
	}
	return map[string]any{"address": addr.EncodeAddress(), "watchedFrom": from}, nil
}

// btcAddressHandler returns a registered address's balance, unspent outputs
// and history.
func (srv *server) btcAddressHandler(r *http.Request) (any, error) {
	if err := srv.requireIndex(); err != nil {
		return nil, err
	}
	addr, script, err := srv.btcAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	from, watched := srv.btcIdx.isWatched(script)
	if !watched {
		return nil, &apiError{http.StatusNotFound, "address is not registered"}
	}
	tip, _ := srv.btcIdx.tip()
	confs := func(height int64) int64 {
		if height == 0 {
			return 0
		}
		return tip - height + 1
	}
	utxos, history, pending := srv.btcIdx.view(script)
	var confirmed int64
	outs := []map[string]any{}
	for _, u := range utxos {
		confirmed += u.value
		outs = append(outs, map[string]any{
			"txid": u.op.Hash.String(), "vout": u.op.Index,
			"value": strconv.FormatInt(u.value, 10), "script": hex.EncodeToString(script),
			"confirmations": confs(u.height),
		})
	}
	hist := []map[string]any{}
	for _, h := range history {
		hist = append(hist, map[string]any{
			"txid": h.txid.String(), "net": formatSigned(h.net),
			"confirmations": confs(h.height), "time": h.unix,
		})
	}
	return map[string]any{
		"address":     addr.EncodeAddress(),
		"confirmed":   formatBTC(confirmed),
		"pending":     formatSigned(pending),
		"utxos":       outs,
		"history":     hist,
		"watchedFrom": from,
		"indexedTo":   tip,
	}, nil
}

// btcImport adds a payment made to a registered address before it was
// registered.
func (srv *server) btcImport(r *http.Request) (any, error) {
	if err := srv.requireIndex(); err != nil {
		return nil, err
	}
	var body struct {
		Address string `json:"address"`
		Txid    string `json:"txid"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	_, script, err := srv.btcAddress(body.Address)
	if err != nil {
		return nil, err
	}
	txid, err := chainhash.NewHashFromStr(body.Txid)
	if err != nil {
		return nil, badRequest("invalid transaction ID")
	}
	n, err := srv.btcIdx.importTx(script, *txid)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"imported": n}, nil
}

// btcRawTx returns a Bitcoin transaction's bytes, so the wallet can check
// what it spends.
func (srv *server) btcRawTx(r *http.Request) (any, error) {
	txid, err := chainhash.NewHashFromStr(r.PathValue("txid"))
	if err != nil {
		return nil, badRequest("invalid txid")
	}
	var hexTx string
	if err := srv.btc.rpc.call(&hexTx, "getrawtransaction", txid.String(), 0); err != nil {
		return nil, &apiError{http.StatusNotFound, "no such transaction on Bitcoin"}
	}
	return map[string]string{"hex": hexTx}, nil
}

// btcBroadcast sends a signed Bitcoin transaction through the bridge's
// node.
func (srv *server) btcBroadcast(r *http.Request) (any, error) {
	var body struct {
		Hex string `json:"hex"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	tx, err := decodeTx(body.Hex)
	if err != nil {
		return nil, badRequest("invalid transaction: %v", err)
	}
	txid, err := srv.btc.send(tx)
	if err != nil {
		return nil, broadcastError(err)
	}
	// Take the payment into the index's view of the mempool now, so the
	// coins it spends stop being offered at once, not on the next pass.
	if srv.btcIdx != nil {
		if err := srv.btcIdx.refreshMempool(); err != nil {
			log.Printf("bitcoin index mempool: %v", err)
		}
	}
	return map[string]string{"txid": txid.String()}, nil
}
