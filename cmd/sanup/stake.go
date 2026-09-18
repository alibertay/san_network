package main

import (
	"fmt"
	"io"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sdk"
)

func newHealthClient(host string, port int, timeout time.Duration, token ...string) *sdk.SanClient {
	client := sdk.NewSanClient(apiURL(host, port), nil, timeout)
	if len(token) > 0 {
		client.SetToken(token[0])
	}
	return client
}

// reconcileStake drives the on-chain stake to targetUnits. Increases are a
// plain deposit; reductions use the node's all-or-nothing undelegate, wait for
// the (dev: zero) unbonding period, withdraw and re-stake the target.
func reconcileStake(client *sdk.SanClient, address string, targetUnits int64, stdout, stderr io.Writer) error {
	current := currentStakeUnits(client, address)
	logf(stdout, "stake before: %s SAN (target %s SAN)",
		ledger.UnitsToSAN(current), ledger.UnitsToSAN(targetUnits))
	if current == targetUnits {
		logf(stdout, "stake already matches the target; nothing to do")
		return nil
	}
	if targetUnits > current {
		if err := depositStake(client, address, targetUnits-current, stdout); err != nil {
			return err
		}
	} else {
		if current > 0 {
			logf(stdout,
				"reducing stake: the node only supports an all-or-nothing undelegate, "+
					"so this undelegates all, withdraws and re-stakes %s SAN",
				ledger.UnitsToSAN(targetUnits))
			if err := withdrawAllStake(client, address, stdout); err != nil {
				return err
			}
		}
		if targetUnits > 0 {
			if err := depositStake(client, address, targetUnits, stdout); err != nil {
				return err
			}
		}
	}
	final := currentStakeUnits(client, address)
	logf(stdout, "stake after: %s SAN", ledger.UnitsToSAN(final))
	if final != targetUnits {
		return fmt.Errorf("stake reconciliation did not converge: expected %s SAN, chain reports %s SAN",
			ledger.UnitsToSAN(targetUnits), ledger.UnitsToSAN(final))
	}
	return nil
}

func currentStakeUnits(client *sdk.SanClient, address string) int64 {
	record, err := client.Stake(address)
	if err != nil {
		return 0
	}
	return anyToInt64(record["stake_units"])
}

func depositStake(client *sdk.SanClient, address string, delta int64, stdout io.Writer) error {
	before := currentStakeUnits(client, address)
	amount := ledger.UnitsToSAN(delta)
	logf(stdout, "depositing %s SAN", amount)
	result, err := client.DepositStake(amount, nil)
	if err != nil {
		return fmt.Errorf("stake deposit failed: %w", err)
	}
	logf(stdout, "deposit: status=%v tx=%v", result["status"], result["tx_id"])
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		if units := currentStakeUnits(client, address); units >= before+delta {
			logf(stdout, "deposit committed: stake is now %s SAN", ledger.UnitsToSAN(units))
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for the %s SAN deposit to commit", amount)
}

func withdrawAllStake(client *sdk.SanClient, address string, stdout io.Writer) error {
	result, err := client.Undelegate(nil)
	if err != nil {
		return fmt.Errorf("undelegate failed: %w", err)
	}
	logf(stdout, "undelegate: status=%v tx=%v", result["status"], result["tx_id"])

	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		record, err := client.Stake(address)
		if err == nil && record["release_height"] != nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	logf(stdout, "undelegate committed; unbonding period is 0 in dev, withdrawing now")

	result, err = client.WithdrawStake(nil)
	if err != nil {
		return fmt.Errorf("withdraw failed: %w", err)
	}
	logf(stdout, "withdraw: status=%v tx=%v", result["status"], result["tx_id"])

	deadline = time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if currentStakeUnits(client, address) == 0 {
			logf(stdout, "withdrew the stake (stake is now 0 SAN)")
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for the stake withdrawal to commit")
}
