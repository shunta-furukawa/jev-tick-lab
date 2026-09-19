// Package exec is the fill simulator (phase 4) and, eventually, live order
// placement (phase 5).
//
// It consumes decide.Signal and market data, never jev.Answer: keeping the
// model output behind the decision layer is what makes the gating logic
// testable in isolation.
//
// The three requirements this package was written against, and where each one
// lives now:
//
//   - Fees are configuration, not constants — see Fees. The bitbank maker
//     rebate is a campaign and can end, and a simulator with it welded in
//     keeps reporting a profitable strategy long after it stops existing.
//   - A maker fill is never assumed from the price touching the limit. See
//     Simulator.OnPrints: the order joins the back of the queue at its level
//     and advances only as real prints consume the size ahead of it.
//   - Adverse selection is not bolted on, it falls out. A resting buy is
//     filled by sell prints, which is to say in exactly the moments the market
//     is moving against it.
//
// The pieces: Simulator answers "would this order have filled, and at what
// price". Ledger answers "what did that do to the account". Trader runs both
// down a maker and a taker path at once, because running one and quoting the
// other's fees is how a paper result becomes meaningless.
//
// It is deliberately not a backtester (CLAUDE.md rule 1). It is driven forward
// by live data, and it could not be replayed over the tick log even if that
// were allowed: the prints that fill a passive order are not in it.
package exec
