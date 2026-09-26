// Checks the web wallet's address book (TestWebAddressBook): only addresses
// the forms' own decoder accepts are saved, names are cleaned and capped,
// the book is capped, stored data is re-checked, and lookups find an
// address for its network. Prints "ok".
//
// Test keys are sha256 of public labels: this file holds no secret.
import assert from 'node:assert/strict';
import * as chain from '../web/chain.js';
import * as book from '../web/addressbook.js';
import { sha256 } from '../web/vendor/noble-hashes-1.8.0/sha2.js';

// Mainnet: Bitcoin and BTCVM share address formats. A testnet-style
// network stands in for a network whose addresses differ.
const mainnet = { p2pkh: 0, p2sh: 5, wif: 128, hrp: 'bc' };
const testnet = { p2pkh: 111, p2sh: 196, wif: 239, hrp: 'tb' };
const versions = { btc: mainnet, vm: mainnet };
const decode = (a, n) => chain.encodeAddress(chain.decodeAddress(a, versions[n]), versions[n]);

const dest = (label) => chain.keyDestination(sha256(new TextEncoder().encode(label)));
const legacy = chain.encodeAddress({ kind: chain.P2PKH, hash: dest("btcvm book mum").hash }, mainnet); // 1…
const segwit = chain.encodeAddress({ kind: chain.P2WPKH, hash: dest('btcvm book kraken').hash }, mainnet); // bc1q…
const onTestnet = chain.encodeAddress(dest('btcvm book testnet'), testnet);
assert.match(legacy, /^1/);
assert.match(segwit, /^bc1q/);

// Adding.
let list = [];
list = book.add(list, { name: '  Mum ', address: legacy, network: 'btc' }, decode);
assert.deepEqual(list, [{ name: 'Mum', address: legacy, network: 'btc' }]);
// Saved in canonical form: an upper-case bech32 address reads back lower.
list = book.add(list, { name: 'Kraken deposit', address: ` ${segwit.toUpperCase()} `, network: 'btc' }, decode);
assert.equal(list[1].address, segwit);
// The same address may be saved for the other network, not twice for one.
list = book.add(list, { name: 'Mum on BTCVM', address: legacy, network: 'vm' }, decode);
assert.equal(list.length, 3);
assert.throws(() => book.add(list, { name: 'Again', address: legacy, network: 'btc' }, decode), /Already saved as "Mum"/);

// Never an invalid address.
const typo = legacy.slice(0, -1) + (legacy.endsWith('a') ? 'b' : 'a');
assert.throws(() => book.add(list, { name: 'Typo', address: typo, network: 'btc' }, decode), /Not a valid Bitcoin address/);
assert.throws(() => book.add(list, { name: 'Testnet', address: onTestnet, network: 'btc' }, decode), /different network/);
assert.throws(() => book.add(list, { name: 'Empty', address: '  ', network: 'btc' }, decode), /Enter an address/);
assert.throws(() => book.add(list, { name: 'Nowhere', address: segwit, network: 'eth' }, decode), /Bitcoin or BTCVM/);
assert.throws(() => book.add(list, { name: 'Proto', address: segwit, network: '__proto__' }, decode), /Bitcoin or BTCVM/);
assert.equal(list.length, 3);

// Names: cleaned, required, capped at 60 characters (not UTF-16 units).
assert.equal(book.cleanName(' Kraken \n\t deposit '), 'Kraken deposit');
assert.equal(book.cleanName('Mu\u202Em\u200B'), 'Mum'); // bidi override and zero-width removed
assert.throws(() => book.cleanName('   '), /name/);
assert.throws(() => book.cleanName('\u200B\u202E'), /name/);
assert.equal(book.cleanName('x'.repeat(60)), 'x'.repeat(60));
assert.throws(() => book.cleanName('x'.repeat(61)), /at most 60/);
assert.equal(book.cleanName('😀'.repeat(60)), '😀'.repeat(60));
assert.throws(() => book.add(list, { name: 'y'.repeat(61), address: segwit, network: 'vm' }, decode), /at most 60/);
// A name is plain text: markup is kept as typed, for textContent to show.
assert.equal(book.cleanName('<img src=x onerror=alert(1)>'), '<img src=x onerror=alert(1)>');

// Renaming and deleting.
list = book.rename(list, legacy, 'btc', '  Mum (savings) ');
assert.equal(book.find(list, legacy, 'btc').name, 'Mum (savings)');
assert.equal(book.find(list, legacy, 'vm').name, 'Mum on BTCVM');
assert.throws(() => book.rename(list, legacy, 'btc', ''), /name/);
assert.throws(() => book.rename(list, segwit, 'vm', 'Nope'), /no longer/);
list = book.remove(list, legacy, 'vm');
assert.equal(list.length, 2);

// Lookup: the entry for the network, else one for the other network.
assert.equal(book.find(list, segwit, 'btc').name, 'Kraken deposit');
const other = book.find(list, segwit, 'vm');
assert.equal(other.network, 'btc'); // the caller says it was saved for Bitcoin
assert.equal(book.find(list, chain.encodeAddress(dest('btcvm book stranger'), mainnet), 'btc'), null);

// Matching what is typed: by name or address, ignoring case; one network
// only, or the preferred network first.
list = book.add(list, { name: 'Alice', address: segwit, network: 'vm' }, decode);
assert.deepEqual(book.matches(list, 'KRAK').map((e) => e.name), ['Kraken deposit']);
assert.deepEqual(book.matches(list, segwit.slice(4, 14)).map((e) => e.name), ['Alice', 'Kraken deposit']);
assert.deepEqual(book.matches(list, '', { only: 'btc' }).map((e) => e.name), ['Kraken deposit', 'Mum (savings)']);
assert.deepEqual(book.matches(list, '', { prefer: 'btc' }).map((e) => e.name), ['Kraken deposit', 'Mum (savings)', 'Alice']);
assert.deepEqual(book.matches(list, 'nobody'), []);
assert.equal(book.shorten(segwit), `${segwit.slice(0, 10)}…${segwit.slice(-8)}`);

// The cap.
let full = [];
for (let i = 0; full.length < book.MAX_ENTRIES; i++) {
  full = book.add(full, { name: `n${i}`, address: chain.encodeAddress(dest(`btcvm book ${i}`), mainnet), network: 'btc' }, decode);
}
assert.throws(() => book.add(full, { name: 'one more', address: segwit, network: 'btc' }, decode), /full/);

// Stored data round-trips, and is re-checked as it loads: bad entries,
// duplicates and anything past the cap are dropped.
assert.deepEqual(book.parse(book.serialize(list), decode), list);
const stored = JSON.stringify([
  { name: 'Good', address: legacy, network: 'btc' },
  { name: 'Dup', address: legacy, network: 'btc' },
  { name: 'Bad address', address: typo, network: 'btc' },
  { name: '', address: segwit, network: 'btc' },
  { name: 'Bad network', address: segwit, network: 'doge' },
  { name: 'x'.repeat(61), address: segwit, network: 'vm' },
  { name: 'Extra fields dropped', address: segwit, network: 'btc', html: '<b>' },
  null, 7, 'text',
]);
assert.deepEqual(book.parse(stored, decode), [
  { name: 'Good', address: legacy, network: 'btc' },
  { name: 'Extra fields dropped', address: segwit, network: 'btc' },
]);
assert.deepEqual(book.parse(null, decode), []);
assert.deepEqual(book.parse('not json', decode), []);
assert.deepEqual(book.parse('{"a":1}', decode), []);
assert.equal(book.parse(JSON.stringify([...full, { name: 'over', address: segwit, network: 'vm' }]), decode).length, book.MAX_ENTRIES);

console.log('ok');
