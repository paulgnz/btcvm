package main

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MetalBlockchain/btcvm/btcd/btcec/v2"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// all: includes files starting with _, such as noble-hashes/_md.js.
//
//go:embed all:web
var webFiles embed.FS

// server is the public web API behind the web wallet. It holds the node RPC
// credentials; browsers get read access, broadcast, deposit-address
// registration and the faucet, and never see a key or password.
type server struct {
	b       *bridge
	health  *healthChecker
	chainID string // the BTCVM chain's ID on Metal, for links
	vm      *vmChain
	btc     *btcChain
	faucet  *faucet
	btcIdx  *btcIndex // Bitcoin balances for the wallet; nil if disabled

	finality *finalityMeter // how long payments take to be final, measured live

	mu       sync.RWMutex
	snapshot *snapshot

	supply atomic.Pointer[btcSupply]

	cacheMu  sync.Mutex
	prevOuts map[wire.OutPoint]*wire.TxOut

	// heavy bounds concurrent explorer requests, each of which can make
	// many RPC calls.
	heavy chan struct{}
	// scan finds the unspent outputs of imported addresses (btcscan.go).
	scan *scanner

	registerLimit *rateLimit // new registrations per client network
	registerTotal *rateLimit // new registrations in all
}

// btcSync is the part of Bitcoin Core's getblockchaininfo the page shows
// while the node catches up; deposits are not seen until it has.
type btcSync struct {
	Headers              int64   `json:"headers"`
	VerificationProgress float64 `json:"verificationprogress"`
}

// snapshot is the bridge state, refreshed in the background so requests do
// not each rescan both chains.
type snapshot struct {
	state     *pegState
	audit     audit
	vmHeight  int64
	btcHeight int64
	btcSync   btcSync
	btcTime   int64 // when the latest Bitcoin block was found, unix seconds
	feeRate   int64 // what a Bitcoin payout pays now, sat/vB
	checks    []check
	updated   time.Time
	err       string
}

func (srv *server) refresh() {
	snap := &snapshot{updated: time.Now()}
	state, err := srv.b.load()
	if err != nil {
		snap.err = err.Error()
	} else {
		snap.state = state
		snap.audit = srv.b.audit(state)
	}
	snap.checks = srv.health.run(state, err)
	snap.feeRate = srv.b.currentFeeRate()
	_ = srv.vm.rpc.call(&snap.vmHeight, "getblockcount")
	_ = srv.btc.rpc.call(&snap.btcHeight, "getblockcount")
	_ = srv.btc.rpc.call(&snap.btcSync, "getblockchaininfo")
	var best string
	if srv.btc.rpc.call(&best, "getbestblockhash") == nil {
		var header struct {
			Time int64 `json:"time"`
		}
		if srv.btc.rpc.call(&header, "getblockheader", best) == nil {
			snap.btcTime = header.Time
		}
	}

	srv.mu.Lock()
	srv.snapshot = snap
	srv.mu.Unlock()
}

func (srv *server) current() *snapshot {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	return srv.snapshot
}

// feeRate is the payout fee rate as of the last refresh.
func (srv *server) feeRate() int64 {
	if snap := srv.current(); snap != nil && snap.feeRate > 0 {
		return snap.feeRate
	}
	return srv.b.minFeeRate
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// handle adapts a handler returning a JSON value or an error.
func handle(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		result, err := fn(r)
		if err != nil {
			status := http.StatusInternalServerError
			var aerr *apiError
			if errors.As(err, &aerr) {
				status = aerr.status
			} else {
				log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}
}

func decodeBody(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func (srv *server) vmAddress(s string) (btcutil.Address, destination, error) {
	addr, err := btcutil.DecodeAddress(s, srv.b.vmParams)
	if err != nil || !addr.IsForNet(srv.b.vmParams) {
		return nil, destination{}, badRequest("not a BTCVM %s address: %q", srv.b.vmParams.Name, s)
	}
	dest, err := destinationOf(addr)
	if err != nil {
		return nil, destination{}, badRequest("%v", err)
	}
	return addr, dest, nil
}

func (srv *server) info(*http.Request) (any, error) {
	feeRate := srv.feeRate()
	pegAddr, err := srv.b.btcPegAddress()
	if err != nil {
		return nil, err
	}
	reserveAddr, err := srv.b.vmReserveAddress()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"btcvmNetwork":         srv.b.vmParams.Name,
		"bitcoinNetwork":       srv.b.btcParams.Name,
		"pegAddress":           pegAddr.EncodeAddress(),
		"reserveAddress":       reserveAddr.EncodeAddress(),
		"signers":              map[string]any{"required": srv.b.signers.Required, "publicKeys": srv.b.signers.PublicKeys},
		"depositConfirmations": srv.b.depositConfirmations,
		"confirmationTiers":    tiersForAPI(srv.b.confirmationTiers),
		"vmFee":                formatBTC(srv.b.vmFee),
		"btcFeeRate":           feeRate,
		"payoutFee":            formatBTC(srv.b.payoutFee(feeRate)),
		"feeRateRange":         []int64{srv.b.minFeeRate, srv.b.maxFeeRate},
		"minDeposit":           formatBTC(srv.b.minDeposit),
		"minPegOut":            formatBTC(srv.b.minPegOut),
		"maxDeposit":           formatBTC(srv.b.maxDeposit),
		"maxCirculating":       formatBTC(srv.b.maxCirculating),
		"faucet":               srv.faucet.info(),
		"btcWallet":            srv.btcIdx != nil,
		"btcvmVersions":        addressVersions(srv.b.vmParams),
		"bitcoinVersions":      addressVersions(srv.b.btcParams),
		"chainID":              srv.chainID,
	}, nil
}

// addressVersions are the base58 version bytes the web wallet needs to
// encode and check addresses and keys for a network.
// addressVersions is how a network encodes addresses and keys, for the web
// wallet.
func addressVersions(p *chaincfg.Params) map[string]any {
	return map[string]any{"p2pkh": p.PubKeyHashAddrID, "p2sh": p.ScriptHashAddrID, "wif": p.PrivateKeyID, "hrp": p.Bech32HRPSegwit}
}

func formatAudit(a audit) map[string]any {
	return map[string]any{
		"solvent":            a.solvent(),
		"circulating":        formatBTC(a.Circulating),
		"locked":             formatBTC(a.Locked),
		"pendingPegIns":      formatBTC(a.PendingPegIns),
		"pendingPegOuts":     formatBTC(a.PendingPegOuts),
		"surplus":            formatBTC(a.Surplus),
		"unclaimedOnBitcoin": formatBTC(a.UnclaimedOnBTC),
		"unclaimedOnBTCVM":   formatBTC(a.UnclaimedOnVM),
	}
}

func (srv *server) status(*http.Request) (any, error) {
	snap := srv.current()
	if snap == nil {
		return nil, &apiError{http.StatusServiceUnavailable, "starting up"}
	}
	out := map[string]any{
		"btcvmHeight":   snap.vmHeight,
		"bitcoinHeight": snap.btcHeight,
		"bitcoinSync": map[string]any{
			"headers":  snap.btcSync.Headers,
			"progress": snap.btcSync.VerificationProgress,
			// Bitcoin Core reports progress just under 1 when caught up.
			"syncing": snap.btcSync.Headers > 0 && snap.btcHeight < snap.btcSync.Headers-6,
			// No headers means Bitcoin Core did not answer.
			"available": snap.btcSync.Headers > 0,
		},
		"updated": snap.updated.UTC().Format(time.RFC3339),
		// Bitcoin blocks come at random, a minute apart on average; wallets
		// say so when one is slow.
		"bitcoinBlockTime": snap.btcTime,
	}
	if p := srv.b.paused(); p != nil {
		out["paused"] = p
	}
	if supply := srv.supply.Load(); supply != nil {
		out["bitcoinSupply"] = supply
	}
	if f := srv.finality.summary(); f != nil {
		out["finality"] = f
	}
	if snap.err != "" {
		out["error"] = snap.err
	} else {
		out["audit"] = formatAudit(snap.audit)
	}
	return out, nil
}

// healthHandler reports the health checks: 200 if all pass, 503 if one
// fails, for uptime monitors.
func (srv *server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	snap := srv.current()
	if snap == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "checks": []check{}})
		return
	}
	// A Bitcoin node catching up, or a deliberate pause, is degraded, not
	// down: uptime monitors should not page for them.
	status := "ok"
	for _, c := range snap.checks {
		if c.OK {
			continue
		}
		if (c.Name == "bitcoin" && strings.HasPrefix(c.Detail, "still syncing")) || c.Name == "pause" {
			if status == "ok" {
				status = "degraded"
			}
			continue
		}
		status = "down"
	}
	if status == "down" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": status == "ok", "status": status, "checks": snap.checks, "updated": snap.updated.UTC().Format(time.RFC3339),
	})
}

func (srv *server) address(r *http.Request) (any, error) {
	addr, _, err := srv.vmAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	utxos, err := srv.vm.unspent([]btcutil.Address{addr}, 0)
	if err != nil {
		return nil, err
	}
	txs, err := srv.vm.addressTxs(addr)
	if err != nil {
		return nil, err
	}

	var confirmed, pending int64
	outs := []map[string]any{}
	for _, u := range utxos {
		if u.confirmations > 0 {
			confirmed += u.value
		} else {
			pending += u.value
		}
		// Values are strings so JavaScript never rounds them.
		outs = append(outs, map[string]any{
			"txid": u.outPoint.Hash.String(), "vout": u.outPoint.Index,
			"value": strconv.FormatInt(u.value, 10), "script": hex.EncodeToString(u.pkScript),
			"confirmations": u.confirmations,
		})
	}

	// The address's outputs across its history, so each transaction's net
	// effect counts what it spent from the address, not just what it paid in.
	script := string(destinationScript(addr))
	owned := map[wire.OutPoint]int64{}
	for _, t := range txs {
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if string(out.PkScript) == script {
				owned[wire.OutPoint{Hash: hash, Index: uint32(i)}] = out.Value
			}
		}
	}
	history := []map[string]any{}
	for i := len(txs) - 1; i >= 0 && len(history) < 50; i-- {
		var in, out int64
		for _, txIn := range txs[i].tx.TxIn {
			in += owned[txIn.PreviousOutPoint]
		}
		for _, o := range txs[i].tx.TxOut {
			if string(o.PkScript) == script {
				out += o.Value
			}
		}
		history = append(history, map[string]any{
			"txid": txs[i].tx.TxHash().String(), "confirmations": txs[i].confirmations,
			"net": formatSigned(out - in), "time": txs[i].time,
		})
	}
	return map[string]any{
		"address":   addr.EncodeAddress(),
		"confirmed": formatBTC(confirmed),
		"pending":   formatBTC(pending),
		"utxos":     outs,
		"history":   history,
	}, nil
}

// btcSupply is all the BTC in existence, as the bridge's Bitcoin node
// counts it.
type btcSupply struct {
	Amount string `json:"amount"` // whole BTC
	Height int64  `json:"height"`
}

// watchSupply reads Bitcoin's supply from the node's UTXO set every six
// hours, once the node has caught up; before that the figure would be for
// the past. Summing Bitcoin's UTXO set takes minutes, so it gets its own
// long timeout, and skips the set's hash, which it does not need.
func (srv *server) watchSupply() {
	d := srv.btc.rpc
	rpc := newRPCClient(d.url, d.user, d.pass)
	rpc.http.Timeout = 30 * time.Minute
	for {
		if snap := srv.current(); snap != nil && snap.btcSync.Headers > 0 && snap.btcHeight >= snap.btcSync.Headers-6 {
			var info struct {
				Height      int64       `json:"height"`
				TotalAmount json.Number `json:"total_amount"`
			}
			if err := rpc.callNamed(&info, "gettxoutsetinfo", map[string]any{"hash_type": "none"}); err != nil {
				log.Printf("bitcoin supply: %v", err)
			} else {
				// Kept as whole BTC, as text.
				whole, _, _ := strings.Cut(info.TotalAmount.String(), ".")
				srv.supply.Store(&btcSupply{Amount: whole, Height: info.Height})
				time.Sleep(6 * time.Hour)
				continue
			}
		}
		time.Sleep(time.Minute)
	}
}

// limited runs fn once a slot among srv.heavy is free, or fails fast if the
// server is saturated.
func (srv *server) limited(fn func(*http.Request) (any, error)) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		select {
		case srv.heavy <- struct{}{}:
			defer func() { <-srv.heavy }()
			return fn(r)
		case <-time.After(10 * time.Second):
			return nil, &apiError{http.StatusServiceUnavailable, "busy; try again shortly"}
		}
	}
}

// rawTx returns a BTCVM transaction's bytes, so the wallet can check the
// value of each output it spends against the transaction's own ID.
func (srv *server) rawTx(r *http.Request) (any, error) {
	if _, err := chainhash.NewHashFromStr(r.PathValue("txid")); err != nil {
		return nil, badRequest("invalid txid")
	}
	var hexTx string
	if err := srv.vm.rpc.call(&hexTx, "getrawtransaction", r.PathValue("txid"), 0); err != nil {
		return nil, &apiError{http.StatusNotFound, "no such transaction on BTCVM"}
	}
	return map[string]string{"hex": hexTx}, nil
}

func formatSigned(satoshis int64) string {
	if satoshis < 0 {
		return "-" + formatBTC(-satoshis)
	}
	return formatBTC(satoshis)
}

// securityHeaders sets the headers a wallet page should have: scripts and
// connections only from this origin, no framing, no content sniffing.
func securityHeaders(next http.Handler) http.Handler {
	// connect-src: the wallet fetches old Bitcoin transactions from public
	// explorers when the bridge's pruned node no longer has them, and
	// checks them against their txid.
	const csp = "default-src 'self'; script-src 'self'; connect-src 'self' https://mempool.space https://blockstream.info; " +
		"style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; " +
		"img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (srv *server) broadcast(r *http.Request) (any, error) {
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
	// The finality meter's clock starts now, before the node sees it.
	srv.finality.received(tx.TxHash().String(), time.Now())
	txid, err := srv.vm.send(tx)
	if err != nil {
		srv.finality.forget(tx.TxHash().String())
		return nil, broadcastError(err)
	}
	return map[string]string{"txid": txid.String()}, nil
}

// broadcastError tells a transaction the node refused (400: it was not sent)
// from a node that didn't answer (502: it may or may not have been sent, so
// the wallet must keep its record and check the chain).
func broadcastError(err error) error {
	var rejected *rpcError
	if errors.As(err, &rejected) {
		return badRequest("rejected by the network: %s", rejected.Message)
	}
	log.Printf("broadcast: %v", err)
	return &apiError{http.StatusBadGateway, "no answer from the node, so the transaction may or may not have been sent; check its ID in the explorer before trying again"}
}

func (srv *server) depositAddress(r *http.Request) (any, error) {
	var body struct {
		Address string `json:"address"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	addr, dest, err := srv.vmAddress(body.Address)
	if err != nil {
		return nil, err
	}
	// Only new registrations count against the limits: each is permanent,
	// and one more address every load of the bridge reads.
	known, err := srv.b.registry.has(dest)
	if err != nil {
		return nil, err
	}
	if !known {
		if !srv.registerLimit.allow(limitKey(r)) {
			return nil, &apiError{http.StatusTooManyRequests, "too many new deposit addresses from this network; try later"}
		}
		if !srv.registerTotal.allow("*") {
			return nil, &apiError{http.StatusTooManyRequests, "too many new deposit addresses right now; try later"}
		}
	}
	depositAddr, err := registerDeposit(srv.b, dest)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"depositAddress": depositAddr.EncodeAddress(),
		"creditTo":       addr.EncodeAddress(),
		"redeemScript":   hex.EncodeToString(srv.b.signers.depositRedeemScript(dest)),
	}, nil
}

func (srv *server) deposits(r *http.Request) (any, error) {
	_, dest, err := srv.vmAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	snap := srv.current()
	out := []map[string]any{}
	if snap == nil || snap.state == nil {
		return out, nil
	}
	s := snap.state
	add := func(d deposit, status, reason string) {
		entry := map[string]any{
			"txid": d.outPoint.Hash.String(), "vout": d.outPoint.Index,
			"amount": formatBTC(d.value), "confirmations": d.confirmations,
			"required": srv.b.confirmationsFor(d.value), "status": status,
		}
		if reason != "" {
			entry["reason"] = reason
		}
		if release, ok := s.released[d.outPoint]; ok {
			entry["status"] = "credited"
			entry["creditTxid"] = release.String()
			entry["credited"] = formatBTC(d.value - srv.b.vmFee)
		}
		if refund, ok := s.refunded[d.outPoint]; ok {
			entry["status"] = "refunded"
			entry["refundTxid"] = refund.String()
		}
		out = append(out, entry)
	}
	for _, d := range s.deposits {
		if d.dest != dest {
			continue
		}
		status := "confirming"
		if d.confirmations >= srv.b.confirmationsFor(d.value) {
			status = "waiting_for_capacity" // the bridge credits within a poll unless the cap blocks it
			if srv.b.maxCirculating == 0 || s.reserveCreated-s.reserveUnspent+d.value <= srv.b.maxCirculating {
				status = "crediting"
			}
		}
		add(d, status, "")
	}
	// Held and refunded deposits to a personal address still name it.
	for _, d := range s.held {
		if d.dest == dest && d.dest != (destination{}) {
			add(d, "held", srv.b.holdReason(d))
		}
	}
	for _, d := range s.settled {
		if d.dest == dest && d.dest != (destination{}) {
			add(d, "refunded", "")
		}
	}
	return out, nil
}

func (srv *server) pegOut(r *http.Request) (any, error) {
	txid, err := chainhash.NewHashFromStr(r.PathValue("txid"))
	if err != nil {
		return nil, badRequest("invalid txid")
	}
	snap := srv.current()
	if snap == nil || snap.state == nil {
		return map[string]any{"status": "unknown"}, nil
	}
	for _, p := range snap.state.pegOuts {
		if p.txid != *txid {
			continue
		}
		to, _ := p.dest.address(srv.b.btcParams)
		out := map[string]any{
			"status": "pending", "amount": formatBTC(p.value),
			"pays": formatBTC(snap.state.pays(srv.b, p, snap.feeRate)), "to": to.EncodeAddress(),
		}
		if payment, ok := snap.state.paid[p.txid]; ok {
			out["status"] = "paid"
			out["paymentTxid"] = payment.String()
			// How far the payout is on Bitcoin: 0 while it waits for a block.
			var tx struct {
				Confirmations int64 `json:"confirmations"`
			}
			if err := srv.btc.rpc.callNamed(&tx, "gettransaction", map[string]any{"txid": payment.String()}); err == nil {
				out["paymentConfirmations"] = tx.Confirmations
			}
		}
		return out, nil
	}
	return map[string]any{"status": "unknown", "note": "not yet final on BTCVM, or not a valid peg-out"}, nil
}

func (srv *server) faucetClaim(r *http.Request) (any, error) {
	if srv.faucet == nil {
		return nil, &apiError{http.StatusNotFound, "no faucet on this network"}
	}
	var body struct {
		Address string `json:"address"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	addr, _, err := srv.vmAddress(body.Address)
	if err != nil {
		return nil, err
	}
	return srv.faucet.claim(srv.vm, srv.b.vmParams, addr, clientIP(r))
}

// clientIP is the request's client address. Behind a local reverse proxy it
// is the first X-Forwarded-For entry.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			return strings.TrimSpace(strings.Split(fwd, ",")[0])
		}
	}
	return host
}

// limitKey is who a request counts against for rate limits: its IPv4
// address, or its IPv6 /64, which one user typically has all of.
func limitKey(r *http.Request) string {
	ip := net.ParseIP(clientIP(r))
	if ip == nil || ip.To4() != nil {
		return clientIP(r)
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// rateLimit allows each key n events per window.
type rateLimit struct {
	n      int
	window time.Duration
	mu     sync.Mutex
	events map[string][]time.Time
}

func newRateLimit(n int, window time.Duration) *rateLimit {
	return &rateLimit{n: n, window: window, events: map[string][]time.Time{}}
}

func (l *rateLimit) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.events[key][:0]
	for _, t := range l.events[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.n {
		l.events[key] = recent
		return false
	}
	l.events[key] = append(recent, now)
	return true
}

// faucet hands out testnet BTC on BTCVM from a key the operator funds
// by pegging in.
type faucet struct {
	key       *btcec.PrivateKey
	amount    int64
	perAddr   *rateLimit
	perIP     *rateLimit
	sendMutex sync.Mutex
}

func (f *faucet) info() map[string]any {
	if f == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "amount": formatBTC(f.amount)}
}

func (f *faucet) claim(vm *vmChain, params *chaincfg.Params, to btcutil.Address, ip string) (any, error) {
	if !f.perAddr.allow(to.EncodeAddress()) || !f.perIP.allow(ip) {
		return nil, &apiError{http.StatusTooManyRequests, "faucet limit reached; try again tomorrow"}
	}
	// One at a time, so claims do not race for the same outputs.
	f.sendMutex.Lock()
	defer f.sendMutex.Unlock()
	txid, err := payFromKey(vm, params, f.key, destinationScript(to), f.amount, nil)
	if err != nil {
		return nil, fmt.Errorf("faucet: %w", err)
	}
	return map[string]string{"txid": txid.String(), "amount": formatBTC(f.amount)}, nil
}

func cmdServe(args []string) error {
	var s settings
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := flags.String("listen", "127.0.0.1:8080", "address to serve on")
	signersPath := flags.String("signers", "", "peg signer set file (public keys are enough)")
	depositsPath := flags.String("deposits", "", "deposit address registry (default: deposits.json next to -signers)")
	faucetKey := flags.String("faucet-key", "", "private key (WIF or hex) of the faucet's BTCVM address; empty disables the faucet")
	faucetAmount := flags.String("faucet-amount", "100", "BTC per faucet claim")
	chainID := flags.String("chain-id", "", "the BTCVM chain's ID on Metal, shown on the page")
	allowKeys := flags.Bool("allow-signing-keys", false, "accept a signer set with private keys (local development only)")
	btcIndexPath := flags.String("btc-index", "", "directory for the wallet's Bitcoin address index (default: btcindex next to -signers; \"off\" disables Bitcoin balances)")
	s.register(flags)
	b := bridgeFlags(flags)
	health := &healthChecker{b: b}
	health.register(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := s.resolve(); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	if err := signers.refuseKeys("btcvm serve", *allowKeys); err != nil {
		return err
	}
	if err := b.connect(&s, signers); err != nil {
		return err
	}
	b.registry = registryFor(*depositsPath, *signersPath)

	srv := &server{
		b:             b,
		health:        health,
		chainID:       *chainID,
		vm:            b.vm.(*vmChain),
		btc:           b.btc.(*btcChain),
		scan:          newScanner(b.btc.(*btcChain).rpc),
		registerLimit: newRateLimit(30, time.Hour),
		registerTotal: newRateLimit(500, time.Hour),
		prevOuts:      map[wire.OutPoint]*wire.TxOut{},
		heavy:         make(chan struct{}, 8),
	}
	if *faucetKey != "" {
		key, err := parseKey(*faucetKey)
		if err != nil {
			return fmt.Errorf("-faucet-key: %w", err)
		}
		amount, err := parseBTC(*faucetAmount)
		if err != nil {
			return fmt.Errorf("-faucet-amount: %w", err)
		}
		srv.faucet = &faucet{
			key: key, amount: amount,
			perAddr: newRateLimit(1, 24*time.Hour),
			perIP:   newRateLimit(3, 24*time.Hour),
		}
		faucetAddr, _ := keyAddress(key, s.vmParams)
		log.Printf("faucet: %s BTC per claim from %s", formatBTC(amount), faucetAddr.EncodeAddress())
	}
	// Watching the peg addresses waits on Bitcoin Core, which can be slow to
	// answer while it syncs; the site must not wait with it.
	go func() {
		for {
			err := watchPeg(b, false)
			if err == nil {
				return
			}
			log.Printf("importing peg addresses into Bitcoin Core (will retry): %v", err)
			time.Sleep(time.Minute)
		}
	}()

	go func() {
		for {
			srv.refresh()
			time.Sleep(15 * time.Second)
		}
	}()
	go srv.watchSupply()
	if *btcIndexPath != "off" {
		if *btcIndexPath == "" {
			*btcIndexPath = filepath.Join(filepath.Dir(*signersPath), "btcindex")
		}
		idx, err := openBTCIndex(*btcIndexPath, srv.btc)
		if err != nil {
			return fmt.Errorf("opening the Bitcoin index: %w", err)
		}
		srv.btcIdx = idx
		srv.finality = newFinalityMeter(filepath.Join(filepath.Dir(*btcIndexPath), "finality.json"))
		go idx.run(make(chan struct{}))
	}

	// Go does not know the web app manifest's type, and the page is served
	// with nosniff.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	// A wallet should always run its current code, so browsers check for a
	// newer file each time rather than reusing a cached one.
	files := http.FileServerFS(static)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	if err := handlePages(mux); err != nil {
		return err
	}
	// New blocks are pushed to open wallets, so they refresh the moment a
	// payment is final. Started after the Bitcoin index, which it reads.
	hub := newEventHub()
	go srv.watchBlocks(hub)
	mux.HandleFunc("GET /api/events", srv.events(hub))
	mux.HandleFunc("GET /api/info", handle(srv.info))
	mux.HandleFunc("GET /api/status", handle(srv.status))
	mux.HandleFunc("GET /api/health", srv.healthHandler)
	mux.HandleFunc("GET /api/blocks", handle(srv.limited(srv.blocksHandler)))
	mux.HandleFunc("GET /api/block/{id}", handle(srv.limited(srv.blockHandler)))
	mux.HandleFunc("GET /api/tx/{txid}", handle(srv.limited(srv.txHandler)))
	mux.HandleFunc("GET /api/activity", handle(srv.activityHandler))
	mux.HandleFunc("GET /api/reserves", handle(srv.reservesHandler))
	mux.HandleFunc("GET /api/address/{addr}", handle(srv.limited(srv.address)))
	mux.HandleFunc("GET /api/deposits/{addr}", handle(srv.deposits))
	mux.HandleFunc("GET /api/pegout/{txid}", handle(srv.pegOut))
	mux.HandleFunc("POST /api/tx", handle(srv.broadcast))
	mux.HandleFunc("GET /api/rawtx/{txid}", handle(srv.rawTx))
	mux.HandleFunc("POST /api/deposit-address", handle(srv.depositAddress))
	mux.HandleFunc("POST /api/faucet", handle(srv.faucetClaim))
	mux.HandleFunc("POST /api/btc/watch", handle(srv.btcWatch))
	mux.HandleFunc("GET /api/btc/address/{addr}", handle(srv.btcAddressHandler))
	mux.HandleFunc("POST /api/btc/import", handle(srv.limited(srv.btcImport)))
	mux.HandleFunc("GET /api/btc/rawtx/{txid}", handle(srv.limited(srv.btcRawTx)))
	mux.HandleFunc("POST /api/btc/tx", handle(srv.btcBroadcast))
	mux.HandleFunc("POST /api/btc/scan", handle(srv.btcScan))
	mux.HandleFunc("GET /api/btc/scan/{id}", handle(srv.btcScanStatus))

	log.Printf("serving on http://%s", *listen)
	server := &http.Server{
		Addr: *listen, Handler: securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
	return server.ListenAndServe()
}
