package btcd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MetalBlockchain/btcvm/btcd/blockchain"
	"github.com/MetalBlockchain/btcvm/btcd/btcec/v2"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil/hdkeychain"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// TestBTCVMGenesisHashes pins the genesis blocks. Changing either one starts a
// new chain, so it must be a deliberate edit to this test.
func TestBTCVMGenesisHashes(t *testing.T) {
	tests := []struct {
		params *chaincfg.Params
		want   string
	}{
		{&BTCVMMainNetParams, "e0cc27df465ffbfc06ac4d89fdf1ff6cd365cab7149961b6f749ad1e0f183de5"},
		{&BTCVMTestNetParams, "65f25687681d8a5eaf49bfb515d149336e1087522fa8ede05c27816ecf969466"},
	}
	for _, test := range tests {
		if got := test.params.GenesisHash.String(); got != test.want {
			t.Errorf("%s: genesis hash %s, want %s", test.params.Name, got, test.want)
		}
		if got := test.params.GenesisBlock.BlockHash(); got != *test.params.GenesisHash {
			t.Errorf("%s: GenesisHash does not match GenesisBlock", test.params.Name)
		}
		for _, out := range test.params.GenesisBlock.Transactions[0].TxOut {
			if out.Value != 0 {
				t.Errorf("%s: genesis output pays %d, want no premine", test.params.Name, out.Value)
			}
		}
	}
}

// TestBTCVMEncodings checks that addresses, WIF keys and extended keys use
// Bitcoin's encodings, so a key has the same address on BTCVM and Bitcoin.
func TestBTCVMEncodings(t *testing.T) {
	tests := []struct {
		params              *chaincfg.Params
		p2pkh, p2sh, wif    string
		extPriv, extPub     string
		p2wpkh, p2wsh, p2tr string
		bitcoin             *chaincfg.Params // the Bitcoin network it matches
	}{
		{&BTCVMMainNetParams, "1", "3", "K|L", "xprv", "xpub", "bc1q", "bc1q", "bc1p", &chaincfg.MainNetParams},
		{&BTCVMTestNetParams, "m|n", "2", "c", "tprv", "tpub", "tb1q", "tb1q", "tb1p", &chaincfg.TestNet3Params},
	}

	hash160 := bytes.Repeat([]byte{0x42}, 20)
	privKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x01}, 32))
	seed := bytes.Repeat([]byte{0x07}, hdkeychain.RecommendedSeedLen)

	for _, test := range tests {
		name := test.params.Name

		pkh, err := btcutil.NewAddressPubKeyHash(hash160, test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "P2PKH", pkh.EncodeAddress(), test.p2pkh)
		decoded, err := btcutil.DecodeAddress(pkh.EncodeAddress(), test.params)
		if err != nil || !decoded.IsForNet(test.params) {
			t.Errorf("%s: P2PKH address does not round-trip: %v", name, err)
		}

		sh, err := btcutil.NewAddressScriptHashFromHash(hash160, test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "P2SH", sh.EncodeAddress(), test.p2sh)

		wif, err := btcutil.NewWIF(privKey, test.params, true)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "WIF", wif.String(), test.wif)

		master, err := hdkeychain.NewMaster(seed, test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "extended private key", master.String(), test.extPriv)
		pub, err := master.Neuter()
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "extended public key", pub.String(), test.extPub)

		// SegWit and Taproot addresses.
		wpkh, err := btcutil.NewAddressWitnessPubKeyHash(hash160, test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "P2WPKH", wpkh.EncodeAddress(), test.p2wpkh)
		wsh, err := btcutil.NewAddressWitnessScriptHash(bytes.Repeat([]byte{0x42}, 32), test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "P2WSH", wsh.EncodeAddress(), test.p2wsh)
		tr, err := btcutil.NewAddressTaproot(bytes.Repeat([]byte{0x42}, 32), test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "P2TR", tr.EncodeAddress(), test.p2tr)

		// The very same strings on the matching Bitcoin network.
		for _, a := range []btcutil.Address{pkh, sh, wpkh, wsh, tr} {
			if _, err := btcutil.DecodeAddress(a.EncodeAddress(), test.bitcoin); err != nil {
				t.Errorf("%s: %s is not a valid %s address: %v", name, a.EncodeAddress(), test.bitcoin.Name, err)
			}
		}
		btcWIF, _ := btcutil.NewWIF(privKey, test.bitcoin, true)
		if btcWIF.String() != wif.String() {
			t.Errorf("%s: WIF %s differs from %s's %s", name, wif, test.bitcoin.Name, btcWIF)
		}
	}
}

func checkPrefix(t *testing.T, network, kind, encoded, allowed string) {
	t.Helper()
	// allowed lists alternative prefixes separated by "|".
	for _, p := range strings.Split(allowed, "|") {
		if strings.HasPrefix(encoded, p) {
			return
		}
	}
	t.Errorf("%s: %s %s does not start with %q", network, kind, encoded, allowed)
}

// TestBTCVMNoBlockSubsidy checks that BTCVM never mints BTC through the
// coinbase.
func TestBTCVMNoBlockSubsidy(t *testing.T) {
	for _, params := range []*chaincfg.Params{&BTCVMMainNetParams, &BTCVMTestNetParams} {
		for _, height := range []int32{0, 1, 100000, 210000, 1 << 30} {
			if got := blockchain.CalcBlockSubsidy(height, params); got != 0 {
				t.Errorf("%s: subsidy at height %d is %d, want 0", params.Name, height, got)
			}
		}
	}
}

// TestBitcoinMaxMoney checks that single outputs are bounded by Bitcoin's
// MAX_MONEY of 21 million BTC.
func TestBitcoinMaxMoney(t *testing.T) {
	const satPerBTC = 1e8

	tests := []struct {
		name  string
		value int64
		valid bool
	}{
		{"1 BTC", satPerBTC, true},
		{"exactly MAX_MONEY", 21e6 * satPerBTC, true},
		{"one satoshi over MAX_MONEY", 21e6*satPerBTC + 1, false},
	}
	for _, test := range tests {
		tx := wire.NewMsgTx(1)
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 0}, nil, nil))
		tx.AddTxOut(wire.NewTxOut(test.value, []byte{0x51}))

		err := blockchain.CheckTransactionSanity(btcutil.NewTx(tx))
		if (err == nil) != test.valid {
			t.Errorf("%s: CheckTransactionSanity error = %v, want valid=%v", test.name, err, test.valid)
		}
	}
}
