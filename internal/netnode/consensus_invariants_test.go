package netnode

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
)

// ---------------------------------------------------------------------- #
// Proposer selection and rounds
// ---------------------------------------------------------------------- #

// TestProposerSelectionDeterministic asserts the proposer for a height/round
// never changes across 100 passes over a 12-validator active set.
func TestProposerSelectionDeterministic(t *testing.T) {
	vs := newValidatorSet(t, 12)
	seen := map[string]string{}
	for run := 0; run < 100; run++ {
		for height := int64(1); height <= 30; height++ {
			for round := int64(0); round < 20; round++ {
				key := fmt.Sprintf("%d:%d", height, round)
				got := vs.node.ExpectedProposer(height, round)
				if got == "" {
					t.Fatalf("height %d round %d has no proposer", height, round)
				}
				if previous, present := seen[key]; present && previous != got {
					t.Fatalf("proposer for %s changed between runs: %s -> %s", key, previous, got)
				}
				seen[key] = got
			}
		}
	}
	for round := int64(0); round < 12; round++ {
		if a, b := vs.node.ExpectedProposer(9, round), vs.node.ExpectedProposer(9, round+12); a != b {
			t.Fatalf("proposer rotation does not wrap: round %d=%s, round %d=%s", round, a, round+12, b)
		}
	}
}

// TestOnlyExpectedProposerMayPropose checks both positive and negative cases
// when validators exist.
func TestOnlyExpectedProposerMayPropose(t *testing.T) {
	vs := newValidatorSet(t, 3)
	goodRound := proposalRoundFor(vs.node, vs.addrs[0])
	good := buildProposal(t, vs.node, vs.ids[0], nil, goodRound)
	if !vs.node.VerifyBlock(good, true) {
		t.Fatalf("expected proposer block was rejected")
	}

	wrongRound := (goodRound + 1) % 3
	wrong := buildProposal(t, vs.node, vs.ids[0], nil, wrongRound)
	if vs.node.VerifyBlock(wrong, true) {
		t.Fatalf("block from the wrong round proposer was accepted")
	}

	outsider := mustIdentity(t)
	outsiderBlock := buildProposal(t, vs.node, outsider, nil, 0)
	if vs.node.VerifyBlock(outsiderBlock, true) {
		t.Fatalf("non-validator block was accepted")
	}
}

// TestBootstrapFallbackWithoutValidators covers the documented fallback: with
// no active validators any node may propose and no proposer is expected.
func TestBootstrapFallbackWithoutValidators(t *testing.T) {
	config := consensusConfig()
	node, err := NewNode(config, mustIdentity(t))
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	if got := node.ExpectedProposer(1, 0); got != "" {
		t.Fatalf("bootstrap expected proposer: got %q, want empty", got)
	}
	block := buildProposal(t, node, node.identity, nil, 0)
	if !node.VerifyBlock(block, true) {
		t.Fatalf("bootstrap block was rejected")
	}
}

// TestRoundAdvancementAndCap checks the round clock, its cap and the
// timestamp justification of non-zero rounds.
func TestRoundAdvancementAndCap(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node

	height := node.blockchain.Tip().Index + 1
	timeout := node.blockchain.ProposerTimeout()

	node.mu.Lock()
	node.proposerState = &proposerRoundState{
		Height:  height,
		Round:   0,
		Started: time.Now().Add(-time.Duration((3*timeout + 0.5) * float64(time.Second))),
	}
	advanced := node.currentRoundLocked(height)
	node.mu.Unlock()
	if advanced != 3 {
		t.Fatalf("round after 3.5 timeouts: got %d, want 3", advanced)
	}

	node.mu.Lock()
	node.proposerState.Started = time.Now().Add(-time.Hour)
	capped := node.currentRoundLocked(height)
	node.mu.Unlock()
	if capped != 15 {
		t.Fatalf("round cap: got %d, want 15 (max_proposer_rounds-1)", capped)
	}

	overCap := buildProposal(t, node, vs.ids[0], nil, 16)
	if node.VerifyBlock(overCap, true) {
		t.Fatalf("block with round 16 was accepted (cap is 16 rounds: 0..15)")
	}

	tip := node.blockchain.Tip()
	underJustified := buildProposal(t, node, vs.ids[0], nil, 2)
	underJustified.Timestamp = tip.TimestampFloat() + timeout*0.8 // 1.0x timeout < 2 * 0.8
	underJustified.CurrentBlockHash = underJustified.CalculateHash()
	underJustified.ValidatorSignature = vs.ids[0].SignHex([]byte(underJustified.CurrentBlockHash))
	if node.VerifyBlock(underJustified, true) {
		t.Fatalf("round 2 block without timestamp justification was accepted")
	}

	justified := buildProposal(t, node, vs.ids[0], nil, 2)
	if !node.VerifyBlock(justified, true) {
		t.Fatalf("properly justified round 2 block was rejected")
	}
}

// ---------------------------------------------------------------------- #
// Validator set transitions and slashing
// ---------------------------------------------------------------------- #

func TestValidatorLifecycleAndSetTransitions(t *testing.T) {
	vs := newValidatorSet(t, 2)
	node := vs.node
	if len(node.blockchain.ActiveValidators()) != 2 {
		t.Fatalf("expected 2 active validators, got %d", len(node.blockchain.ActiveValidators()))
	}

	undelegate := validatorCommandPayload(t, node, vs.ids[1], 1, map[string]any{"command": "undelegate"})
	undelegateBlock := commitProposal(t, node, vs.ids[0], []any{undelegate})
	release := undelegateBlock.Index + node.blockchain.UnbondingPeriod()

	active := node.blockchain.ActiveValidators()
	if _, present := active[vs.addrs[1]]; present {
		t.Fatalf("undelegating validator is still active")
	}
	info := node.blockchain.Validators[vs.addrs[1]]
	if int64Value(info["release_height"]) != release {
		t.Fatalf("release_height: got %v, want %d", info["release_height"], release)
	}
	if _, present := node.blockchain.ActiveValidatorsFor(undelegateBlock.Index)[vs.addrs[1]]; !present {
		t.Fatalf("ActiveValidatorsFor must ignore release timing for the freeze height")
	}

	// Withdrawing before the release height must fail during simulation.
	earlyWithdraw := validatorCommandPayload(t, node, vs.ids[1], 2, map[string]any{"command": "withdraw"})
	if node.simulateBlock(rawProposal(node, vs.ids[0], []any{earlyWithdraw}, 0), false) != nil {
		t.Fatalf("early withdraw was accepted")
	}

	for node.blockchain.Tip().Index < release {
		commitProposal(t, node, vs.ids[0], nil)
	}

	balanceBefore := node.blockchain.SAN[vs.addrs[1]]
	withdraw := validatorCommandPayload(t, node, vs.ids[1], 2, map[string]any{"command": "withdraw"})
	commitProposal(t, node, vs.ids[0], []any{withdraw})
	if _, present := node.blockchain.Validators[vs.addrs[1]]; present {
		t.Fatalf("withdrawn validator record still exists")
	}
	wantWithdraw := balanceBefore - int64Value(withdraw["fee"]) + testStakeUnits
	if got := node.blockchain.SAN[vs.addrs[1]]; got != wantWithdraw {
		t.Fatalf("withdraw returned wrong amount: got %d, want %d", got, wantWithdraw)
	}
}

func TestStakeAccountingAndSlashingMath(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node
	victim := vs.addrs[1]

	height := node.blockchain.Tip().Index + 1
	voteA := finalityVoteFor(t, node, vs.ids[1], height, strings.Repeat("aa", 32))
	voteB := finalityVoteFor(t, node, vs.ids[1], height, strings.Repeat("bb", 32))
	node.handleFinalityVote(voteA)
	node.handleFinalityVote(voteB)
	if len(node.EquivocationEvidence()) != 1 {
		t.Fatalf("expected 1 equivocation evidence record, got %d", len(node.EquivocationEvidence()))
	}

	balanceBefore := node.blockchain.SAN[victim]
	evidence := validatorCommandPayload(t, node, vs.ids[0], 1, map[string]any{
		"command": "evidence",
		"vote_a":  voteA,
		"vote_b":  voteB,
	})
	commitProposal(t, node, vs.ids[0], []any{evidence})

	wantBurned := testStakeUnits * ledger.DefaultSlashBps / 10_000
	if got := node.blockchain.TotalSlashed; got != wantBurned {
		t.Fatalf("total_slashed: got %d, want %d", got, wantBurned)
	}
	if got := node.blockchain.SAN[victim]; got != balanceBefore+testStakeUnits-wantBurned {
		t.Fatalf("slashed balance returned the wrong remainder: got %d, want %d",
			got, balanceBefore+testStakeUnits-wantBurned)
	}
	if _, present := node.blockchain.Validators[victim]; present {
		t.Fatalf("slashed validator was not removed")
	}
	if node.blockchain.TotalBurned != 0 {
		t.Fatalf("slashing must not increase total_burned")
	}
}

// ---------------------------------------------------------------------- #
// Governance
// ---------------------------------------------------------------------- #

func governancePayload(t *testing.T, node *Node, sender *ledger.NodeIdentity, nonce int64, name string, value int64, approvers ...*ledger.NodeIdentity) map[string]any {
	t.Helper()
	approvals := make([]any, 0, len(approvers))
	for _, approver := range approvers {
		approvals = append(approvals, governanceApproval(node.chainID, name, value, nonce, sender.PublicKeyHex(), approver))
	}
	return signTransaction(t, node, sender, map[string]any{
		"chain_id": node.chainID,
		"sender":   sender.PublicKeyHex(),
		"nonce":    nonce,
		"governance": map[string]any{
			"command":   "set_param",
			"name":      name,
			"value":     value,
			"approvals": approvals,
		},
	}).Payload
}

func TestGovernanceParameterUpdateAndEffects(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node

	update := governancePayload(t, node, vs.ids[0], 1, "proposer_timeout_ms", 2000, vs.ids[0], vs.ids[1])
	block := commitProposal(t, node, vs.ids[0], []any{update})
	if got := node.blockchain.Parameters["proposer_timeout_ms"]; got != 2000 {
		t.Fatalf("proposer_timeout_ms: got %d, want 2000", got)
	}
	if got := node.blockchain.ProposerTimeout(); got != 2.0 {
		t.Fatalf("ProposerTimeout(): got %v, want 2.0", got)
	}

	// A round justified under the old 6s timeout but not under the new 2s one
	// must now be rejected.
	round := proposalRoundFor(node, vs.addrs[0]) + 3
	roundBlock := buildProposal(t, node, vs.ids[0], nil, round)
	roundBlock.Timestamp = block.TimestampFloat() + float64(round)*2.0*0.8 - 1
	roundBlock.CurrentBlockHash = roundBlock.CalculateHash()
	roundBlock.ValidatorSignature = vs.ids[0].SignHex([]byte(roundBlock.CurrentBlockHash))
	if node.VerifyBlock(roundBlock, true) {
		t.Fatalf("round justification ignored the updated proposer timeout")
	}
}

func TestGovernanceApprovalRules(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node

	// Under quorum: 1 of 3 validators.
	underQuorum := governancePayload(t, node, vs.ids[0], 1, "slash_bps", 2000, vs.ids[0])
	if node.simulateBlock(rawProposal(node, vs.ids[0], []any{underQuorum}, 0), false) != nil {
		t.Fatalf("governance change below 2/3 stake was accepted")
	}

	// Duplicate approval from the same validator counts once.
	duplicate := governancePayload(t, node, vs.ids[0], 1, "slash_bps", 2000, vs.ids[0], vs.ids[0])
	if node.simulateBlock(rawProposal(node, vs.ids[0], []any{duplicate}, 0), false) != nil {
		t.Fatalf("duplicate governance approval was accepted")
	}

	// Non-validator approval.
	outsider := mustIdentity(t)
	nonValidator := governancePayload(t, node, vs.ids[0], 1, "slash_bps", 2000, vs.ids[0], vs.ids[1], outsider)
	if node.simulateBlock(rawProposal(node, vs.ids[0], []any{nonValidator}, 0), false) != nil {
		t.Fatalf("non-validator governance approval was accepted")
	}

	// An approval cannot be replayed onto another transaction (different nonce).
	// The tx sender is validator 3, but validator 1's approval was signed for
	// nonce 2 while the transaction carries nonce 1.
	approvalForNonceTwo := governanceApproval(node.chainID, "slash_bps", 2000, 2, vs.ids[2].PublicKeyHex(), vs.ids[0])
	replayed := signTransaction(t, node, vs.ids[2], map[string]any{
		"chain_id": node.chainID,
		"sender":   vs.ids[2].PublicKeyHex(),
		"nonce":    int64(1),
		"governance": map[string]any{
			"command": "set_param", "name": "slash_bps", "value": int64(2000),
			"approvals": []any{
				approvalForNonceTwo,
				governanceApproval(node.chainID, "slash_bps", 2000, 1, vs.ids[2].PublicKeyHex(), vs.ids[1]),
			},
		},
	}).Payload
	if node.simulateBlock(rawProposal(node, vs.ids[2], []any{replayed}, 0), false) != nil {
		t.Fatalf("replayed governance approval was accepted")
	}

	// Positive: exactly 2/3 stake applies the change.
	valid := governancePayload(t, node, vs.ids[0], 1, "slash_bps", 2000, vs.ids[0], vs.ids[1])
	commitProposal(t, node, vs.ids[0], []any{valid})
	if got := node.blockchain.Parameters["slash_bps"]; got != 2000 {
		t.Fatalf("slash_bps: got %d, want 2000", got)
	}
}

func TestGovernanceMinValidatorStakeEffect(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node

	raiseStake := governancePayload(t, node, vs.ids[0], 1, "min_validator_stake", testStakeUnits+1, vs.ids[0], vs.ids[1])
	commitProposal(t, node, vs.ids[0], []any{raiseStake})
	if got := len(node.blockchain.ActiveValidators()); got != 0 {
		t.Fatalf("raising min_validator_stake left %d active validators, want 0", got)
	}

	depositA := validatorCommandPayload(t, node, vs.ids[0], 2, map[string]any{"command": "deposit", "amount": testStakeUnits})
	depositB := validatorCommandPayload(t, node, vs.ids[1], 1, map[string]any{"command": "deposit", "amount": testStakeUnits})
	commitProposal(t, node, vs.ids[0], []any{depositA, depositB})
	active := node.blockchain.ActiveValidators()
	if len(active) != 2 {
		t.Fatalf("re-deposit left %d active validators, want 2", len(active))
	}
	if _, present := active[vs.addrs[2]]; present {
		t.Fatalf("validator below the raised minimum is still active")
	}
}

func TestGovernanceGasLimitEffect(t *testing.T) {
	vs := newValidatorSet(t, 3)
	node := vs.node

	lowerGas := governancePayload(t, node, vs.ids[0], 1, "block_gas_limit", 100_000, vs.ids[0], vs.ids[1])
	commitProposal(t, node, vs.ids[0], []any{lowerGas})
	if got := node.blockchain.BlockGasLimit(); got != 100_000 {
		t.Fatalf("block_gas_limit: got %d, want 100000", got)
	}
	bigExec := signTransaction(t, node, vs.ids[0], map[string]any{
		"chain_id":  node.chainID,
		"sender":    vs.ids[0].PublicKeyHex(),
		"nonce":     int64(2),
		"bytecode":  []any{[]any{"PUSH", int64(1)}, []any{"POP"}},
		"gas_limit": int64(200_000),
		"gas_price": int64(ledger.MinGasPrice),
	}).Payload
	if node.simulateBlock(rawProposal(node, vs.ids[0], []any{bigExec}, 0), false) != nil {
		t.Fatalf("execution above the governed block gas limit was accepted")
	}
}

func TestGovernanceMinBlockIntervalEffect(t *testing.T) {
	vs := newValidatorSet(t, 1)
	node := vs.node

	interval := governancePayload(t, node, vs.ids[0], 1, "min_block_interval_ms", 5000, vs.ids[0])
	commitProposal(t, node, vs.ids[0], []any{interval})
	if got := node.blockchain.MinBlockInterval(); got != 5.0 {
		t.Fatalf("MinBlockInterval: got %v, want 5.0", got)
	}
	tipTimestamp := node.blockchain.Tip().TimestampFloat()

	tooSoon := buildProposalAt(t, node, vs.ids[0], nil, 0, tipTimestamp+1)
	if node.VerifyBlock(tooSoon, true) {
		t.Fatalf("block sooner than the governed minimum interval was accepted")
	}
	allowed := buildProposalAt(t, node, vs.ids[0], nil, 0, tipTimestamp+5)
	if !node.VerifyBlock(allowed, true) {
		t.Fatalf("block at the governed minimum interval was rejected")
	}
}

// ---------------------------------------------------------------------- #
// Block reward and fee/base-fee arithmetic
// ---------------------------------------------------------------------- #

func TestBlockRewardMintingAndFeeArithmetic(t *testing.T) {
	vs := newValidatorSetCustom(t, 1, nil, func(config *NodeConfig) {
		config.BlockReward = 2.0
	})
	node := vs.node
	validator := vs.addrs[0]
	receiver := "0x" + strings.Repeat("ab", 20)

	// Transfer block: the fee is paid by the sender and fully returned as the
	// proposer tip; the subsidy is minted on top.
	transfer := transferPayload(t, node, vs.ids[0], 1, receiver, "1")
	before := node.blockchain.SAN[validator]
	block := commitProposal(t, node, vs.ids[0], []any{transfer})
	fee := int64Value(transfer["fee"])
	wantBalance := before - ledger.SANBase - fee + fee + 2*ledger.SANBase
	if got := node.blockchain.SAN[validator]; got != wantBalance {
		t.Fatalf("proposer balance after transfer block: got %d, want %d", got, wantBalance)
	}
	if got := node.blockchain.SAN[receiver]; got != ledger.SANBase {
		t.Fatalf("receiver balance: got %d, want %d", got, ledger.SANBase)
	}
	if block.StateRoot == nil {
		t.Fatalf("block has no state root")
	}

	// Execution block: verify escrow/refund/burn accounting exactly.
	exec := signTransaction(t, node, vs.ids[0], map[string]any{
		"chain_id":  node.chainID,
		"sender":    vs.ids[0].PublicKeyHex(),
		"nonce":     int64(2),
		"bytecode":  []any{[]any{"PUSH", int64(1)}, []any{"POP"}},
		"gas_limit": int64(1000),
		"gas_price": int64(ledger.MinGasPrice),
	}).Payload
	baseFeeBefore := node.blockchain.BaseFee
	before = node.blockchain.SAN[validator]
	burnedBefore := node.blockchain.TotalBurned
	execBlock := commitProposal(t, node, vs.ids[0], []any{exec})
	receipts := node.receipts[execBlock.Index]
	receipt, _ := receipts[0].(map[string]any)
	gasUsed := int64Value(receipt["gas_used"])
	if gasUsed <= 0 {
		t.Fatalf("execution reported no gas use")
	}
	fee = int64Value(exec["fee"])
	refund := (int64Value(exec["gas_limit"]) - gasUsed) * int64Value(exec["gas_price"])
	burned := gasUsed * baseFeeBefore
	wantBalance = before - fee + refund + (fee - refund - burned) + 2*ledger.SANBase
	if got := node.blockchain.SAN[validator]; got != wantBalance {
		t.Fatalf("proposer balance after execution block: got %d, want %d", got, wantBalance)
	}
	if got := node.blockchain.TotalBurned - burnedBefore; got != burned {
		t.Fatalf("total_burned delta: got %d, want %d", got, burned)
	}
	wantBaseFee := ledger.NextBaseFee(baseFeeBefore, gasUsed, node.blockchain.BlockGasLimit())
	if got := node.blockchain.BaseFee; got != wantBaseFee {
		t.Fatalf("base_fee: got %d, want %d", got, wantBaseFee)
	}
}
