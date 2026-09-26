// Checks the web wallet's amount units (TestWebUnits): BTC, sats and USD
// show and read back as exact satoshis, and dollars need a fresh price.
// Prints "ok".
import assert from 'node:assert/strict';
import * as units from '../web/units.js';

const now = 1_800_000_000_000;
const price = { usd: 64_200, time: now - 60_000 }; // $64,200 a BTC, a minute old
const stale = { usd: 64_200, time: now - units.PRICE_MAX_AGE - 1 };

// Showing.
assert.equal(units.format(10_000n, 'btc'), '0.0001 BTC');
assert.equal(units.format(1n, 'sats'), '1 sat');
assert.equal(units.format(0n, 'sats'), '0 sats');
assert.equal(units.format(10_000n, 'sats'), '10,000 sats');
assert.equal(units.format(2_100_000_000_000_000n, 'sats'), '2,100,000,000,000,000 sats');
assert.equal(units.format(10_000n, 'usd', price, now), '$6.42');
assert.equal(units.format(100_000_000n, 'usd', price, now), '$64,200.00');
assert.equal(units.format(1n, 'usd', price, now), '< $0.01');
assert.equal(units.format(0n, 'usd', price, now), '$0.00');
// Without a fresh price, USD shows BTC rather than an old figure.
assert.equal(units.format(10_000n, 'usd', stale, now), '0.0001 BTC');
assert.equal(units.format(10_000n, 'usd', null, now), '0.0001 BTC');
assert.deepEqual(units.parts(12_345n, 'sats'), { value: '12,345', unit: 'sats' });

// Reading.
assert.equal(units.parse('0.0001', 'btc'), 10_000n);
assert.equal(units.parse('0.00000001', 'btc'), 1n);
assert.throws(() => units.parse('0.000000001', 'btc'));
assert.equal(units.parse('10,000', 'sats'), 10_000n);
assert.equal(units.parse('1 sat', 'sats'), 1n);
assert.equal(units.parse(' 546 ', 'sats'), 546n);
assert.throws(() => units.parse('1.5', 'sats'), /whole numbers/);
assert.throws(() => units.parse('-5', 'sats'));
assert.throws(() => units.parse('', 'sats'));
assert.throws(() => units.parse('2100000000000001', 'sats'), /21 million/);
assert.equal(units.parse('6.42', 'usd', price, now), 10_000n);
assert.equal(units.parse('$6.42', 'usd', price, now), 10_000n);
assert.equal(units.parse('64,200', 'usd', price, now), 100_000_000n);
// $1 at $64,200 is 1557.63… sats: it rounds to the nearest whole sat.
assert.equal(units.parse('1', 'usd', price, now), 1558n);
assert.throws(() => units.parse('6.425', 'usd', price, now));
assert.throws(() => units.parse('6.42', 'usd', stale, now), /price/);
assert.throws(() => units.parse('6.42', 'usd', null, now), /price/);
assert.throws(() => units.parse('6.42', 'usd', { usd: 64_200, time: now + 60_000 }, now), /price/);
assert.throws(() => units.parse('6.42', 'usd', { usd: NaN, time: now }, now), /price/);

// Every amount shown in BTC or sats reads back exactly.
for (const n of [1n, 546n, 10_000n, 123_456_789n, 2_100_000_000_000_000n]) {
  assert.equal(units.parse(units.format(n, 'btc').replace(/[, BTC]/g, ''), 'btc'), n);
  assert.equal(units.parse(units.format(n, 'sats'), 'sats'), n);
}

assert.equal(units.exact(12_345n), '0.00012345 BTC (12,345 sats)');
console.log('ok');
