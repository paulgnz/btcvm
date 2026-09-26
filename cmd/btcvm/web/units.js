// Amounts shown and entered in BTC, sats or US dollars. Amounts are BigInt
// satoshis throughout. A dollar amount is converted at a price passed in
// explicitly, and rounds to whole satoshis; the wallet signs only the
// satoshis, never a dollar figure.
import { SATS, parseBTC, formatBTC } from './chain.js';

export const UNITS = ['btc', 'sats', 'usd'];

// A price older than this is not used, to show or to enter amounts.
export const PRICE_MAX_AGE = 10 * 60 * 1000;

const MAX_SATS = 21_000_000n * SATS;

// freshPrice returns price ({usd, time}: dollars per BTC, and when this page
// fetched it, in ms) if it is recent enough to convert with, or null.
export function freshPrice(price, now = Date.now()) {
  if (!price || !(price.usd > 0) || !Number.isFinite(price.usd)) return null;
  const age = now - price.time;
  return age >= 0 && age <= PRICE_MAX_AGE ? price : null;
}

// The price in cents per BTC, as an integer.
const centsPerBTC = (price) => BigInt(Math.round(price.usd * 100));

// parts splits an amount into its figure and its unit label, for places
// that style the two apart. Without a usable price, USD falls back to BTC.
export function parts(sats, unit, price, now = Date.now()) {
  sats = BigInt(sats);
  if (unit === 'sats') return { value: sats.toLocaleString('en-US'), unit: sats === 1n ? 'sat' : 'sats' };
  const p = unit === 'usd' ? freshPrice(price, now) : null;
  if (!p) return { value: formatBTC(sats), unit: 'BTC' };
  const cents = (sats * centsPerBTC(p) + SATS / 2n) / SATS;
  if (sats > 0n && cents === 0n) return { value: '< $0.01', unit: '' };
  return { value: `$${(cents / 100n).toLocaleString('en-US')}.${(cents % 100n).toString().padStart(2, '0')}`, unit: '' };
}

// format shows an amount in a unit: "0.0001 BTC", "1 sat", "10,000 sats",
// "$6.42".
export function format(sats, unit, price, now = Date.now()) {
  const p = parts(sats, unit, price, now);
  return p.unit ? `${p.value} ${p.unit}` : p.value;
}

// parse reads an amount typed in a unit and returns its exact satoshis.
// sats are whole numbers, BTC has up to 8 decimals, and dollars up to 2;
// dollars need a fresh price and round to the nearest satoshi.
export function parse(text, unit, price, now = Date.now()) {
  const s = String(text).trim();
  let sats;
  if (unit === 'sats') {
    const t = s.replace(/\s*sats?$/i, '').replace(/[,_\s]/g, '');
    if (/^\d*\.\d*$/.test(t) && t !== '.') throw new Error('sats are whole numbers: enter an amount like 10,000');
    if (!/^\d{1,16}$/.test(t)) throw new Error('enter an amount like 10,000');
    sats = BigInt(t);
  } else if (unit === 'usd') {
    const p = freshPrice(price, now);
    if (!p) throw new Error("The BTC price isn't available right now, so amounts can't be entered in USD. Switch to BTC or sats.");
    const m = /^\$?\s*(\d{1,12})(?:\.(\d{1,2}))?$/.exec(s.replace(/,/g, ''));
    if (!m) throw new Error('enter an amount like 6.42');
    const cents = BigInt(m[1]) * 100n + BigInt((m[2] || '').padEnd(2, '0'));
    const per = centsPerBTC(p);
    sats = (cents * SATS + per / 2n) / per;
  } else {
    return parseBTC(s.replace(/\s*btc$/i, ''));
  }
  if (sats > MAX_SATS) throw new Error('more than 21 million BTC');
  return sats;
}

// exact shows an amount in BTC and in sats, for the line under an amount
// field: what is actually sent.
export function exact(sats) {
  return `${formatBTC(sats)} BTC (${format(sats, 'sats')})`;
}
