// Package exec will hold the fill simulator (phase 4) and, eventually, live
// order placement (phase 5).
//
// It consumes decide.Signal and nothing else. It must never read jev.Answer
// directly: keeping the model output behind the decision layer is what makes
// the gating logic testable in isolation.
//
// Fill simulation requirements, when it is written:
//
//   - Maker and taker fees must come from configuration, not constants. The
//     bitbank maker rebate is a campaign and can end.
//   - A maker fill must not be assumed just because the price traded through
//     the limit. Model queue position, or at minimum require the price to
//     trade strictly past the order before filling it.
//   - Adverse selection is the real cost of a maker strategy: passive orders
//     fill disproportionately when the market is moving against them. A
//     simulator that ignores this will produce optimistic and useless results.
package exec
