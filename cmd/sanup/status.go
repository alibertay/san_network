package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
	"github.com/alibertay/san_network/internal/sdk"
)

type statusReport struct {
	Running         bool   `json:"running"`
	PID             int    `json:"pid,omitempty"`
	Address         string `json:"address,omitempty"`
	API             string `json:"api,omitempty"`
	DataDir         string `json:"data_dir,omitempty"`
	Height          int64  `json:"height"`
	FinalizedHeight int64  `json:"finalized_height"`
	Peers           int64  `json:"peers"`
	Validators      int64  `json:"validators"`
	Balance         string `json:"balance,omitempty"`
	BalanceUnits    int64  `json:"balance_units"`
	Stake           string `json:"stake,omitempty"`
	StakeUnits      int64  `json:"stake_units"`
	ActiveValidator bool   `json:"active_validator"`
}

// printStatus reports the node recorded in the data directory. It returns a
// non-zero code only for JSON encoding failures.
func printStatus(state nodeState, wallet string, stdout, stderr io.Writer, asJSON bool, token string) int {
	host := connectHost(state.Host)
	running := state.PID > 0 && processAlive(state.PID) &&
		nodeHealthyAuth(host, state.APIPort, 1500*time.Millisecond, token)

	report := statusReport{
		Running: running,
		PID:     state.PID,
		Address: wallet,
	}
	if state.APIPort > 0 {
		report.API = apiURL(host, state.APIPort)
	}
	if running {
		client := sdk.NewSanClient(report.API, nil, 5*time.Second).SetToken(token)
		if health, err := client.Health(); err == nil {
			report.Height = anyToInt64(health["height"])
			report.FinalizedHeight = anyToInt64(health["finalized_height"])
			report.Peers = anyToInt64(health["peers"])
			report.Validators = anyToInt64(health["validators"])
		}
		if wallet != "" {
			if account, err := client.Account(wallet); err == nil {
				report.BalanceUnits = anyToInt64(account["balance_units"])
				if text, ok := account["balance"].(string); ok {
					report.Balance = text
				} else {
					report.Balance = ledger.UnitsToSAN(report.BalanceUnits)
				}
			}
			record, err := client.Stake(wallet)
			if err == nil {
				report.StakeUnits = anyToInt64(record["stake_units"])
				report.Stake = ledger.UnitsToSAN(report.StakeUnits)
			}
			if validators, err := client.Validators(); err == nil {
				if records, ok := validators["validators"].([]any); ok {
					for _, raw := range records {
						if record, ok := raw.(map[string]any); ok && anyToString(record["address"]) == wallet {
							report.ActiveValidator = true
						}
					}
				}
			}
		}
	}

	if asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintf(stderr, "[sanup] cannot encode the status: %v\n", err)
			return 1
		}
		return 0
	}

	if !running {
		logf(stdout, "node is not running")
		if wallet != "" {
			logf(stdout, "wallet : %s", wallet)
			if state.APIPort > 0 {
				logf(stdout, "start it with: go run ./cmd/sanup --wallet %s", wallet)
			}
		}
		return 0
	}
	logf(stdout, "node is running (pid %d) at %s", state.PID, report.API)
	fmt.Fprintf(stdout, "  wallet           : %s\n", report.Address)
	fmt.Fprintf(stdout, "  height           : %d (finalized %d)\n", report.Height, report.FinalizedHeight)
	fmt.Fprintf(stdout, "  peers            : %d (validators %d)\n", report.Peers, report.Validators)
	fmt.Fprintf(stdout, "  balance          : %s SAN\n", report.Balance)
	if report.ActiveValidator {
		fmt.Fprintf(stdout, "  stake            : %s SAN (%d units) [active validator]\n", report.Stake, report.StakeUnits)
	} else {
		fmt.Fprintf(stdout, "  stake            : %s SAN (%d units)\n", report.Stake, report.StakeUnits)
	}
	if state.LogFile != "" {
		fmt.Fprintf(stdout, "  log              : %s\n", state.LogFile)
	}
	return 0
}

func runStatus(opts options, stdout, stderr io.Writer) int {
	dataDir, _, _, stateAddress, code := resolveIdentity(opts, stdout, stderr, false)
	if code >= 0 {
		return code
	}
	state := readState(dataDir)
	if state.Host == "" {
		state.Host = opts.host
	}
	if state.APIPort == 0 {
		state.APIPort = opts.apiPort
	}
	wallet := state.Address
	if stateAddress != "" {
		wallet = stateAddress
	}
	return printStatus(state, wallet, stdout, stderr, opts.json, opts.apiToken)
}

func runStop(opts options, stdout, stderr io.Writer) int {
	dataDir, err := filepath.Abs(opts.dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "[sanup] invalid --data-dir: %v\n", err)
		return 2
	}
	state := readState(dataDir)
	switch {
	case state.PID <= 0 || !processAlive(state.PID):
		logf(stdout, "no running node found for %s", dataDir)
	case !isNodeProcess(state.PID):
		logf(stdout, "refusing to stop pid %d: it is not a sanup node process (stale %s?)",
			state.PID, stateFileName)
	default:
		killProcess(state.PID)
		for attempt := 0; attempt < 40 && processAlive(state.PID); attempt++ {
			time.Sleep(250 * time.Millisecond)
		}
		logf(stdout, "stopped the node (pid %d)", state.PID)
	}
	removeRegistryEntry(state)
	state.PID = 0
	_ = saveState(dataDir, state)
	return 0
}

// isNodeProcess reports whether pid belongs to a sanup node executable: the
// staged child (data-dir/sanup-node[.exe]) or this launcher itself. A stale or
// tampered pid file must not let --stop kill an arbitrary process.
func isNodeProcess(pid int) bool {
	name := filepath.Base(processImageName(pid))
	if name == "" {
		return false
	}
	if strings.EqualFold(name, "sanup-node") || strings.EqualFold(name, "sanup-node.exe") {
		return true
	}
	executable, err := os.Executable()
	return err == nil && strings.EqualFold(filepath.Base(executable), name)
}

// removeRegistryEntry drops this node's record after a forced stop, since a
// killed process cannot clean up after itself.
func removeRegistryEntry(state nodeState) {
	path := netnode.DefaultPeerRegistryPath()
	if path == "" || (state.PublicKey == "" && state.APIPort == 0) {
		return
	}
	host := state.Host
	if host == "" {
		host = "127.0.0.1"
	}
	_ = netnode.RemovePeerRegistryEntry(path, state.PublicKey, host, int64(state.APIPort))
}
