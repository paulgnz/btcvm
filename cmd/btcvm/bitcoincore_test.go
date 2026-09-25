package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
)

// regtestNode is a private Bitcoin Core on regtest, for tests only. It
// enforces mainnet's relay policy (-acceptnonstdtxn=0), so what it accepts
// mainnet nodes relay too.
type regtestNode struct {
	t        *testing.T
	settings settings
	funder   *rpcClient // a wallet holding mined coins
	node     *rpcClient
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func startRegtest(t *testing.T, extra ...string) *regtestNode {
	bitcoind, err := exec.LookPath("bitcoind")
	if err != nil {
		t.Skip("bitcoind is not installed")
	}
	dir := t.TempDir()
	var secret [16]byte
	_, _ = rand.Read(secret[:])
	pass := hex.EncodeToString(secret[:])
	rpcPort := freePort(t)
	args := append([]string{"-regtest", "-datadir=" + dir, "-txindex", "-acceptnonstdtxn=0",
		"-listen=0", fmt.Sprintf("-rpcport=%d", rpcPort), "-rpcuser=test", "-rpcpassword=" + pass,
		"-fallbackfee=0.0002", "-printtoconsole=0"}, extra...)
	cmd := exec.Command(bitcoind, args...)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	n := &regtestNode{t: t}
	n.settings = settings{
		btcRPC: fmt.Sprintf("http://127.0.0.1:%d", rpcPort), btcUser: "test", btcPass: pass,
		btcNet: "regtest", btcWallet: "btcvm", btcParams: &chaincfg.RegressionNetParams,
	}
	n.node = n.settings.btcWalletClient("")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := n.node.call(nil, "getblockchaininfo"); err == nil {
			break
		}
		require.True(t, time.Now().Before(deadline), "bitcoind did not start (%s)", filepath.Join(dir, "regtest", "debug.log"))
		time.Sleep(200 * time.Millisecond)
	}
	require.NoError(t, n.node.callNamed(nil, "createwallet", map[string]any{"wallet_name": "funder"}))
	n.funder = n.settings.btcWalletClient("funder")
	n.mine(101) // mature coinbases to spend
	return n
}

func (n *regtestNode) mine(blocks int) {
	var addr string
	require.NoError(n.t, n.funder.call(&addr, "getnewaddress"))
	require.NoError(n.t, n.node.call(nil, "generatetoaddress", blocks, addr))
}

func (n *regtestNode) newAddress() destination {
	var s string
	require.NoError(n.t, n.funder.call(&s, "getnewaddress"))
	addr, err := btcutil.DecodeAddress(s, &chaincfg.RegressionNetParams)
	require.NoError(n.t, err)
	d, err := destinationOf(addr)
	require.NoError(n.t, err)
	return d
}

func (n *regtestNode) inMempool(txid chainhash.Hash) bool {
	var ids []string
	require.NoError(n.t, n.node.call(&ids, "getrawmempool"))
	for _, id := range ids {
		if id == txid.String() {
			return true
		}
	}
	return false
}

// TestBridgeWithBitcoinCore runs a round trip against a real Bitcoin Core:
// a deposit to a personal P2WSH address is credited, a withdrawal is paid
// on Bitcoin, the stalled payout is replaced at a higher fee rate under
// Core's BIP125 rules, and the peg stays solvent. BTCVM is the in-memory
// fake; Bitcoin is real.
func TestBridgeWithBitcoinCore(t *testing.T) {
	require := require.New(t)
	n := startRegtest(t)

	h := newHarness(t)
	btcChain := &btcChain{rpc: n.settings.btcRPCClient()}
	require.NoError(btcChain.ensureWallet())
	require.NoError(btcChain.ensureWallet(), "loading an existing wallet")
	h.b.btc = btcChain
	h.b.btcParams = &chaincfg.RegressionNetParams
	h.b.minFeeRate, h.b.maxFeeRate = 1, 100
	rate := int64(2)
	h.b.feeRate = func() (int64, error) { return rate, nil }
	h.b.minDeposit, h.b.minPegOut = 10_000, 30_000
	require.NoError(watchPeg(h.b, false))

	// A deposit from an ordinary wallet to Alice's personal address.
	alice := h.user(1)
	depositAddr, err := registerDeposit(h.b, alice)
	require.NoError(err)
	require.Regexp("^bcrt1q", depositAddr.EncodeAddress())
	var depositTxid string
	require.NoError(n.funder.call(&depositTxid, "sendtoaddress", depositAddr.EncodeAddress(), 0.5))
	require.Empty(h.step(), "unconfirmed: not credited")
	n.mine(6)
	require.NotEmpty(h.step())
	require.Equal(btc/2-h.b.vmFee, paidTo(h.vm, alice))
	h.vm.mine()

	// Withdraw to an address in the funder wallet.
	aliceOnBTC := n.newAddress()
	h.pegOut(btc/5, aliceOnBTC)
	did, err := h.b.step()
	require.NoError(err)
	require.Contains(did, "paid")
	s, err := h.b.load()
	require.NoError(err)
	require.Len(s.unconfirmed, 1)
	var first chainhash.Hash
	for id := range s.unconfirmed {
		first = id
	}
	require.True(n.inMempool(first), "Bitcoin Core accepted the payout")

	// Fees rise and the payout has waited: it is replaced.
	h.b.bumpAfter = time.Nanosecond
	rate = 12
	did, err = h.b.step()
	require.NoError(err)
	require.Contains(did, "replaced "+first.String())
	require.False(n.inMempool(first), "the first payout left the mempool")
	s, err = h.b.load()
	require.NoError(err)
	require.Len(s.unconfirmed, 1, "the replaced payout no longer counts: %v", s.unconfirmed)
	var second chainhash.Hash
	for id := range s.unconfirmed {
		second = id
	}
	require.NotEqual(first, second)
	require.True(n.inMempool(second))

	// Nothing more to do: paid once, and not bumped again at the same rate.
	require.Empty(h.step())
	n.mine(1)
	require.Empty(h.step())

	var received float64
	addr, err := aliceOnBTC.address(&chaincfg.RegressionNetParams)
	require.NoError(err)
	require.NoError(n.funder.call(&received, "getreceivedbyaddress", addr.EncodeAddress(), 1))
	s, err = h.b.load()
	require.NoError(err)
	require.InDelta(float64(s.paidAmount[h.vm.txs[len(h.vm.txs)-1].tx.TxHash()])/btc, received, 1e-9)
	require.Less(received, 0.2)
	require.Greater(received, 0.199)

	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Zero(a.PendingPegOuts)
	require.Equal(int64(btc/2-btc/5), a.Circulating)
}

// TestSeparateSignersWithBitcoinCore has three signers, each with its own
// Bitcoin Core wallet, sign a round trip for a coordinator holding no keys.
// The signers are not told of the deposit address until the release that
// needs it, so each must import the deposit it missed (importprunedfunds)
// from its own node.
func TestSeparateSignersWithBitcoinCore(t *testing.T) {
	require := require.New(t)
	n := startRegtest(t)

	h := newCosignHarness(t)
	wallet := func(name string) *btcChain {
		s := n.settings
		s.btcWallet = name
		c := &btcChain{rpc: s.btcRPCClient()}
		require.NoError(c.ensureWallet())
		return c
	}
	setup := func(b *bridge, c *btcChain) {
		b.btc, b.btcParams = c, &chaincfg.RegressionNetParams
		b.minFeeRate, b.maxFeeRate = 1, 100
		b.minDeposit, b.minPegOut = 10_000, 30_000
	}
	setup(h.b, wallet("btcvm"))
	h.b.feeRate = func() (int64, error) { return 3, nil }
	for i, c := range h.signers {
		setup(c.b, wallet(fmt.Sprintf("signer%d", i)))
		require.NoError(watchPeg(c.b, false))
	}
	require.NoError(watchPeg(h.b, false))

	// The coordinator registers Alice's address without telling the
	// signers, as if they were offline.
	cosigners := h.b.cosigners
	h.b.cosigners = nil
	alice := h.user(1)
	depositAddr, err := registerDeposit(h.b, alice)
	require.NoError(err)
	h.b.cosigners = cosigners

	require.NoError(n.funder.call(nil, "sendtoaddress", depositAddr.EncodeAddress(), 0.25))
	n.mine(6)
	require.NotEmpty(h.step(), "signers caught up on the deposit and signed")
	require.Equal(btc/4-h.b.vmFee, paidTo(h.vm, alice))
	h.vm.mine()

	aliceOnBTC := n.newAddress()
	h.pegOut(btc/10, aliceOnBTC)
	did, err := h.b.step()
	require.NoError(err)
	require.Contains(did, "paid")
	n.mine(1)
	require.Empty(h.step())

	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Zero(a.PendingPegOuts)
	for _, c := range h.signers {
		s, err := c.b.load()
		require.NoError(err)
		require.Equal(int64(btc/4-btc/10), c.b.audit(s).Locked, "each signer sees the peg on its own node")
	}
}

// TestBridgeRefusesNodeWithoutTxIndex checks the bridge won't read the peg
// from a Bitcoin Core without -txindex: it could not see every payout, and
// signers could then pay one twice.
func TestBridgeRefusesNodeWithoutTxIndex(t *testing.T) {
	n := startRegtest(t, "-txindex=0")
	c := &btcChain{rpc: n.settings.btcRPCClient()}
	require.NoError(t, c.ensureWallet())
	_, err := c.txsFor(nil)
	require.ErrorContains(t, err, "-txindex")
}
