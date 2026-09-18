# bitbank public stream — verification record

CLAUDE.md TODO 1. The stream contract in `internal/stream` and
`internal/marketstate` was originally written from secondary sources. This is
the record of checking it against the real thing.

**Verified 2026-09-17 (UTC)** against `wss://stream.bitbank.cc/socket.io/?EIO=4&transport=websocket`
and against <https://github.com/bitbankinc/bitbank-api-docs/blob/master/public-stream.md>.

Reproduce with:

```bash
go run ./cmd/dump -pair xrp_jpy -for 20s              # everything, truncated
go run ./cmd/dump -pair xrp_jpy -rooms depth_whole -truncate 0
```

`cmd/dump` deliberately does not import `internal/stream`: it prints the wire,
not our interpretation of it.

## Handshake — as assumed ✅

```
<< 0{"sid":"xVoRbyKgp6BPQCDoFOF_","upgrades":[],"pingInterval":25000,"pingTimeout":20000,"maxPayload":1000000}
>> 40
<< 40{"sid":"S76l0p1mfYj7zPdGFOGA"}
>> 42["join-room","ticker_xrp_jpy"]
<< 42["message",{"room_name":"ticker_xrp_jpy","message":{"data":{...}}}]
```

`pingInterval` is 25s and `pingTimeout` 20s; the client now reads those out of
the OPEN packet and sets a read deadline of their sum, so a silently dropped
connection reconnects instead of blocking forever.

## Room names — as assumed ✅

`ticker_{pair}`, `transactions_{pair}`, `depth_whole_{pair}`,
`depth_diff_{pair}`, and `circuit_break_info_{pair}` all accepted the join and
delivered payloads.

## Payload shapes

| room | fields observed live | matches skeleton? |
|---|---|---|
| `ticker` | `sell` `buy` `open` `high` `low` `last` `vol` `timestamp` (all strings except `timestamp`) | added — was not subscribed before |
| `transactions` | `transactions[]` of `transaction_id` (number), `side`, `price`, `amount`, `executed_at` (millis) | ✅ |
| `depth_whole` | `asks` `bids` `asks_over` `bids_under` `asks_under` `bids_over` `ask_market` `bid_market` `timestamp` `sequenceId` | ✅ plus sequencing, see below |
| `depth_diff` | `a` `b` `t` `s`, optional `ao` `bu` `au` `bo` `am` `bm` | ✅ |
| `circuit_break_info` | `mode` `fee_type` `estimated_itayose_*` `*_trigger_price` `reopen_timestamp` | added |

Captured examples:

```json
{"room_name":"depth_diff_xrp_jpy","message":{"data":{
  "a":[["202.216","0"],["202.268","4653.8133"]],
  "b":[["202.061","699.9691"]],
  "t":1789689213561,"s":"34337088955",
  "ao":"9744491.602","bu":"20897801.3809"}}}

{"room_name":"depth_whole_xrp_jpy","message":{"data":{
  "asks":[["202.277","160.6057"], ...201 levels],
  "bids":[["202.276","24144.4465"], ...205 levels],
  "asks_over":"9745001.2275","bids_under":"20895629.5121",
  "asks_under":"0","bids_over":"0","ask_market":"0","bid_market":"0",
  "timestamp":1789689280687,"sequenceId":"34337099791"}}}

{"room_name":"transactions_btc_jpy","message":{"data":{"transactions":[
  {"transaction_id":1237470332,"side":"sell","price":"11921596",
   "amount":"0.0029","executed_at":1789689416710}]}}}

{"room_name":"circuit_break_info_xrp_jpy","message":{"data":{
  "mode":"NONE","estimated_itayose_price":null,"upper_trigger_price":"242.678",
  "lower_trigger_price":"161.784","fee_type":"NORMAL","reopen_timestamp":null}}}
```

## What the verification changed

1. **`sequenceId` is a string on the wire**, although the docs table calls it a
   number (`"sequenceId":"34337099791"`). `marketstate.seqID` accepts both
   forms rather than trusting either.

2. **Depth sequencing was wrong, not just missing.** The documented algorithm is
   not gap detection — sequence ids rise monotonically but are explicitly *not
   consecutive*, so arithmetic cannot detect a gap. The exchange's rule is:
   buffer diffs, and on each `depth_whole` replace the book and replay only the
   buffered diffs whose `s` exceeds its `sequenceId`. The skeleton applied diffs
   blindly and, worse, served a book built from diffs alone before the first
   whole ever arrived — a book missing every level that had not happened to
   change, reporting a confidently wrong best bid and ask. `Snapshot` now
   reports `BookSynced=false` until a whole has seeded it, and the bot treats
   that as warmup.

   Wholes arrive every few seconds (observed 23:53:32.284 and 23:53:35.974), so
   the unsynced window at connect time is short.

3. **Trades are sparse, and that broke every time-labelled number.** Over 30s:
   `btc_jpy` produced one transactions frame, `xrp_jpy` produced none. The
   skeleton appended a bar per *print*, so "the last 60 bars" could span many
   minutes while the state text called it 60 seconds, and "Return 60s" was
   really "return over the last 60 trades". The bar series is now continuous in
   wall-clock seconds: a second with no print carries the previous close with
   zero volume and `Traded=false`. The state text reports how many seconds in
   the window had no print, so a quiet market looks quiet rather than flat.

4. **The ticker room is now subscribed.** It is the only room that reports a
   price on a pair that is not trading, so it seeds `Last` at startup — without
   it the bot idled through warmup waiting for a print that may be minutes away.

5. **`circuit_break_info` is now subscribed.** `mode != "NONE"` means the
   exchange itself says the market is not trading normally. That is a fact off
   the wire, so it is a hard brake in `decide`, not a question for the model.

## Not consumed

`asks_over` / `bids_under` / `ask_market` / `bid_market` (aggregate size outside
the 200 published levels) are ignored: the depth figures in the state text are
top-10 only. `au` / `bo` / `am` / `bm` on diffs likewise.
