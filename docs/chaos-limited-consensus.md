# Chaos Limited Consensus (CLC)

Chaos Limited Consensus is the name for the SAN Network consensus mechanism:
a height follows the deterministic proposer when it is online, but if that
proposer is late or offline the height is **not** allowed to stall forever.
Instead it falls back ("limited chaos") through a bounded sequence of
successor proposers - one per elapsed timeout, up to a hard cap - and the
first valid block that reaches a controller quorum is committed. Safety is
then layered on top by stake-weighted 2/3 finality, which turns a
probabilistic longest-chain rule into an explicit checkpoint.

This document describes what the implementation actually does. It is based on
the Go node (`internal/netnode/node_consensus.go`, `node.go`, `node_state.go`,
`node_fork.go`, `internal/ledger/*`) and the Python reference
(`network/Node.py`, `blockchain/Blockchain.py`, `blockchain/economics.py`).
Where Go and Python differ, this is called out in
[Status / limitations](#status--limitations).

Contents:

1. [Roles and eligibility](#1-roles-and-eligibility)
2. [Proposer selection and rounds](#2-proposer-selection-and-rounds)
3. [Block validity rules](#3-block-validity-rules)
4. [Finality](#4-finality)
5. [Governance](#5-governance)
6. [Fork choice and reorgs](#6-fork-choice-and-reorgs)
7. [Determinism](#7-determinism)
8. [Consensus parameter table](#8-consensus-parameter-table)
9. [Threat model and limits](#9-threat-model-and-limits)
10. [Status / limitations](#status--limitations)

---

## 1. Roles and eligibility

Four roles exist in the implementation: **validators**, the **proposer**,
**controllers**, and ordinary (funded) nodes.

### 1.1 Validators

A validator is an account that has bonded stake with a signed `deposit`
transaction. State lives in `Blockchain.Validators` (Go:
`internal/ledger/blockchain.go:83`; Python: `blockchain/Blockchain.py:70`):

| Field | Meaning |
|-------|---------|
| `public_key` | pubkey that signed the deposit (address is derived from it) |
| `stake` | bonded base units (1 SAN = 10^8 units) |
| `joined_height` | first height the validator is eligible from |
| `release_height` | `null` while bonded; height after which `withdraw` is allowed |

A validator becomes active once `joined_height <= tip` and is counted while
`stake >= min_validator_stake`:

* `active_validators` (used for proposal and for a node's own vote duty):
  excludes any validator whose `release_height != nil`.
* `active_validators_for(height)`: uses the cutoff `height - 1` and
  **ignores release timing** on purpose ("votes are tallied within a block or
  two of the target height", `blockchain/Blockchain.py:147`). This is the set
  frozen at commit time to weight votes for that height.

A validator leaves with `undelegate`:
`release_height = block_index + unbonding_period` (default 100). It then stops
being proposer/voter eligible but its stake stays locked until `withdraw`
succeeds at `block_index >= release_height`. `evidence` removes a validator
immediately (see [finality](#4-finality)).

### 1.2 Proposer

The proposer is derived deterministically from the chain id, the height and
the round ("round 0" is the base proposer). It must sign the block with the
key whose address is expected. See [section 2](#2-proposer-selection-and-rounds).

### 1.3 Controllers (pre-commit quorum)

Before a locally produced block is committed and gossiped, the producing node
asks its **controller set** for signed votes
(`sendToControllers`, `internal/netnode/node_sync.go:513`;
`Node.send_to_controllers`, `network/Node.py:2439`):

* Each controller runs the full `verify_block` and answers with a signed
  `BLOCK_VOTE_RESPONSE` (over chain id, approved flag and block hash).
* Approval requires `approvals / len(controllers) >= 0.66`, i.e. at least 66%
  of the set asked.
* Records are deduplicated before asking: a repeated record, or several
  addresses advertising the same `public_key`, counts once, so one signing key
  cannot inflate the quorum.
* Stale records are dropped from the selection: a peer record whose signed
  timestamp is older than `SAN_PEER_TTL` (default 300 s) no longer counts as a
  controller, so a node that stopped re-announcing ages out of the pre-commit
  set instead of blocking it indefinitely.
* With an empty controller set the block is accepted locally (bootstrap /
  single-node mode). `SAN_PUBLIC_DEVNET=1` makes this a startup concern: the
  target must be at least `SAN_CONTROLLER_MIN_COUNT` (default
  `3`), a peer source must be configured, and a node that still has no
  effective controllers logs a clear startup `WARNING`. Prometheus exposes
  `san_controllers` (effective) next to `san_controllers_target` (configured
  target).

Controller selection (`selectControllers`,
`internal/netnode/node_peers.go:233`; `Node._select_controllers`,
`network/Node.py:766`):

* Eligibility: the peer record must carry a `public_key`; if
  `SAN_CONTROLLER_MIN_STAKE > 0`, the peer address's current balance must be
  `>= controller_min_stake` (liquid balance, not bonded stake).
* Epoch: `epoch = tip_index / max(epoch_length, 1)`; `epoch_length` default
  100.
* Score: `sha256("{epoch}:{public_key}")`, ranked ascending; the first
  `controller_count` (default 10) peers are selected.
* The set is recomputed on `refreshPeerSelection()`, which runs after peer
  add/remove, health checks and bootstrap.

Controllers are a **local view**: each node selects from its own peer table.
Controller quorum is an availability/pre-commit gate, **not finality**: a block
that reaches controller quorum is still only *committed locally* and can be
replaced by a longer valid branch; the safety proof is stake-weighted 2/3
finality (section 4). Do not treat a controller approval as irreversible. This
is discussed in [threat model](#9-threat-model-and-limits).

### 1.4 Bootstrap mode

When there are no eligible validators, `isExpectedProposer` returns true for
everyone (any node may propose) and no finality votes are collected
(`commitBlock` only queues a vote when the active set is non-empty). Chains
run without finality until validators stake.

---

## 2. Proposer selection and rounds

This is the core of "limited chaos". The rules are identical in Go and
Python.

### 2.1 The base proposer

`expectedProposer(height, round)` (`internal/netnode/node_consensus.go:19`;
`Node.expected_proposer`, `network/Node.py:1203`):

```
ranked  = sorted(active_validator_addresses)          # lexicographic
seed    = sha256(f"{chain_id}:{height}")              # bytes
base    = big_endian_uint64(seed[0:8]) % len(ranked)
index   = (base + round) % len(ranked)
proposer = ranked[index]
```

Every node with the same chain id, height and active set computes the same
`ranked` list and therefore the same proposer for a given round. Rounds
rotate through the list in order and wrap around.

### 2.2 Round clock

Each node keeps local per-height round state
(`{height, round, started}`; Go `proposerRoundState`, `internal/netnode/node.go:65`).
`currentRoundLocked` (`internal/netnode/node_consensus.go:49`;
`Node._current_round`, `network/Node.py:1221`) computes:

```
timeout   = max(proposer_timeout_ms / 1000, 0.1)          # default 6.0 s
capRounds = max(max_proposer_rounds, len(active), 1)      # default >= 16
advance   = floor((now - started) / timeout)
round     = min(advance, capRounds - 1)
```

The round only ever moves forward for a height, and the state is reset when a
new height is proposed (`proposerState` is cleared once `Height <= tip`).
`max_proposer_rounds` is genesis/proposal parameter
`SAN_MAX_PROPOSER_ROUNDS` (default 16). The effective bound is the larger of
that value and the number of active validators, so a large validator set
still gets one full rotation; if there is only one validator the bound is
still at least 16.

### 2.3 A block must claim a justified round

`verify_block` rejects a block whose `round` is not justified by its own
timestamp (`internal/netnode/node_state.go:419`;
`network/Node.py:1161`):

* `round < 0 || round >= capRounds` -> reject;
* `round > 0` requires
  `timestamp >= tip.timestamp + round * proposer_timeout * 0.8`
  (the `0.8` factor keeps the timestamp rule slightly ahead of the local
  timeout clock so honest nodes agree);
* the block's validator address must equal `expectedProposer(height, round)`;
* the transaction/state rules of section 3 apply unchanged.

Because round justification is a pure function of the previous block's
timestamp and the parameter, **sync, replay and live verification all agree**;
no verifier consults its local round counter when accepting a remote block.
The local counter only decides when *this* node may produce a block.

### 2.4 Falling back to the next proposer

```
height H, active = {A, B, C}, ranked = [A, B, C], base = 0

round 0 (t=0s)         round 1 (t=6s)         round 2 (t=12s)
    proposer A             proposer B             proposer C
       |                      |                      |
   block? no              block? no              block? yes
       |                      |                      |
 validation fails/      B offline/              C builds with round=2,
 A offline              timeout               timestamp >= tip + 2*6*0.8s
                                                    |
                                              controller quorum >= 66%
                                                    |
                                         commit + vote (gossiped at quorum)
```

Notes:

* A node that sees a valid, signed block for round `r > 0` does not need to
  have advanced to `r` itself; the timestamp rule is the only round proof.
* If no round produces a block within `capRounds * timeout`, the height simply
  waits for the next round's proposer to come online (or for a restart of the
  round state after a new tip). CLC bounds how many proposer turns are
  attempted; it cannot manufacture a block without an online proposer.
* Competing round blocks (e.g. proposer at round 1 and round 2 building on
  the same parent) are resolved by the fork-choice rules in section 6; only a
  block that reaches the controller quorum is committed locally in the first
  place.

### 2.5 Producing a block

`maybeProduceFromPool` (`internal/netnode/node_consensus.go:881`;
`Node._maybe_produce_from_pool`, `network/Node.py:2710`) runs from the block
production loop every `clamp(proposer_timeout, 0.5s, 2.0s)` and on every new
mempool transaction:

1. If the mempool is empty: return unless `block_reward > 0` (subsidy chains
   keep producing empty blocks; fee-only chains produce on demand). On a
   subsidy chain an empty block is only produced when the tip is at least
   `proposer_timeout` seconds old.
2. If `require_block_signature` and there is no private key: do nothing.
3. `height = tip + 1`; `round = current_round(height)`; stop unless this node
   is the expected proposer for `(height, round)`.
4. If the mempool is non-empty, stop unless the summed fees reach
   `block_threshold_fee` (`SAN_BLOCK_THRESHOLD_FEE`, default 500 SAN,
   `cmd/sanup` lowers it to `0.0001`).
5. Build the block, ask controllers (it is gossiped as soon as the quorum is
   reached, before the local commit), commit, vote.

`buildBlock` (`node_consensus.go:829`) computes
`minimum_timestamp = tip.timestamp + min_block_interval`, upgrades it to
`tip.timestamp + round * proposer_timeout * 0.8` for `round > 0`, and uses
`max(now, minimum_timestamp)`. It simulates the block on state copies to
predict `state_root`, then signs the header hash.

---

## 3. Block validity rules

`verifyBlock` (`internal/netnode/node_state.go:419`;
`Node.verify_block`, `network/Node.py:1093`) runs on every accepted block
(live, sync replay with `historical=true`, and reorg replay). The same checks
are re-run during execution (`simulateBlock`), so a block cannot be accepted
by the header checks and fail silently during state application.

### 3.1 Header checks

| Check | Rule | Source |
|-------|------|--------|
| chain binding | `block.chain_id == node.chain_id` | `verifyBlock` |
| continuity | `block.index == tip.index + 1` and `previous_block_hash == tip.hash` | `verifyBlock` |
| time lower bound | `timestamp > median_time_past(11)` | `verifyBlock` |
| time upper bound | `timestamp <= now + 120 s` (`BLOCK_FUTURE_DRIFT`) | `node.go:39` |
| live time window | if not historical: `timestamp >= now - 120 s` (`BLOCK_PAST_DRIFT`) | `node.go:40` |
| min interval | if `min_block_interval > 0`: `timestamp >= tip.timestamp + interval` | `verifyBlock` |
| reward address | if set, must normalize to a valid 20-byte address | `verifyBlock` |
| state commitment | if `require_state_root` and `index > 0`: `state_root` must be present | `verifyBlock` |
| round claim | section 2.3 | `verifyBlock` |
| hash | `current_block_hash == sha3_256(canonical(header))` | `Block.CalculateHash` |
| signature | `validator_signature` verifies over the block hash with `validator` pubkey (unless `SAN_REQUIRE_BLOCK_SIGNATURE=false`) | `VerifyBlockSignature`, `node.go:499` |
| proposer | `address(validator) == expectedProposer(index, round)` when the active set is non-empty | `verifyBlock` |
| tx root | recomputed Merkle root of the transactions must equal the announced `tx_root` | `BlockFromDict`, `internal/ledger/block.go:213` |
| state root | recomputed state root must equal the announced `state_root` | `simulateBlock` |

The block hash commits to (`HeaderDict`, `internal/ledger/block.go:121`):
`chain_id`, `index`, `previous_block_hash`, `timestamp`, `validator`,
`transactions`, `tx_root`, `state_root`, `round`, `reward_address`. Schema
version is **5** (`internal/ledger/block.go:20`, `blockchain/Block.py:13`),
and `HELLO`-style handshakes use protocol version **2**.

Timestamps are floats in canonical form (Python `repr` style), so the median
and the round derivation are reproducible across implementations.

### 3.2 Transactions

Each transaction inside a block is re-validated (`simulateBlock`,
`internal/netnode/node_state.go:517`; `Node._simulate_block`,
`network/Node.py:1239`):

* `chain_id` matches, `sender` is a valid public key, `signature` verifies
  over the canonical payload **without** `signature` and `fee`;
* `nonce` matches the sender's current on-chain nonce (strict sequential
  order);
* structural gas rules: transactions without `bytecode`/`contract_code` must
  not set `gas_limit`/`gas_price`; execution transactions require
  `gas_limit >= 1` and `gas_price >= MinGasPrice = 1`;
* validator commands (`deposit`/`undelegate`/`withdraw`/`evidence`) and
  governance commands (`set_param`) must not mix with transfers or execution;
* the announced `fee` equals the deterministic fee for the current fee rate;
* summed `gas_limit` over the block does not exceed `block_gas_limit`;
* execution transactions require `gas_price >= base_fee`;
* transfers: `value > 0`, receiver normalizes; sender balance covers
  `value + fee`;
* execution: run on a state copy; out-of-gas/failed execution rolls back the
  state changes but the escrow (gas_limit * gas_price) is still consumed and
  burned, so the fee formula stays deterministic.

### 3.3 Fee market (EIP-1559 style)

Constants in `internal/ledger/economics.go` (`blockchain/economics.py`):

| Constant | Value | Meaning |
|----------|-------|---------|
| `SANDecimals` / `SANBase` | 8 / 100,000,000 | 1 SAN = 10^8 base units |
| `MinFeePerByte` | 0.01 SAN | size fee floor |
| `MaxFeePerByte` | 0.1 SAN | size fee ceiling |
| `CongestionTxPerStep` / `CongestionMaxSteps` | 100 / 10 | parent-block load steps |
| `MinGasPrice` | 1 unit | minimum `gas_price` |
| `InitialBaseFee` | 1 unit | genesis per-gas base fee |
| `BaseFeeMaxChangeDenom` | 8 | max +/-12.5% change per block |
| `BaseFeeTargetDivisor` | 2 | target gas = `block_gas_limit / 2` |
| `FinalityNumerator` / `FinalityDenominator` | 2 / 3 | minimum 2/3 threshold |
| `MinValidatorStakeUnits` | 1000 SAN | `min_validator_stake` default |
| `DefaultUnbondingPeriod` | 100 | blocks |
| `DefaultSlashBps` | 5000 | 50% of stake burned |

Fee formula (`ExpectedFee`, `internal/ledger/transaction.go:348`):

```
fee       = len(canonical_json(payload without "fee")) * fee_rate
          + gas_limit * gas_price
fee_rate  = min(MinFeePerByte * (1 + min(parent_tx_count / 100, 10)), MaxFeePerByte)
```

So the per-byte rate is `0.01 SAN` rising in `0.01` steps per 100
transactions in the parent block, capped at `0.1 SAN` per byte; a block's
fees are fully determined by on-chain data.

Execution accounting (`simulateBlock`):

```
escrow   = gas_limit * gas_price                  # charged up front
refund   = (gas_limit - gas_used) * gas_price     # back to the sender
burned   += gas_used * base_fee                   # leaves circulation
tip       = fee - refund - burned                 # to the reward address
reward   += tip + block_reward                    # block subsidy
base_fee  = NextBaseFee(base_fee, gas_used, block_gas_limit)
```

The base fee moves toward the target `gas_limit / 2`: up to `1/8` of the
current fee per block, never below `InitialBaseFee = 1`.

### 3.4 Coinbase / reward address

The reward for a block is credited to the address recorded in the signed
header:

* if `reward_address` is present, it is used (validated as an address);
* otherwise the address derived from `validator` (the proposer's public key)
  is used;
* the producer sets it from `SAN_REWARD_ADDRESS`, falling back to its own
  address (`buildBlock` / `validatorRewardAddress`,
  `internal/netnode/node_state.go:1217`);
* if `validator` is empty, no reward is credited at all.

This makes the destination identical on every node: the block signature
commits to both the validator and the reward address.

---

## 4. Finality

Finality is a separate, stake-weighted vote layer on top of controller
quorum. A block is **finalized** once validators controlling at least 2/3 of
the frozen active stake have signed a `FINALITY_VOTE` for it while it is on
the canonical chain.

### 4.1 Vote lifecycle

* `commitBlock` queues `pendingVotes[block.index]` whenever the active set is
  non-empty. The direct production paths (transaction submit, block
  production and incoming gossip) call `maybeVote` right away; the remaining
  paths (sync, orphan connection and block fetch) drain the queue through
  `flushPendingVotes`, which votes for every queued height at or below the
  tip (`maybeVote`, `node_consensus.go:101`).
* A vote is canonical JSON plus a hex signature:
  `{chain_id, public_key, height, block_hash, timestamp, signature}`;
  the signature covers everything except `signature`
  (`votePayload`, `node_state.go:821`).
* `handleFinalityVote` (`node_consensus.go:272`) verifies chain id, size,
  height, hash and signature, then either stages it (if the height has no
  frozen weight set yet) or tallies it against the frozen set.
* Valid commits freeze the voting weights first:
  `finalitySets[height] = active_validators_for(height)`
  (`commitBlock`, `node_state.go:1050`). Staking changes later cannot
  re-weight an old height.
* `rebroadcastOwnVotes` re-sends a node's own not-yet-finalized votes in the
  window `height > max(finalized - 8, tip - 64, 0)` on each health-check
  cycle, so a dropped gossip does not stall finality.

### 4.2 Bounds against vote spam

| Constant | Value | Purpose |
|----------|-------|---------|
| `VOTE_LOOKAHEAD` | 64 | votes for heights more than 64 ahead of the tip are dropped |
| `VoteMaxBytes` | 16,384 | maximum canonical size of a vote |
| staged hashes per height | 8 | maximum competing hashes kept per height |
| `VOTE_LOOKAHEAD * 4` | 256 | maximum staged hashes across all heights |
| `MaxStagedVotersPerHash` | 128 | maximum voters staged per hash before the block arrives |

Votes for unknown heights at or below the tip are dropped; votes within the
lookahead are staged so gossip can overtake the block. Staging also detects
equivocation: the same address voting for two different hashes at the same
height records evidence (see 4.5).

### 4.3 The 2/3 threshold

`tallyFinality` (`node_consensus.go:431`; `Node._tally_finality`,
`network/Node.py:1761`):

```
if voted_stake * 3 < total_stake * 2:  return      # needs >= 2/3
if block_at(height) is missing or its hash != block_hash: return
finalized_height = height
finalized_hash   = block_hash
drop all vote records below height
persist finality checkpoint
optionally prune blocks below height - SAN_PRUNE_KEEP
```

Finality is monotone: `tallyFinality` returns for `height <= finalized_height`
and a persisted checkpoint is only adopted if it is ahead of the in-memory
one.

### 4.4 Persistence and attestations

* `persistFinality` (`node_state.go:344`) writes the checkpoint plus the
  frozen sets and staged votes for the last 256 heights to database metadata
  (`finality_state`, `finalized_height`, `finalized_hash`).
* On restart, `loadFinalityState` restores sets/votes and checks the
  checkpoint against the store's canonical index. Pruning never removes the
  block at the finalized height, so a checkpoint whose block is missing or
  hashes differently is a **fatal startup error** (`refusing to start`)
  instead of a guess; finality is monotone and can never be rolled back.
* `AttestationState` (`node_consensus.go:489`) backs `/validators`
  (finalized height/hash, validator list with stake, total stake,
  `min_stake_units`, `unbonding_period`, `slash_bps`, `base_fee`,
  `total_burned` and the full parameter map). `/finality` returns the chain
  id, tip height/hash, finalized height/hash and the pending vote heights
  (`handleFinality`, `internal/api/server.go:132`).
* `/snapshot` (`FinalizedSnapshot`, `node.go:757`) serves a snapshot only if
  its height is `<= finalized_height` and its hash matches the canonical
  block; a snapshot from a discarded branch is never served.

### 4.5 Equivocation and slashing

When the same validator address is seen voting for two different block hashes
at one height, `recordEvidence` stores
`{offender, height, vote_a, vote_b}` (bounded to 256 entries) and the node
gossips the vote onward. The evidence is also included in a later
`validator: {command: "evidence", vote_a, vote_b}` transaction, which is
validated (`verifyEquivocation`, `node_state.go:825`):

* both votes carry the same public key, same height, different non-empty
  hashes, matching chain id, and both signatures verify;
* the offender must have a validator record (a validator that is unbonding
  still qualifies, because `release_height` is not checked here).

On success (`applyValidatorCommand`, `node_state.go:742`):

```
burned    = stake * slash_bps / 10000        # default 5000 bps = 50%
remaining = stake - burned
balance[offender] += remaining
delete validators[offender]
total_slashed += burned
```

The validator is removed immediately; the unburned part returns to its liquid
balance. `slash_bps` is governable (`0..10000`).

### 4.6 Vote flow

```
commit block H  (proposer commit / BLOCK gossip / Sync / orphan connect)
        |
        v
finality_sets[H] = active_validators_for(H)      # frozen before apply
pendingVotes += H
        |
        +--> maybeVote(H)  (only if this node is an active validator)
        |        | sign {chain_id, public_key, height, block_hash, timestamp}
        |        v
        |   handleFinalityVote(vote) ---------> gossip FINALITY_VOTE to peers
        |        |
        |        +-- no frozen set yet: stage if H <= tip + 64 (caps apply)
        |        +-- frozen set: drop non-voters, stage, detect equivocation
        |        v
        |   tallyFinality(H, hash)
        |        |
        |        +-- voted_stake * 3 >= total_stake * 2 ?
        |        |       yes -> finalized_height = H, persist checkpoint,
        |        |              drop old votes, prune below H - prune_keep
        |        |       no  -> keep collecting
        v
   flushPendingVotes() drains queued heights; rebroadcastOwnVotes()
   re-sends own votes for max(finalized-8, tip-64, 0) < h <= tip
```

---

## 5. Governance

Consensus parameters live in chain state (`Blockchain.Parameters`) and are
changed only by a `set_param` transaction carrying validator approvals.
The parameter table is identical in Go and Python
(`internal/ledger/blockchain.go:25`, `blockchain/Blockchain.py:21`):

| Parameter | Min | Max | Genesis default | Effect |
|-----------|-----|-----|-----------------|--------|
| `min_validator_stake` | 0 | none | 1000 SAN = 100,000,000,000 units | minimum stake for the active set |
| `unbonding_period` | 0 | none | 100 | blocks from `undelegate` to `withdraw` |
| `slash_bps` | 0 | 10,000 | 5000 | share of stake burned on proven equivocation |
| `block_gas_limit` | 100,000 | none | 30,000,000 | maximum total gas per block |
| `proposer_timeout_ms` | 100 | 60,000 | 6,000 | round deadline in ms (consensus state) |
| `block_reward` | 0 | none | 0 (devnet scripts use 2 SAN) | per-block subsidy in base units |
| `min_block_interval_ms` | 0 | 600,000 | 0 | minimum ms between blocks; a subsidy chain always enforces at least 1 s |

Rules (`applyGovernanceCommand`, `internal/netnode/node_state.go:866`;
`Node._apply_governance_command`, `network/Node.py:1915`):

* the transaction may carry exactly one `governance: {command: "set_param",
  name, value, approvals: [...]}` command and must not mix with a transfer,
  a validator command or execution;
* `value` must be an integer inside the governance range above;
* each approval's signature covers the canonical JSON digest
  `{chain_id, command: "set_param", name, value, tx_sender, tx_nonce}` -
  approvals cannot be replayed onto another transaction or chain;
* an approval must come from an address that is in the *currently applied*
  active set (stake `>= min_validator_stake`, `release_height == nil`),
  duplicates are rejected;
* `approved_stake * 3 >= total_active_stake * 2` (same 2/3 rule as finality);
* on success the parameter is written in the same block application, and the
  new value takes effect for subsequent blocks (governance changes a
  parameter that the *next* block's rules read).

Producing an approval in the SDK: `client.governance_approval(name, value,
nonce=...)`, submitted through `governance_set_param` (see `README.md`).

---

## 6. Fork choice and reorgs

Fork handling is in `internal/netnode/node_fork.go` (Python:
`network/Node.py:2202-2460`).

### 6.1 Incoming block paths

`processIncomingBlock` (Go `node_fork.go:92`) classifies a block:

1. Already known (`chainHashes`) -> ignored.
2. `previous_block_hash == tip.hash`: full `verifyBlock` + `commitBlock`;
   then connect buffered orphans.
3. Parent known on-chain or buffered: `bufferForkBlock` (a competing branch).
4. Unknown parent: an orphan. Bounds:
   * `index > tip + max_orphans` -> reject (too far ahead);
   * `index <= tip - max_reorg_depth` -> reject (too far behind);
   * cheap `verifyForkBlock` (hash, block signature, transaction signatures)
     and buffer.

`bufferOrphan` keeps the map at `max_orphans` entries, dropping the lowest
index first. `connectOrphans` repeatedly attaches any buffered block whose
parent becomes the tip.

### 6.2 Longest fully verified chain

`bestOrphanChain` walks each orphan's parent chain back to a known ancestor
and builds the candidate chain. Orphans are visited, connected and evicted in
sorted-hash order and equal-length candidates are broken by the smaller tip
hash, so fork choice is reproducible regardless of Go map iteration order.
`tryReorg` only switches when the candidate is **strictly longer**
(`len(candidate) > len(current)`), i.e. longest-chain with finality as a
floor:

* **finality guard**: if `finalized_height > 0`, a candidate shorter than
  finality or one whose block at the finalized height has a different hash is
  rejected ("finalized history can never be rewritten"). The checkpoint is
  addressed by absolute height, so the guard also works on a pruned window;
* the candidate is replayed: `captureState` snapshots chain, balances, nonces,
  validators, parameters, storage, chain hashes, finality sets/votes and
  receipts; `replayChain` re-runs `verifyBlock` + `simulateBlock` for every
  block from the anchor, forcing historical validation for blocks at or
  below the finality checkpoint;
* on failure the snapshot is restored and the reorg is abandoned;
* on success transactions from blocks that are no longer on the canonical
  chain are pushed back into the mempool by `requeueTransactions`, which
  re-validates them against the new tip (nonce, fee at the new fee rate,
  balance) and then prunes the pool;
* the replaced branch's blocks are retained in the orphan buffer (bounded by
  `max_orphans`), so a later, longer branch descending from them is assembled
  without waiting for a re-fetch;
* `persistFinality` runs, snapshots that do not match the new canonical
  hashes are deleted, and stray orphans are dropped.

### 6.3 Orphan parent fetches

When a block arrives whose parent is unknown, the node asks peers for the
parent with `GET_BLOCK` (`requestBlock`, `node_sync.go:279`):

* in-flight requests are deduplicated in `requestedBlocks` (up to 512, then
  the set is reset);
* the outgoing peer is tried first, then every known peer until one returns
  the block; a failed fetch removes the dedup entry so a later gossip or
  health-check cycle can retry;
* `BLOCK_NOT_FOUND` is a normal answer for a peer that does not have the
  block: Go records it per hash (`BlockMissTTL`, 120 s), moves that peer to
  the end of the candidate list, and tries the others. When **every**
  candidate lacks the block, one chain sync is attempted per
  `SAN_PEER_CHECK_INTERVAL` because the block may live on a longer branch;
  a successful fetch or an incoming gossip of the hash clears the marks.
  (Python's reference treats `BLOCK_NOT_FOUND` as a plain failed fetch and
  asks only its one selected peer; the wire behavior is identical.)
* the peer health loop retries up to 8 missing parents per cycle.

---

## 7. Determinism

Nodes converge because every value that decides validity or selection is
derived from chain data with a canonical encoding.

* **Canonical JSON**: `internal/canonical` reproduces Python
  `json.dumps(payload, sort_keys=True, separators=(",", ":"))` byte for byte,
  including Python's float representation rules and integer-vs-float
  decoding. All hashes, signatures, peer records, votes and persistence
  payloads go through it (`internal/canonical/canonical.go`).
* **Block hash**: SHA3-256 over the canonical header object
  (`chain_id`, `index`, `previous_block_hash`, `timestamp`, `validator`,
  `transactions`, `tx_root`, `state_root`, `round`, `reward_address`).
* **`tx_root`**: Merkle tree over the canonical transaction payloads,
  SHA3-256 leaves, odd nodes promoted (no hash duplication), fixed
  `EmptyRoot` (`internal/ledger/merkle.go`).
* **`state_root`**: Merkle root over ordered state entries:
  `acct:<address>` (balance, nonce, optional validator record),
  `code:<id>` and `store:<id>` per contract, `vmdata`, `vmfuncs`,
  `total_slashed`, `total_burned`, `base_fee`, `parameters`. Addresses and
  contract ids are sorted before hashing.
* **Signatures**: ML-DSA-44 (FIPS 204 / CRYSTALS-Dilithium2) via
  `pqcrypto>=1.0`; addresses are `0x` + `sha3_256(pubkey)[:20]`. The chain id
  is part of every signed message, so signatures cannot move between chains.
* **Replay rules**: sync replays every verified block (never trusting a peer
  snapshot); restart verifies the persisted block window, the genesis
  fingerprint and the persisted state root before serving; a persisted
  chain with a mismatched allocation or state root refuses to start
  (`loadPersistedState`, `internal/netnode/node_state.go:113`).
* **Deterministic inputs**: fee rate comes from the parent block's
  transaction count; the base fee from gas used; the proposer from
  chain id + height + round + sorted active set; finality weights from the
  frozen eligible set at commit; controller selection from the epoch and
  peer public keys.
* **Fork-choice order**: orphan traversal, orphan eviction and equal-length
  tie-breaks are sorted by hash, so a reorg decision never depends on Go map
  iteration order; state entries, validator lists and approvals are ordered
  the same way.

Two honest nodes with the same chain data therefore compute the same hash,
the same state root, the same proposer and the same voting weights, and
accept exactly the same blocks.

---

## 8. Consensus parameter table

Runtime configuration (`internal/netnode/config.go:70`,
`network/config.py:187`); the governance parameters in section 5 are chain
state and can be changed on-chain, the rest are node-local.

| Environment | Field | Default | Governance range |
|-------------|-------|---------|------------------|
| `SAN_CHAIN_ID` | chain id | `san-devnet-1` | not governable |
| `SAN_MIN_VALIDATOR_STAKE` | minimum stake | 1000 SAN | 0 .. none |
| `SAN_UNBONDING_PERIOD` | unbonding blocks | 100 | 0 .. none |
| `SAN_SLASH_BPS` | slash share | 5000 | 0 .. 10000 |
| `SAN_BLOCK_GAS_LIMIT` | block gas limit | 30,000,000 | 100,000 .. none |
| `SAN_PROPOSER_TIMEOUT` | round timeout (s) | 6.0 | maps to `proposer_timeout_ms` 100..60000 |
| `SAN_MAX_PROPOSER_ROUNDS` | fallback round cap | 16 | node-local |
| `SAN_MIN_BLOCK_INTERVAL_MS` | min block interval (ms) | 0 (subsidy chains: min 1000) | 0 .. 600000 |
| `SAN_BLOCK_REWARD` | subsidy per block | 0 (devnet: 2 SAN) | 0 .. none |
| `SAN_BLOCK_THRESHOLD_FEE` | mempool fee that triggers block production | 500 SAN (devnet: 0.0001) | node-local |
| `SAN_CONTROLLER_COUNT` | controllers asked per block | 10 | node-local |
| `SAN_CONTROLLER_MIN_STAKE` | balance needed to be a controller | 0 | node-local |
| `SAN_EPOCH_LENGTH` | controller-selection epoch | 100 blocks | node-local |
| `SAN_MAX_ORPHANS` | buffered fork blocks | 64 | node-local |
| `SAN_MAX_REORG_DEPTH` | max reorg depth | 64 | node-local |
| `SAN_SNAPSHOT_INTERVAL` | state snapshot interval | 1000 blocks | node-local |
| `SAN_PRUNE_KEEP` | blocks kept behind finality | 0 (keep all) | node-local |

Constants compiled into the node:

| Constant | Value | File |
|----------|-------|------|
| `ProtocolVersion` | 3 | `internal/netnode/node.go` |
| `LegacyProtocolVersion` | 2 (explicit legacy mode only) | `internal/netnode/node.go` |
| `SchemaVersion` | 5 | `internal/ledger/block.go:20` |
| `VoteLookahead` | 64 | `internal/netnode/node.go:49` |
| `MaxStagedVotersPerHash` | 128 | `internal/netnode/node.go:50` |
| `VoteMaxBytes` | 16,384 | `internal/netnode/node.go:51` |
| `HelloTTL` | 60.0 s | `internal/netnode/node.go:52` |
| `BlockFutureDrift` / `BlockPastDrift` | 120.0 s / 120.0 s | `internal/netnode/node.go:39` |
| `BlockMissTTL` | 120.0 s | `internal/netnode/node.go:45` |
| `FinalityNumerator` / `FinalityDenominator` | 2 / 3 | `internal/ledger/economics.go:28` |
| `DefaultProposerTimeoutMs` | 6000 | `internal/ledger/blockchain.go:13` |
| `GenesisTimestamp` / `GenesisMessage` | 0.0 / `TEXT A MESSAGE TO THE HUMANITY` | `internal/ledger/blockchain.go:10` |

---

## 9. Threat model and limits

What CLC provides:

* deterministic single-proposer commit with a bounded liveness fallback
  (at most `max(max_proposer_rounds, n_validators)` rounds per height);
* a pre-commit controller quorum as an availability gate;
* stake-weighted 2/3 finality, a monotone checkpoint, and slashing for
  provable double-voting;
* longest-fully-verified-chain reorgs bounded by `max_reorg_depth` and
  blocked from rewriting finalized history.

What it does **not** solve (and what to be careful about):

* **No view-change certificate / no timeout slashing.** A stalled or offline
  proposer is simply skipped; validators are not punished for liveness
  failures. If fewer than 1/3 of the stake is online, finality cannot
  advance, but a chain with enough controllers can still produce blocks.
  Liveness and safety are traded separately: controller quorum gates
  production, finality gates irreversibility.
* **Controller quorum is not BFT safety.** Each node chooses controllers
  from its own peer view; a set can be small or empty (`SAN_CONTROLLER_COUNT=0`
  accepts blocks locally). The 2/3 finality vote is the stronger guarantee,
  but it only finalizes what is already on a node's canonical chain.
* **`controller_min_stake=0` is sybil-prone.** The default only checks that
  a peer presents a public key; open networks should stake-gate controllers
  and validators.
* **No long-range / weak-subjectivity protection beyond unbonding.** A node
  that syncs from a malicious peer still replays and verifies every block and
  the state root, so it cannot be given false state, but it can be shown a
  valid minority chain of the same genesis. Finality checkpoints are only a
  local persisted anchor; there is no checkpoint gossip or light-client
  finality proof service.
* **No anti-censorship guarantee.** Only the expected proposer may build a
  block for a height; a proposer can censor transactions until the round
  advances and the next proposer includes them.
* **Timing assumptions.** Round advance uses the local clock, and round
  justification allows timestamps as early as
  `tip + round * timeout * 0.8`. A node with a badly skewed clock may be
  slower to propose, but cannot force invalid rounds on others.
* **`VOTE_LOOKAHEAD` staging is only an anti-spam bound.** Votes for heights
  up to 64 ahead of the tip are stored; every other future height is dropped.
  Staged votes for a block that never arrives age out only when their height
  is pruned or the node restarts.
* **Stake concentration.** 2/3 finality and governance assume stake is not
  concentrated; there is no cap, delegation market or slashing for
  governance capture.
* **Not covered: MEV, fee-market manipulation, contract bugs, peer
  eclipsing** (a node can be surrounded by hostile peers; it still verifies
  every block, but can miss transactions and votes until it finds an honest
  peer).

---

## Status / limitations

The Go node in `internal/netnode` and the Python reference in
`network/Node.py` implement the same consensus rules; the two were compared
against each other while writing this document. Differences worth knowing:

* **Round clock source**: Go `currentRoundLocked` uses `time.Now()` (wall
  clock), Python `_current_round` uses `time.monotonic()`. Only the local
  proposal decision is affected; remote blocks are accepted purely via the
  timestamp justification in `verify_block`, so consensus is unaffected. A
  system clock jump could make a Go node's local round jump.
* **Exported wrappers**: Go exposes `VerifyBlock`, `CommitBlock`,
  `GossipBlock`, `BroadcastBlock` and `AttestationState` for the REST API and
  tests; the internal call paths are the same ones the node itself uses.
* **Parity scope**: `tools/parity_fixtures.py` + `internal/parity` compare
  pure functions (canonical JSON, hashes, merkle/state roots, economics,
  transactions, blocks, chain replay). There is no Go-vs-Python live
  consensus harness; `cmd/sane2e` is the Go-only end-to-end test.
* **Controller responses**: a controller verifies with live rules
  (`VerifyBlock(block, false)` in Go, `verify_block(block)` in Python); the
  proposal message is a full block dict, so a controller needs the block's
  parent to be its tip to approve. A controller that is behind returns
  `approved: false`, which counts as a missing approval.
* **`active_validators_for` ignores release timing** in both
  implementations (documented in `blockchain/Blockchain.py:147`). A validator
  that has undelegated can still be counted for a height committed one block
  earlier; votes are tallied within a block or two of the target height, so
  the practical window is tiny.
* **`min_block_interval` also applies to sync replay**: a subsidy chain
  (block_reward > 0) enforces at least 1 s between consecutive blocks at all
  times, including historical verification.
* **No finality vote gossip over a dedicated port**: votes travel on the
  peer port as `FINALITY_VOTE` messages; the controller port is only used for
  `BLOCK_VOTE_REQUEST`/`BLOCK_VOTE_RESPONSE`.
* **Stricter Go persistence/fork behavior**: the Go node refuses to start when
  the persisted finality checkpoint contradicts the canonical index (Python
  logs and ignores it), deduplicates controller records by signing key, and
  retains a replaced branch's verified blocks as orphans (Python discards
  them). None of this changes the wire rules or block validity; it only
  removes guessing and re-fetch delays on the Go node.

---

## 10. SANVM gas and collection hardening (Batch C)

This section records the resource bounds added to the Go SANVM/PENA
implementation (`internal/sanvm`, mirrored by `SANVM/` where noted) and the
intentional Go/Python differences.

### 10.1 Existing schedule and limits

* Every opcode has a fixed cost (`GasCosts`); `PUSH` payloads are priced by
  canonical byte size (16 bytes per gas).
* Integer arithmetic is priced by operand width
  (`max(bit_length(a), bit_length(b)) / 64` extra gas), so repeated squaring
  cannot burn CPU for the base opcode cost.
* A stack value may not exceed `MaxValueBytes = 65,536` serialized bytes or
  `MaxIntBits = 4096` bits.
* `DefaultMaxSteps = 100,000`, `DefaultMaxStack = 1,024`,
  `DefaultMaxCallDepth = 64`, and an optional gas limit bound every run.

### 10.2 Batch C additions (Go-only guards)

* `MaxCollectionItems = 65,536`: stored lists and list-backed dicts cannot
  grow past this item count. Python has no document-level cap; the Go node
  rejects the operation with `VMError("... exceeds 65536 items")`.
* `LIST_REMOVE` charges an extra `len(list) / 1024` gas above 1,024 items;
  `DICT_KEYS` charges an extra `len(dict) / 1024` gas above 1,024 entries.
  Below those thresholds the gas schedule is byte-for-byte the Python
  schedule, so normal contracts are unaffected.
* Python's `str * int` and `list * int` repetition is now implemented in Go
  (previously it failed with an operand-type error). Results that would exceed
  `MaxValueBytes` are rejected before allocating memory.

### 10.3 Differential parity fuzzer and known deviations

`internal/parity/differential_fuzz_test.go` generates deterministic SANVM
programs (arithmetic, stack ops, control flow, storage, lists/dicts, function
calls, gas/step limits, extreme integers) and executes each on the Go VM and
the Python reference VM, comparing final storage, stack, logs, gas used, error
type and error timing (`steps`/`pc`). Run it with `SAN_PARITY_CASES=N`
(default 128, `-short` skips; long runs use thousands) and
`SAN_PARITY_SEED`. Mismatches are written to
`internal/parity/testdata/parity_regression_*.json` and replayed by
`TestParityRegressionFixtures`.

Intentional deviations kept in the comparator:

* **`DICT_KEYS` order**: Go returns sorted keys, Python returns insertion
  order. Cases that observe key order are compared with string-list order
  relaxed. This is a documented deviation, not a consensus change (both nodes
  run the same implementation on a chain).
* **Operand-type failures**: Python raises `TypeError`; Go mirrors the name
  with `sanvm.TypeError` (division/modulo by zero stay `VMError` in both).
* **Hardening rejections**: the Go-only collection cap and superlinear charges
  above 1,024 items fire only on programs Python would run for a long
  time/with unbounded memory.

Fuzzing (10-30 s per target) found and fixed a real panic: comparing a number
with a non-numeric value called `big.Int.Cmp` on a nil pointer, crashing the
node on hostile contract bytecode. Regression test:
`TestMixedTypeComparisonsNeverPanic` plus the retained fuzz seed
`internal/sanvm/testdata/fuzz/FuzzVM/36c112d8fc0ef853`.



