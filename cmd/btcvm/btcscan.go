package main

// Finding old coins. A wallet importing keys (for example from a Bitcoin
// Core wallet.dat) needs the coins its addresses hold now, including ones
// paid long before the index watched them. Bitcoin Core's scantxoutset reads
// the whole UTXO set for them: a minute or two on mainnet, and only one scan
// at a time, so a scan is a job the wallet starts and then polls. The wallet
// sends addresses only; its keys never leave it.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
)

const (
	maxScanAddresses = 5000
	scanKeep         = 15 * time.Minute // how long a finished scan's result is kept
)

type scanJob struct {
	id       string
	started  time.Time
	finished time.Time
	err      string
	utxos    []map[string]any
	height   int64
}

type scanner struct {
	rpc   *rpcClient // a client with a long timeout: a scan takes minutes
	limit *rateLimit

	mu      sync.Mutex
	jobs    map[string]*scanJob
	running *scanJob
}

func newScanner(base *rpcClient) *scanner {
	rpc := newRPCClient(base.url, base.user, base.pass)
	rpc.http.Timeout = 30 * time.Minute
	return &scanner{rpc: rpc, limit: newRateLimit(5, time.Hour), jobs: map[string]*scanJob{}}
}

// btcScan starts a scan for up to maxScanAddresses Bitcoin addresses.
func (srv *server) btcScan(r *http.Request) (any, error) {
	var body struct {
		Addresses []string `json:"addresses"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	if len(body.Addresses) == 0 || len(body.Addresses) > maxScanAddresses {
		return nil, badRequest("send 1 to %d addresses", maxScanAddresses)
	}
	objects := make([]string, len(body.Addresses))
	for i, a := range body.Addresses {
		addr, err := btcutil.DecodeAddress(a, srv.b.btcParams)
		if err != nil || !addr.IsForNet(srv.b.btcParams) {
			return nil, badRequest("not a Bitcoin %s address: %q", srv.b.btcParams.Name, a)
		}
		objects[i] = "addr(" + addr.EncodeAddress() + ")"
	}

	sc := srv.scan
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.running != nil {
		return nil, &apiError{http.StatusServiceUnavailable, "another scan is running; try again in a few minutes"}
	}
	if !sc.limit.allow(clientIP(r)) {
		return nil, &apiError{http.StatusTooManyRequests, "too many scans from this IP; try later"}
	}
	var id [12]byte
	_, _ = rand.Read(id[:])
	job := &scanJob{id: hex.EncodeToString(id[:]), started: time.Now()}
	sc.jobs[job.id] = job
	sc.running = job
	go sc.run(job, objects)
	return map[string]any{"id": job.id}, nil
}

func (sc *scanner) run(job *scanJob, objects []string) {
	var res struct {
		Success  bool  `json:"success"`
		Height   int64 `json:"height"`
		Unspents []struct {
			TxID         string  `json:"txid"`
			Vout         uint32  `json:"vout"`
			ScriptPubKey string  `json:"scriptPubKey"`
			Amount       float64 `json:"amount"`
			Height       int64   `json:"height"`
		} `json:"unspents"`
	}
	err := sc.rpc.callNamed(&res, "scantxoutset", map[string]any{"action": "start", "scanobjects": objects})
	if err == nil && !res.Success {
		err = errors.New("the scan was aborted")
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()
	job.finished = time.Now()
	sc.running = nil
	if err != nil {
		log.Printf("bitcoin scan %s: %v", job.id, err)
		job.err = "the scan failed; try again"
		return
	}
	job.height = res.Height
	job.utxos = []map[string]any{}
	for _, u := range res.Unspents {
		job.utxos = append(job.utxos, map[string]any{
			"txid": u.TxID, "vout": u.Vout, "script": u.ScriptPubKey,
			// Bitcoin Core reports amounts as JSON numbers in BTC.
			"value":  strconv.FormatInt(int64(math.Round(u.Amount*satPerBTC)), 10),
			"height": u.Height,
		})
	}
	// Forget finished scans after a while.
	for id, j := range sc.jobs {
		if !j.finished.IsZero() && time.Since(j.finished) > scanKeep {
			delete(sc.jobs, id)
		}
	}
}

// btcScanStatus reports a scan: running (with progress), failed, or done
// with the unspent outputs it found.
func (srv *server) btcScanStatus(r *http.Request) (any, error) {
	sc := srv.scan
	sc.mu.Lock()
	job, ok := sc.jobs[r.PathValue("id")]
	var running bool
	if ok {
		running = job.finished.IsZero()
	}
	sc.mu.Unlock()
	if !ok {
		return nil, &apiError{http.StatusNotFound, "no such scan; it may have expired"}
	}
	if running {
		var st *struct {
			Progress float64 `json:"progress"`
		}
		_ = sc.rpc.callNamed(&st, "scantxoutset", map[string]any{"action": "status"})
		progress := 0.0
		if st != nil {
			progress = st.Progress
		}
		return map[string]any{"status": "running", "progress": progress}, nil
	}
	if job.err != "" {
		return map[string]any{"status": "failed", "error": job.err}, nil
	}
	return map[string]any{"status": "done", "height": job.height, "utxos": job.utxos}, nil
}
