package main

import (
	"fmt"

	btcd "github.com/MetalBlockchain/btcvm/btcd"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
)

const satPerBTC = 1e8

// Bitcoin network encodings, from Bitcoin Core's chainparams.cpp. Only the
// fields address and key encoding use are set.
var (
	bitcoinMainNet = chaincfg.Params{Name: "mainnet", PubKeyHashAddrID: 30, ScriptHashAddrID: 22, PrivateKeyID: 158}
	bitcoinTestNet = chaincfg.Params{Name: "testnet", PubKeyHashAddrID: 113, ScriptHashAddrID: 196, PrivateKeyID: 241}
	bitcoinRegTest = chaincfg.Params{Name: "regtest", PubKeyHashAddrID: 111, ScriptHashAddrID: 196, PrivateKeyID: 239}
)

func bitcoinParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &bitcoinMainNet, nil
	case "testnet":
		return &bitcoinTestNet, nil
	case "regtest":
		return &bitcoinRegTest, nil
	}
	return nil, fmt.Errorf("unknown Bitcoin network %q (mainnet, testnet or regtest)", network)
}

func btcvmParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &btcd.BTCVMMainNetParams, nil
	case "testnet":
		return &btcd.BTCVMTestNetParams, nil
	}
	return nil, fmt.Errorf("unknown BTCVM network %q (mainnet or testnet)", network)
}

// formatBTC renders satoshis as a BTC decimal string without floating point.
func formatBTC(satoshis int64) string {
	sign := ""
	if satoshis < 0 {
		sign, satoshis = "-", -satoshis
	}
	return fmt.Sprintf("%s%d.%08d", sign, satoshis/satPerBTC, satoshis%satPerBTC)
}

// parseBTC parses a BTC decimal string into satoshis without floating point.
func parseBTC(s string) (int64, error) {
	var whole, frac int64
	var fracDigits int
	seenDot := false
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	for _, c := range s {
		switch {
		case c == '.' && !seenDot:
			seenDot = true
		case c >= '0' && c <= '9' && !seenDot:
			whole = whole*10 + int64(c-'0')
			if whole > 10_000_000_000 {
				return 0, fmt.Errorf("amount %s is too large", s)
			}
		case c >= '0' && c <= '9' && fracDigits < 8:
			frac = frac*10 + int64(c-'0')
			fracDigits++
		default:
			return 0, fmt.Errorf("invalid amount %q", s)
		}
	}
	for ; fracDigits < 8; fracDigits++ {
		frac *= 10
	}
	return whole*satPerBTC + frac, nil
}
