// Keys, addresses and transactions for BTCVM and Bitcoin, in the browser.
// Both chains use Bitcoin's formats, so one key has the same address on
// each. The wallet's own address is native SegWit (P2WPKH, bc1q...); it can
// pay any standard address.
import * as secp from './vendor/noble-secp256k1-2.3.0/index.js';
import { sha256 } from './vendor/noble-hashes-1.8.0/sha2.js';
import { ripemd160 } from './vendor/noble-hashes-1.8.0/legacy.js';

export const SATS = 100_000_000n;

// Fee rates in sat/vB. BTCVM blocks have room to spare, so twice the relay
// minimum always makes the next one. Bitcoin payments pay the bridge's
// current estimate, passed in; this is the fallback.
export const VM_FEE_RATE = 2n;
export const BTC_FEE_RATE = 5n;

// The smallest output the wallet creates: Bitcoin Core's dust threshold
// for the largest standard output. Change below it goes to the fee.
const DUST = 546n;

// No payment this page builds should cost more than this in fees; a larger
// figure means something is wrong, so refuse to sign.
const MAX_FEE = 250_000n;

// Inputs signal replaceability (BIP125), so a Bitcoin payment that stalls
// can be sent again with a higher fee.
const SEQUENCE = 0xfffffffd;

const concat = (...parts) => {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let i = 0;
  for (const p of parts) { out.set(p, i); i += p.length; }
  return out;
};
export const hex = (bytes) => Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
export const unhex = (s) => {
  if (!/^([0-9a-f]{2})*$/i.test(s)) throw new Error('not hex');
  return Uint8Array.from(s.match(/../g) || [], (b) => parseInt(b, 16));
};
const dsha = (b) => sha256(sha256(b));
const hash160 = (b) => ripemd160(sha256(b));

// --- base58check -----------------------------------------------------------

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

function b58encode(bytes) {
  let n = BigInt('0x' + (hex(bytes) || '0'));
  let out = '';
  while (n > 0n) { out = B58[Number(n % 58n)] + out; n /= 58n; }
  for (const b of bytes) { if (b !== 0) break; out = '1' + out; }
  return out;
}

function b58decode(s) {
  let n = 0n;
  for (const c of s) {
    const v = B58.indexOf(c);
    if (v < 0) throw new Error(`invalid character "${c}"`);
    n = n * 58n + BigInt(v);
  }
  let h = n.toString(16);
  if (h.length % 2) h = '0' + h;
  const body = n === 0n ? new Uint8Array() : unhex(h);
  const zeros = s.match(/^1*/)[0].length;
  return concat(new Uint8Array(zeros), body);
}

function checkEncode(version, payload) {
  const data = concat(Uint8Array.of(version), payload);
  return b58encode(concat(data, dsha(data).slice(0, 4)));
}

function checkDecode(s) {
  const raw = b58decode(s.trim());
  if (raw.length < 5) throw new Error('too short');
  const data = raw.slice(0, -4);
  const sum = dsha(data).slice(0, 4);
  if (hex(sum) !== hex(raw.slice(-4))) throw new Error('checksum mismatch: check for a typo');
  return { version: data[0], payload: data.slice(1) };
}

// --- bech32 and bech32m (BIP173, BIP350) -------------------------------------

const CHARSET = 'qpzry9x8gf2tvdw0s3jn54khce6mua7l';
const BECH32 = 1;
const BECH32M = 0x2bc830a3;

function polymod(values) {
  const G = [0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3];
  let chk = 1;
  for (const v of values) {
    const top = chk >>> 25;
    chk = ((chk & 0x1ffffff) << 5) ^ v;
    for (let i = 0; i < 5; i++) if ((top >>> i) & 1) chk ^= G[i];
  }
  return chk >>> 0;
}

const hrpExpand = (hrp) => [...[...hrp].map((c) => c.charCodeAt(0) >> 5), 0, ...[...hrp].map((c) => c.charCodeAt(0) & 31)];

// convertBits regroups bits, as bech32 carries 5 bits per character.
function convertBits(data, from, to, pad) {
  let acc = 0;
  let bits = 0;
  const out = [];
  const max = (1 << to) - 1;
  for (const v of data) {
    acc = (acc << from) | v;
    bits += from;
    while (bits >= to) { bits -= to; out.push((acc >> bits) & max); }
  }
  if (pad) {
    if (bits > 0) out.push((acc << (to - bits)) & max);
  } else if (bits >= from || ((acc << (to - bits)) & max)) {
    throw new Error('invalid padding');
  }
  return out;
}

function segwitEncode(hrp, version, program) {
  const data = [version, ...convertBits(program, 8, 5, true)];
  const mod = polymod([...hrpExpand(hrp), ...data, 0, 0, 0, 0, 0, 0]) ^ (version === 0 ? BECH32 : BECH32M);
  const checksum = [0, 1, 2, 3, 4, 5].map((i) => (mod >>> (5 * (5 - i))) & 31);
  return hrp + '1' + [...data, ...checksum].map((d) => CHARSET[d]).join('');
}

// segwitDecode returns the witness version and program of a bech32(m)
// address for hrp, or throws.
function segwitDecode(address, hrp) {
  if (address !== address.toLowerCase() && address !== address.toUpperCase()) throw new Error('mixed-case address');
  const s = address.toLowerCase();
  const pos = s.lastIndexOf('1');
  if (pos < 1 || pos + 7 > s.length || s.length > 90) throw new Error('not a valid address');
  if (s.slice(0, pos) !== hrp) throw new Error('address is for a different network');
  const data = [...s.slice(pos + 1)].map((c) => CHARSET.indexOf(c));
  if (data.includes(-1)) throw new Error('invalid character in address');
  const check = polymod([...hrpExpand(hrp), ...data]);
  const version = data[0];
  if (version > 16) throw new Error('not a valid address');
  if (check !== (version === 0 ? BECH32 : BECH32M)) throw new Error('checksum mismatch: check for a typo');
  const program = Uint8Array.from(convertBits(data.slice(1, -6), 5, 8, false));
  if (program.length < 2 || program.length > 40) throw new Error('not a valid address');
  return { version, program };
}

// --- addresses and scripts ---------------------------------------------------

// Destination kinds, as in cmd/btcvm's tags.go: {kind, hash}, hash being the
// 20- or 32-byte program.
export const P2PKH = 0;   // 1...
export const P2SH = 1;    // 3...
export const P2WPKH = 2;  // bc1q..., 20 bytes
export const P2WSH = 3;   // bc1q..., 32 bytes
export const P2TR = 4;    // bc1p...
const PROGRAM_SIZE = [20, 20, 20, 32, 32];

// decodeAddress returns the destination of an address on the network with
// the given versions ({p2pkh, p2sh, wif, hrp}).
export function decodeAddress(address, versions) {
  address = address.trim();
  if (address.toLowerCase().startsWith(versions.hrp + '1')) {
    const { version, program } = segwitDecode(address, versions.hrp);
    if (version === 0 && program.length === 20) return { kind: P2WPKH, hash: program };
    if (version === 0 && program.length === 32) return { kind: P2WSH, hash: program };
    if (version === 1 && program.length === 32) return { kind: P2TR, hash: program };
    throw new Error('unsupported SegWit address');
  }
  if (/^[a-z]{1,83}1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]{6,}$/i.test(address)) {
    throw new Error('address is for a different network');
  }
  const { version, payload } = checkDecode(address);
  if (payload.length !== 20) throw new Error('not an address');
  if (version === versions.p2pkh) return { kind: P2PKH, hash: payload };
  if (version === versions.p2sh) return { kind: P2SH, hash: payload };
  throw new Error('address is for a different network');
}

export function encodeAddress(dest, versions) {
  switch (dest.kind) {
    case P2PKH: return checkEncode(versions.p2pkh, dest.hash);
    case P2SH: return checkEncode(versions.p2sh, dest.hash);
    case P2WPKH: case P2WSH: return segwitEncode(versions.hrp, 0, dest.hash);
    case P2TR: return segwitEncode(versions.hrp, 1, dest.hash);
  }
  throw new Error('unknown address kind');
}

export function pkScript(dest) {
  switch (dest.kind) {
    case P2PKH: return concat(Uint8Array.of(0x76, 0xa9, 0x14), dest.hash, Uint8Array.of(0x88, 0xac));
    case P2SH: return concat(Uint8Array.of(0xa9, 0x14), dest.hash, Uint8Array.of(0x87));
    case P2WPKH: case P2WSH: return concat(Uint8Array.of(0x00, dest.hash.length), dest.hash);
    case P2TR: return concat(Uint8Array.of(0x51, 0x20), dest.hash);
  }
  throw new Error('unknown address kind');
}

// scriptDestination reads a standard output script, or returns null.
function scriptDestination(s) {
  if (s.length === 25 && s[0] === 0x76 && s[1] === 0xa9 && s[2] === 0x14 && s[23] === 0x88 && s[24] === 0xac) return { kind: P2PKH, hash: s.slice(3, 23) };
  if (s.length === 23 && s[0] === 0xa9 && s[1] === 0x14 && s[22] === 0x87) return { kind: P2SH, hash: s.slice(2, 22) };
  if (s.length === 22 && s[0] === 0x00 && s[1] === 0x14) return { kind: P2WPKH, hash: s.slice(2) };
  if (s.length === 34 && s[0] === 0x00 && s[1] === 0x20) return { kind: P2WSH, hash: s.slice(2) };
  if (s.length === 34 && s[0] === 0x51 && s[1] === 0x20) return { kind: P2TR, hash: s.slice(2) };
  return null;
}

function pushData(data) {
  if (data.length < 0x4c) return concat(Uint8Array.of(data.length), data);
  if (data.length <= 0xff) return concat(Uint8Array.of(0x4c, data.length), data);
  throw new Error('push too large');
}

const destBytes = (dest) => concat(Uint8Array.of(dest.kind), dest.hash);

// depositRedeemScript mirrors cmd/btcvm: <kind || program> OP_DROP, then
// the m-of-n multisig of the peg signers.
export function depositRedeemScript(dest, signers) {
  const keys = signers.publicKeys.map(unhex);
  const multisig = concat(
    Uint8Array.of(0x50 + signers.required),
    ...keys.map(pushData),
    Uint8Array.of(0x50 + keys.length, 0xae),
  );
  return concat(pushData(destBytes(dest)), Uint8Array.of(0x75), multisig);
}

// depositAddress computes dest's personal Bitcoin deposit address (P2WSH)
// from the signers' public keys, so the page can check what the server says.
export const depositAddress = (dest, signers, btcVersions) =>
  encodeAddress({ kind: P2WSH, hash: sha256(depositRedeemScript(dest, signers)) }, btcVersions);

// --- keys ----------------------------------------------------------------------

export function newPrivateKey() {
  return secp.utils.randomPrivateKey();
}

// parseKey accepts a compressed-key WIF (any network) or 64 hex characters.
// SegWit addresses are for compressed public keys, so an uncompressed WIF is
// refused.
export function parseKey(s) {
  s = s.trim();
  let key;
  if (/^[0-9a-f]{64}$/i.test(s)) {
    key = unhex(s);
  } else {
    const { payload } = checkDecode(s);
    if (payload.length === 32) throw new Error('this is an uncompressed-key WIF; export a compressed one');
    if (payload.length !== 33 || payload[32] !== 1) throw new Error('not a private key');
    key = payload.slice(0, 32);
  }
  if (!secp.utils.isValidPrivateKey(key)) throw new Error('not a valid private key');
  return key;
}

export const wif = (key, versions) => checkEncode(versions.wif, concat(key, Uint8Array.of(1)));

// keyDestination is the key's wallet address: native SegWit, P2WPKH.
export function keyDestination(key) {
  return { kind: P2WPKH, hash: hash160(secp.getPublicKey(key, true)) };
}

// --- amounts ---------------------------------------------------------------------

export function parseBTC(s) {
  const m = /^(\d{1,8})(?:\.(\d{1,8}))?$/.exec(s.trim());
  if (!m) throw new Error('enter an amount like 0.0025');
  const sats = BigInt(m[1]) * SATS + BigInt((m[2] || '').padEnd(8, '0'));
  if (sats > 21_000_000n * SATS) throw new Error('more than 21 million BTC');
  return sats;
}

export function formatBTC(sats) {
  sats = BigInt(sats);
  const whole = sats / SATS;
  const frac = (sats % SATS).toString().padStart(8, '0').replace(/0+$/, '');
  return whole.toLocaleString('en-US') + (frac ? '.' + frac : '');
}

// --- transactions ------------------------------------------------------------------

const u32 = (n) => { const b = new Uint8Array(4); new DataView(b.buffer).setUint32(0, n, true); return b; };
const u64 = (n) => { const b = new Uint8Array(8); new DataView(b.buffer).setBigUint64(0, BigInt(n), true); return b; };
function varint(n) {
  if (n < 0xfd) return Uint8Array.of(n);
  if (n <= 0xffff) return Uint8Array.of(0xfd, n & 0xff, n >> 8);
  return concat(Uint8Array.of(0xfe), u32(n));
}

const outpoint = (i) => concat(unhex(i.txid).reverse(), u32(i.vout));
const serializeOutput = (o) => concat(u64(o.value), varint(o.script.length), o.script);

// serialize writes tx, with its witnesses if it has any (BIP144).
function serialize(tx, withWitness = true) {
  const witness = withWitness && tx.inputs.some((i) => i.witness && i.witness.length);
  return concat(
    u32(tx.version),
    witness ? Uint8Array.of(0x00, 0x01) : new Uint8Array(),
    varint(tx.inputs.length),
    ...tx.inputs.flatMap((i) => [outpoint(i), varint(0), u32(i.sequence)]),
    varint(tx.outputs.length),
    ...tx.outputs.map(serializeOutput),
    ...(witness ? tx.inputs.flatMap((i) => [
      varint(i.witness.length), ...i.witness.flatMap((w) => [varint(w.length), w]),
    ]) : []),
    u32(0),
  );
}

// BIP143 SIGHASH_ALL for a P2WPKH input: commits to the amount it spends.
function sighash(tx, index, keyHash, value) {
  const i = tx.inputs[index];
  const scriptCode = concat(Uint8Array.of(0x19, 0x76, 0xa9, 0x14), keyHash, Uint8Array.of(0x88, 0xac));
  return dsha(concat(
    u32(tx.version),
    dsha(concat(...tx.inputs.map(outpoint))),
    dsha(concat(...tx.inputs.map((x) => u32(x.sequence)))),
    outpoint(i),
    scriptCode,
    u64(value),
    u32(i.sequence),
    dsha(concat(...tx.outputs.map(serializeOutput))),
    u32(0),
    u32(1),
  ));
}

// derSignature encodes a (low-S) signature in the strict DER form consensus
// requires.
function derSignature(sig) {
  const int = (n) => {
    let b = unhex(n.toString(16).padStart(64, '0'));
    let i = 0;
    while (i < b.length - 1 && b[i] === 0 && b[i + 1] < 0x80) i++;
    b = b.slice(i);
    if (b[0] & 0x80) b = concat(Uint8Array.of(0), b);
    return concat(Uint8Array.of(0x02, b.length), b);
  };
  const body = concat(int(sig.r), int(sig.s));
  return concat(Uint8Array.of(0x30, body.length), body);
}

// parseTx reads a transaction, with or without witnesses: its outputs and
// the bytes its id is the hash of (witnesses left out).
function parseTx(raw) {
  let i = 4;
  const view = new DataView(raw.buffer, raw.byteOffset, raw.byteLength);
  const need = (n) => { if (i + n > raw.length) throw new Error('truncated transaction'); };
  const readVarint = () => {
    need(1);
    const b = raw[i++];
    if (b < 0xfd) return b;
    if (b === 0xfd) { need(2); const v = view.getUint16(i, true); i += 2; return v; }
    if (b === 0xfe) { need(4); const v = view.getUint32(i, true); i += 4; return v; }
    throw new Error('transaction too large');
  };
  need(2);
  const segwit = raw[4] === 0x00 && raw[5] === 0x01;
  if (segwit) i += 2;
  const bodyStart = i;
  const inputs = readVarint();
  for (let n = 0; n < inputs; n++) {
    need(36); i += 36;
    const len = readVarint();
    need(len + 4); i += len + 4;
  }
  const count = readVarint();
  const outputs = [];
  for (let n = 0; n < count; n++) {
    need(8);
    const value = view.getBigUint64(i, true); i += 8;
    const len = readVarint();
    need(len);
    outputs.push({ value, script: raw.slice(i, i + len) });
    i += len;
  }
  const bodyEnd = i;
  if (segwit) {
    for (let n = 0; n < inputs; n++) {
      const items = readVarint();
      for (let k = 0; k < items; k++) { const len = readVarint(); need(len); i += len; }
    }
  }
  need(4);
  if (i + 4 !== raw.length) throw new Error('trailing bytes after the transaction');
  const stripped = segwit ? concat(raw.slice(0, 4), raw.slice(bodyStart, bodyEnd), raw.slice(i)) : raw;
  return { outputs, stripped };
}

// txid is a transaction's id: the double SHA-256 of it without witnesses,
// byte-reversed.
export const txid = (raw) => hex(dsha(parseTx(raw).stripped).reverse());

function opReturn(data) {
  return { value: 0n, script: concat(Uint8Array.of(0x6a), pushData(data)) };
}

// verifiedInput checks a UTXO the server listed against the transaction that
// created it, fetched with getRawTx and matched to its id, and returns the
// value from those bytes. A SegWit signature commits to the value it spends,
// so a wrong one would only make the payment invalid; checking first gives
// a clear error instead, and confirms the output is the wallet's.
async function verifiedInput(u, getRawTx, fromScript) {
  const raw = unhex(await getRawTx(u.txid));
  if (txid(raw) !== u.txid) throw new Error(`the server sent the wrong transaction for ${u.txid}`);
  const out = parseTx(raw).outputs[u.vout];
  if (!out) throw new Error(`transaction ${u.txid} has no output ${u.vout}`);
  if (hex(out.script) !== hex(fromScript)) throw new Error(`output ${u.txid}:${u.vout} is not yours`);
  return { txid: u.txid, vout: u.vout, value: out.value };
}

// vsize is the virtual size of a transaction spending n P2WPKH inputs to
// outputs, once signed; signatures are taken at their largest (73 bytes).
function vsize(n, outputs) {
  const base = 4 + 1 + 41 * n + varint(outputs.length).length +
    outputs.reduce((s, o) => s + 8 + varint(o.script.length).length + o.script.length, 0) + 4;
  const witness = 2 + n * (1 + 1 + 73 + 1 + 33);
  return BigInt(Math.ceil((base * 4 + witness) / 4));
}

// planPayment chooses which of key's P2WPKH outputs (from the API's utxo
// list, each checked with getRawTx) pay amount to script, with an optional
// OP_RETURN, at feeRate sat/vB, and returns the unsigned transaction: its
// inputs, outputs, the total of the inputs, the fee, and the unsigned bytes
// to review.
export async function planPayment({ key, utxos, getRawTx, script, amount, data, feeRate = BTC_FEE_RATE }) {
  if (amount < DUST) throw new Error(`the smallest payment is ${formatBTC(DUST)} BTC`);
  feeRate = BigInt(feeRate);
  const from = keyDestination(key);
  const fromScript = pkScript(from);
  const spendable = utxos
    .filter((u) => u.confirmations > 0 && u.script === hex(fromScript))
    .map((u) => ({ ...u, value: BigInt(u.value) }))
    .sort((a, b) => (b.value > a.value ? 1 : -1));

  const outputs = [{ value: amount, script }];
  if (data) outputs.push(opReturn(data));
  const change = { value: 0n, script: fromScript };

  let inputs = [];
  let total = 0n;
  let fee = 0n;
  for (const u of spendable) {
    const input = await verifiedInput(u, getRawTx, fromScript);
    inputs.push(input);
    total += input.value;
    fee = vsize(inputs.length, [...outputs, change]) * feeRate;
    if (total >= amount + fee) break;
  }
  if (total < amount + fee) {
    throw new Error(`not enough confirmed BTC: have ${formatBTC(total)}, need ${formatBTC(amount + fee)} including the fee`);
  }
  change.value = total - amount - fee;
  if (change.value >= DUST) outputs.push(change);
  else fee += change.value;
  if (fee > MAX_FEE) throw new Error(`the fee would be ${formatBTC(fee)} BTC; refusing to sign`);

  const tx = {
    version: 2,
    inputs: inputs.map((u) => ({ txid: u.txid, vout: u.vout, value: u.value, sequence: SEQUENCE, witness: [] })),
    outputs,
  };
  return { tx, inputTotal: total, fee, unsignedHex: hex(serialize(tx)) };
}

// signPlan signs a planned payment, and checks the signed transaction pays
// exactly the outputs that were reviewed. It returns the signed hex, its
// txid and the fee.
export async function signPlan(plan, key) {
  const from = keyDestination(key);
  const tx = { ...plan.tx, inputs: plan.tx.inputs.map((i) => ({ ...i })) };
  const pub = secp.getPublicKey(key, true);
  for (let i = 0; i < tx.inputs.length; i++) {
    const sig = await secp.signAsync(sighash(tx, i, from.hash, tx.inputs[i].value), key, { lowS: true });
    tx.inputs[i].witness = [concat(derSignature(sig), Uint8Array.of(1)), pub];
  }
  const raw = serialize(tx);
  const outs = (bytes) => JSON.stringify(parseTx(bytes).outputs.map((o) => [String(o.value), hex(o.script)]));
  if (outs(raw) !== outs(unhex(plan.unsignedHex))) throw new Error('the signed transaction differs from the one reviewed; nothing was sent');
  return { hex: hex(raw), txid: txid(raw), fee: plan.fee };
}

// buildPayment plans and signs a payment in one step.
export async function buildPayment(args) {
  return signPlan(await planPayment(args), args.key);
}

// reviewOutputs decodes a transaction's outputs from its bytes, for showing
// before it is signed: each is {value, script, address} for a standard
// address, {value, script, data} for OP_RETURN, or {value, script} otherwise.
export function reviewOutputs(txHex, versions) {
  return parseTx(unhex(txHex)).outputs.map((o) => {
    const s = o.script;
    const dest = scriptDestination(s);
    if (dest) return { ...o, address: encodeAddress(dest, versions) };
    if (s[0] === 0x6a && s.length >= 2 && s[1] < 0x4c && s.length === 2 + s[1]) return { ...o, data: s.slice(2) };
    return o;
  });
}

const PEG_OUT_TAG = new TextEncoder().encode('BVMO');

// pegOutDestination reads the Bitcoin address from a BVMO tag, or returns
// null if data isn't one.
export function pegOutDestination(data, btcVersions) {
  if (data.length < 5 || PEG_OUT_TAG.some((b, i) => data[i] !== b)) return null;
  const kind = data[4];
  if (kind > P2TR || data.length !== 5 + PROGRAM_SIZE[kind]) return null;
  return encodeAddress({ kind, hash: data.slice(5) }, btcVersions);
}

// pegOutData is the BVMO tag naming the Bitcoin address to pay.
export function pegOutData(btcDest) {
  return concat(PEG_OUT_TAG, destBytes(btcDest));
}
