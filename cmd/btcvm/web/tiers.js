// How many Bitcoin confirmations a deposit needs before it's credited.
// Smaller deposits need fewer (the bridge's confirmation tiers); larger ones
// need the full count. Shared by the wallet and the docs.

const satoshis = (btc) => {
  const [whole, frac = ''] = String(btc).split('.');
  return BigInt(whole) * 100000000n + BigInt((frac + '00000000').slice(0, 8));
};
const plain = (btc) => String(btc).replace(/\.?0+$/, '');
const plural = (n) => `${n} confirmation${n === 1 ? '' : 's'}`;

// confirmationsFor is the count for a deposit of value satoshis (a BigInt).
export function confirmationsFor(info, value) {
  for (const t of info.confirmationTiers || []) {
    if (value <= satoshis(t.upTo)) return t.confirmations;
  }
  return info.depositConfirmations;
}

// describeTiers says how long deposits wait, for example "2 confirmations
// for up to 0.001 BTC, 3 for up to 0.005 BTC, and 6 for anything larger".
export function describeTiers(info) {
  const tiers = info.confirmationTiers || [];
  if (tiers.length === 0) return plural(info.depositConfirmations);
  const parts = tiers.map((t, i) => `${i === 0 ? plural(t.confirmations) : t.confirmations} for up to ${plain(t.upTo)} BTC`);
  return `${parts.join(', ')}, and ${info.depositConfirmations} for anything larger`;
}
