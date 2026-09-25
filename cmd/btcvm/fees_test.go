package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
)

// TestPayoutToEveryAddressType pays peg-outs to legacy, SegWit and Taproot
// addresses; the destination survives the BVMO tag and the payout pays it.
func TestPayoutToEveryAddressType(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice := h.user(1)
	h.deposit(100*btc, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()

	for i, kind := range []byte{destP2PKH, destP2SH, destP2WPKH, destP2WSH, destP2TR} {
		program := make([]byte, programSize(kind))
		program[0] = byte(i + 1)
		dest, err := newDestination(kind, program)
		require.NoError(err)

		addr, err := dest.address(&chaincfg.MainNetParams)
		require.NoError(err)
		back, err := btcutil.DecodeAddress(addr.EncodeAddress(), &chaincfg.MainNetParams)
		require.NoError(err)
		fromAddr, err := destinationOf(back)
		require.NoError(err)
		require.Equal(dest, fromAddr, addr.EncodeAddress())
		fromScript, err := destinationOfScript(dest.pkScript())
		require.NoError(err)
		require.Equal(dest, fromScript)

		h.pegOut(5*btc, dest)
		require.NotEmpty(h.step(), addr.EncodeAddress())
		require.Equal(5*btc-h.feeOf(h.lastBTC()), paidTo(h.btc, dest), addr.EncodeAddress())
		h.btc.mine()
	}
	require.True(h.audit().solvent())
}

// TestPegAddressIsTheSameOnBothChains checks the peg on Bitcoin and the
// reserve on BTCVM are one P2WSH address.
func TestPegAddressIsTheSameOnBothChains(t *testing.T) {
	h := newHarness(t)
	h.b.btcParams = &chaincfg.MainNetParams
	vmParams, err := btcvmParams("mainnet")
	require.NoError(t, err)
	h.b.vmParams = vmParams
	peg, err := h.b.btcPegAddress()
	require.NoError(t, err)
	reserve, err := h.b.vmReserveAddress()
	require.NoError(t, err)
	require.Equal(t, peg.EncodeAddress(), reserve.EncodeAddress())
	require.Regexp(t, "^bc1q[a-z0-9]{58}$", peg.EncodeAddress())
}

func TestFeeRateStaysWithinPolicy(t *testing.T) {
	h := newHarness(t)
	h.b.minFeeRate, h.b.maxFeeRate = 2, 50
	for estimate, want := range map[int64]int64{1: 2, 7: 7, 500: 50} {
		h.b.feeRate = func() (int64, error) { return estimate, nil }
		require.Equal(t, want, h.b.currentFeeRate(), "estimate %d", estimate)
	}
}

// TestStuckPayoutIsReplaced checks a payout left unconfirmed is replaced by
// one spending the same outputs at a higher fee rate, so only one can
// confirm, and the peg-out is still paid once.
func TestStuckPayoutIsReplaced(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.b.minFeeRate, h.b.maxFeeRate = 1, 100
	for _, c := range h.signers {
		c.b.minFeeRate, c.b.maxFeeRate = 1, 100
	}
	rate := int64(5)
	h.b.feeRate = func() (int64, error) { return rate, nil }
	h.b.bumpAfter = time.Nanosecond

	alice, aliceOnBTC := h.user(1), h.user(2)
	h.deposit(100*btc, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()
	h.pegOut(40*btc, aliceOnBTC)
	require.NotEmpty(h.step())
	first := h.lastBTC()

	// The rate has not risen enough: nothing to do.
	rate = 6
	require.Empty(h.step())

	// Fees jump: the payout is replaced.
	rate = 20
	did := h.step()
	require.Contains(did, "replaced "+first.TxHash().String())
	second := h.lastBTC()
	require.NotEqual(first.TxHash(), second.TxHash())
	require.Len(second.TxIn, len(first.TxIn))
	for i := range first.TxIn {
		require.Equal(first.TxIn[i].PreviousOutPoint, second.TxIn[i].PreviousOutPoint, "same inputs")
	}
	require.Equal(int64(20), h.b.feeRateOf(second, h.spends(second)))
	require.Equal(40*btc-h.feeOf(second), paidTo(h.btc, aliceOnBTC), "paid once")

	// Once it confirms, nothing more happens.
	h.btc.mine()
	rate = 90
	require.Empty(h.step())
	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Zero(a.PendingPegOuts)
}

// TestSignerRefusesBadReplacements checks the rules a signer applies to a
// second transaction for a payout that is already unconfirmed on Bitcoin.
func TestSignerRefusesBadReplacements(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	for _, c := range append([]*cosigner{{b: h.b}}, h.signers...) {
		c.b.minFeeRate, c.b.maxFeeRate = 1, 100
	}
	h.b.feeRate = func() (int64, error) { return 5, nil }
	c := h.signers[0]

	alice, aliceOnBTC := h.user(1), h.user(2)
	h.deposit(100*btc, &alice, 6)
	h.deposit(100*btc, &alice, 6) // a second peg output to try instead
	require.NotEmpty(h.step())
	h.vm.mine()
	require.NotEmpty(h.step())
	h.vm.mine()
	req := h.pegOut(40*btc, aliceOnBTC)
	require.NotEmpty(h.step())
	first := h.lastBTC()

	s, err := h.b.load()
	require.NoError(err)
	p, ok := findPegOut(s.pegOuts, req.TxHash())
	require.True(ok)
	why := action{Kind: actionPayout, PegOut: req.TxHash().String()}
	request := func(inputs []utxo, rate int64) signRequest {
		prev, _, err := s.pegSpends(inputs)
		require.NoError(err)
		tx, err := h.b.buildPayout(inputs, prev, p.value, p.dest, encodePayment(p.txid), rate)
		require.NoError(err)
		return signRequest{Chain: chainBitcoin, Action: why, Tx: encodeTx(tx), FeeRate: rate}
	}
	r, err := h.b.replaceable(s, first.TxHash())
	require.NoError(err)

	_, _, _, err = c.check(request(r.inputs, 5))
	require.ErrorContains(err, "must pay more", "no higher fee")

	var other []utxo
	for _, u := range s.lockedUTXOs {
		if u.outPoint != r.inputs[0].outPoint && u.confirmations > 0 {
			other = append(other, u)
		}
	}
	require.NotEmpty(other)
	_, _, _, err = c.check(request(other[:1], 30))
	// Different inputs could confirm alongside the first payment.
	require.ErrorContains(err, "not a confirmed peg output")

	_, _, _, err = c.check(request(r.inputs, 101))
	require.ErrorContains(err, "outside")

	_, _, _, err = c.check(request(r.inputs, 30))
	require.NoError(err, "same inputs, higher fee")

	// Once the first payment is in a block, nothing replaces it.
	h.btc.mine()
	s, err = h.b.load()
	require.NoError(err)
	_, _, _, err = c.check(request(r.inputs, 40))
	require.ErrorContains(err, "already done")
}
