package main

import (
	"fmt"

	btcd "github.com/MetalBlockchain/btcvm/btcd"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
)

const satPerBTC = 1e8

// maxBTC is Bitcoin's MAX_MONEY in whole BTC.
const maxBTC = 21_000_000

func bitcoinParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &chaincfg.MainNetParams, nil
	case "testnet":
		return &chaincfg.TestNet3Params, nil
	case "regtest":
		return &chaincfg.RegressionNetParams, nil
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
			if whole > maxBTC {
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
