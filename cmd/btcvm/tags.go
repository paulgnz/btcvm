package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/txscript"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// Peg messages are carried in a transaction's single OP_RETURN output:
//
//	BVMD kind program  Bitcoin deposit to the peg; credit this BTCVM address
//	BVMI txid vout     BTCVM release crediting that Bitcoin deposit
//	BVMO kind program  BTCVM peg-out to the reserve; pay this Bitcoin address
//	BVMR txid          Bitcoin payment for that BTCVM peg-out
//	BVMF txid vout     Bitcoin refund of a deposit that was not credited
//
// kind is a destination kind (destP2PKH ... destP2TR) and program its 20- or
// 32-byte hash or key. txids are in internal byte order and vout is little
// endian.
var (
	tagDeposit = []byte("BVMD")
	tagRelease = []byte("BVMI")
	tagPegOut  = []byte("BVMO")
	tagPayment = []byte("BVMR")
	tagRefund  = []byte("BVMF")
)

const (
	destP2PKH  byte = 0 // 1...
	destP2SH   byte = 1 // 3...
	destP2WPKH byte = 2 // bc1q..., 20 bytes
	destP2WSH  byte = 3 // bc1q..., 32 bytes
	destP2TR   byte = 4 // bc1p...
)

// programSize is the length of a destination kind's program.
func programSize(kind byte) int {
	switch kind {
	case destP2PKH, destP2SH, destP2WPKH:
		return 20
	case destP2WSH, destP2TR:
		return 32
	}
	return 0
}

// destination is a standard address in a network-neutral form: the same key
// or script on Bitcoin and on BTCVM, so the same address string on both.
type destination struct {
	kind byte
	hash [32]byte // the program, left-aligned; programSize(kind) bytes used
}

func newDestination(kind byte, program []byte) (destination, error) {
	d := destination{kind: kind}
	if n := programSize(kind); n == 0 || len(program) != n {
		return d, errors.New("malformed destination")
	}
	copy(d.hash[:], program)
	return d, nil
}

// program is d's hash or key.
func (d destination) program() []byte { return d.hash[:programSize(d.kind)] }

func destinationOf(addr btcutil.Address) (destination, error) {
	switch a := addr.(type) {
	case *btcutil.AddressPubKeyHash:
		return newDestination(destP2PKH, a.Hash160()[:])
	case *btcutil.AddressScriptHash:
		return newDestination(destP2SH, a.Hash160()[:])
	case *btcutil.AddressWitnessPubKeyHash:
		return newDestination(destP2WPKH, a.WitnessProgram())
	case *btcutil.AddressWitnessScriptHash:
		return newDestination(destP2WSH, a.WitnessProgram())
	case *btcutil.AddressTaproot:
		return newDestination(destP2TR, a.WitnessProgram())
	}
	return destination{}, fmt.Errorf("unsupported address type %T", addr)
}

// address encodes d for params.
func (d destination) address(params *chaincfg.Params) (btcutil.Address, error) {
	switch d.kind {
	case destP2PKH:
		return btcutil.NewAddressPubKeyHash(d.program(), params)
	case destP2SH:
		return btcutil.NewAddressScriptHashFromHash(d.program(), params)
	case destP2WPKH:
		return btcutil.NewAddressWitnessPubKeyHash(d.program(), params)
	case destP2WSH:
		return btcutil.NewAddressWitnessScriptHash(d.program(), params)
	case destP2TR:
		return btcutil.NewAddressTaproot(d.program(), params)
	}
	return nil, errors.New("malformed destination")
}

func (d destination) pkScript() []byte {
	b := txscript.NewScriptBuilder()
	switch d.kind {
	case destP2PKH:
		b.AddOp(txscript.OP_DUP).AddOp(txscript.OP_HASH160).AddData(d.program()).
			AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_CHECKSIG)
	case destP2SH:
		b.AddOp(txscript.OP_HASH160).AddData(d.program()).AddOp(txscript.OP_EQUAL)
	case destP2WPKH, destP2WSH:
		b.AddOp(txscript.OP_0).AddData(d.program())
	case destP2TR:
		b.AddOp(txscript.OP_1).AddData(d.program())
	}
	script, _ := b.Script()
	return script
}

// destinationOfScript reads a standard output script.
func destinationOfScript(script []byte) (destination, error) {
	switch txscript.GetScriptClass(script) {
	case txscript.PubKeyHashTy:
		return newDestination(destP2PKH, script[3:23])
	case txscript.ScriptHashTy:
		return newDestination(destP2SH, script[2:22])
	case txscript.WitnessV0PubKeyHashTy:
		return newDestination(destP2WPKH, script[2:22])
	case txscript.WitnessV0ScriptHashTy:
		return newDestination(destP2WSH, script[2:34])
	case txscript.WitnessV1TaprootTy:
		return newDestination(destP2TR, script[2:34])
	}
	return destination{}, errors.New("not a standard address output")
}

// bytes is d as kind || program, its form in tags and the API.
func (d destination) bytes() []byte { return append([]byte{d.kind}, d.program()...) }

func encodeDestination(tag []byte, d destination) []byte {
	return append(append([]byte{}, tag...), d.bytes()...)
}

func decodeDestination(payload []byte) (destination, error) {
	if len(payload) < 1 {
		return destination{}, errors.New("malformed destination")
	}
	return newDestination(payload[0], payload[1:])
}

func encodeRelease(deposit wire.OutPoint) []byte {
	return encodeOutPointTag(tagRelease, deposit)
}

func encodeRefund(deposit wire.OutPoint) []byte {
	return encodeOutPointTag(tagRefund, deposit)
}

func encodeOutPointTag(tag []byte, op wire.OutPoint) []byte {
	out := append(append([]byte{}, tag...), op.Hash[:]...)
	return binary.LittleEndian.AppendUint32(out, op.Index)
}

func encodePayment(request chainhash.Hash) []byte {
	return append(append([]byte{}, tagPayment...), request[:]...)
}

// opReturnData returns the tag and payload of tx's OP_RETURN output, if it
// has exactly one.
func opReturnData(tx *wire.MsgTx) (tag, payload []byte, ok bool) {
	var data []byte
	found := 0
	for _, out := range tx.TxOut {
		if txscript.GetScriptClass(out.PkScript) != txscript.NullDataTy {
			continue
		}
		found++
		pushes, err := txscript.PushedData(out.PkScript)
		if err != nil {
			return nil, nil, false
		}
		data = bytes.Join(pushes, nil)
	}
	if found != 1 || len(data) < 4 {
		return nil, nil, false
	}
	return data[:4], data[4:], true
}

func nullData(data []byte) *wire.TxOut {
	script, err := txscript.NullDataScript(data)
	if err != nil {
		panic(err) // data is always far below the size limit
	}
	return wire.NewTxOut(0, script)
}

// parseRelease reads the deposit a VM release credits.
func parseRelease(tx *wire.MsgTx) (wire.OutPoint, bool) {
	return parseOutPointTag(tx, tagRelease)
}

// parseRefund reads the deposit a Bitcoin refund returns.
func parseRefund(tx *wire.MsgTx) (wire.OutPoint, bool) {
	return parseOutPointTag(tx, tagRefund)
}

func parseOutPointTag(tx *wire.MsgTx, want []byte) (wire.OutPoint, bool) {
	tag, payload, ok := opReturnData(tx)
	if !ok || !bytes.Equal(tag, want) || len(payload) != 36 {
		return wire.OutPoint{}, false
	}
	var op wire.OutPoint
	copy(op.Hash[:], payload[:32])
	op.Index = binary.LittleEndian.Uint32(payload[32:])
	return op, true
}

// parsePayment reads the VM peg-out a Bitcoin payment pays.
func parsePayment(tx *wire.MsgTx) (chainhash.Hash, bool) {
	tag, payload, ok := opReturnData(tx)
	if !ok || !bytes.Equal(tag, tagPayment) || len(payload) != 32 {
		return chainhash.Hash{}, false
	}
	var h chainhash.Hash
	copy(h[:], payload)
	return h, true
}

// parseDestinationTag reads a BVMD or BVMO destination.
func parseDestinationTag(tx *wire.MsgTx, want []byte) (destination, bool) {
	tag, payload, ok := opReturnData(tx)
	if !ok || !bytes.Equal(tag, want) {
		return destination{}, false
	}
	d, err := decodeDestination(payload)
	return d, err == nil
}
