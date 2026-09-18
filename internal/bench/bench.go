// Package bench implements the measurements behind cmd/sanbench. Every
// function is deterministic-ish (no random workloads) and returns plain
// Result values so the CLI can render JSON or a human table.
//
// Methodology notes live in docs/benchmarks.md. In short: operations are
// timed with wall-clock monotonic time, warm-up runs are included for VM
// workloads, and no result is advertised without the measured operation
// count, hardware and build metadata.
package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/alibertay/san_network/internal/canonical"
	"github.com/alibertay/san_network/internal/crypto"
	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/sanvm"
)

var outputMu sync.Mutex

// SilenceOutput redirects os.Stdout to the null device while fn runs. The VM
// prints contract output to stdout (the node captures it in logs/receipts),
// which would otherwise corrupt the machine-readable benchmark report.
func SilenceOutput(fn func()) {
	outputMu.Lock()
	defer outputMu.Unlock()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		fn()
		return
	}
	saved := os.Stdout
	os.Stdout = devnull
	defer func() {
		os.Stdout = saved
		_ = devnull.Close()
	}()
	fn()
}

// Result is one measurement.
type Result struct {
	Name    string         `json:"name"`
	Unit    string         `json:"unit"`
	Value   float64        `json:"value"`
	Ops     int64          `json:"ops,omitempty"`
	Bytes   int64          `json:"bytes,omitempty"`
	Seconds float64        `json:"seconds,omitempty"`
	Error   string         `json:"error,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
}

// Environment describes where the numbers came from.
type Environment struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	CPUs     int    `json:"cpus"`
	Go       string `json:"go"`
	Backend  string `json:"crypto_backend"`
	Workload string `json:"workload"`
}

// Describe captures the host and build environment.
func Describe(workload string) Environment {
	return Environment{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPUs:     runtime.NumCPU(),
		Go:       runtime.Version(),
		Backend:  crypto.BackendName,
		Workload: workload,
	}
}

func perSecond(ops int64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(ops) / elapsed.Seconds()
}

// Signatures measures ML-DSA-44 key generation, signing and verification,
// plus the serialized key/signature sizes.
func Signatures(iterations int) []Result {
	if iterations < 1 {
		iterations = 1
	}
	message := []byte("san-benchmark-message")

	keygenRuns := iterations / 10
	if keygenRuns < 5 {
		keygenRuns = 5
	}
	start := time.Now()
	var publicKey, privateKey []byte
	var err error
	for index := 0; index < keygenRuns; index++ {
		publicKey, privateKey, err = crypto.GenerateKeypair()
		if err != nil {
			return []Result{{Name: "mldsa44.keygen", Unit: "ops/sec", Error: err.Error()}}
		}
	}
	keygen := perSecond(int64(keygenRuns), time.Since(start))

	signature, err := crypto.Sign(message, privateKey)
	if err != nil {
		return []Result{{Name: "mldsa44.sign", Unit: "ops/sec", Error: err.Error()}}
	}

	start = time.Now()
	for index := 0; index < iterations; index++ {
		if _, err := crypto.Sign(message, privateKey); err != nil {
			return []Result{{Name: "mldsa44.sign", Unit: "ops/sec", Error: err.Error()}}
		}
	}
	signRate := perSecond(int64(iterations), time.Since(start))

	start = time.Now()
	for index := 0; index < iterations; index++ {
		if !crypto.Verify(message, signature, publicKey) {
			return []Result{{Name: "mldsa44.verify", Unit: "ops/sec", Error: "signature verification failed"}}
		}
	}
	verifyRate := perSecond(int64(iterations), time.Since(start))

	return []Result{
		{Name: "mldsa44.keygen", Unit: "ops/sec", Value: keygen, Ops: int64(keygenRuns)},
		{Name: "mldsa44.sign", Unit: "ops/sec", Value: signRate, Ops: int64(iterations)},
		{Name: "mldsa44.verify", Unit: "ops/sec", Value: verifyRate, Ops: int64(iterations)},
		{Name: "mldsa44.public_key_size", Unit: "bytes", Value: float64(crypto.PublicKeySize), Bytes: int64(crypto.PublicKeySize)},
		{Name: "mldsa44.private_key_size", Unit: "bytes", Value: float64(crypto.SecretKeySize), Bytes: int64(crypto.SecretKeySize)},
		{Name: "mldsa44.signature_size", Unit: "bytes", Value: float64(crypto.SignatureSize), Bytes: int64(crypto.SignatureSize)},
	}
}

// Transactions measures transaction signing, serialization size/cost and
// full signature validation for plain SAN transfers.
func Transactions(count int) []Result {
	if count < 1 {
		count = 1
	}
	identity, err := ledger.GenerateIdentity()
	if err != nil {
		return []Result{{Name: "transaction.validation", Unit: "ops/sec", Error: err.Error()}}
	}
	receiver, err := ledger.AddressFromPublicKey(identity.PublicKeyHex())
	if err != nil {
		return []Result{{Name: "transaction.validation", Unit: "ops/sec", Error: err.Error()}}
	}
	chainID := ledger.DefaultChainID
	rate := ledger.MinFeePerByte

	payloads := make([]map[string]any, 0, count)
	start := time.Now()
	for index := 0; index < count; index++ {
		payload := map[string]any{
			"chain_id": chainID,
			"sender":   identity.PublicKeyHex(),
			"nonce":    int64(index),
			"receiver": receiver,
			"value":    int64(1),
		}
		signature, err := ledger.SignPayload(payload, identity.PrivateKey)
		if err != nil {
			return []Result{{Name: "transaction.validation", Unit: "ops/sec", Error: err.Error()}}
		}
		payload["signature"] = signature
		transaction, err := ledger.NewTransaction(payload, &rate)
		if err != nil {
			return []Result{{Name: "transaction.validation", Unit: "ops/sec", Error: err.Error()}}
		}
		payloads = append(payloads, transaction.Payload)
	}
	signRate := perSecond(int64(count), time.Since(start))

	totalBytes := int64(0)
	for _, payload := range payloads {
		encoded, err := canonical.Marshal(payload)
		if err != nil {
			return []Result{{Name: "transaction.serialize", Unit: "ops/sec", Error: err.Error()}}
		}
		totalBytes += int64(len(encoded))
	}
	averageBytes := float64(totalBytes) / float64(count)

	serializeRuns := count
	start = time.Now()
	for index := 0; index < serializeRuns; index++ {
		if _, err := canonical.Marshal(payloads[index%len(payloads)]); err != nil {
			return []Result{{Name: "transaction.serialize", Unit: "ops/sec", Error: err.Error()}}
		}
	}
	serializeRate := perSecond(int64(serializeRuns), time.Since(start))

	start = time.Now()
	for index := 0; index < count; index++ {
		if !ledger.VerifyTransaction(payloads[index]) {
			return []Result{{Name: "transaction.validation", Unit: "ops/sec", Error: "transaction did not verify"}}
		}
	}
	validateRate := perSecond(int64(count), time.Since(start))

	return []Result{
		{Name: "transaction.sign", Unit: "ops/sec", Value: signRate, Ops: int64(count)},
		{Name: "transaction.validation", Unit: "ops/sec", Value: validateRate, Ops: int64(count)},
		{Name: "transaction.serialize", Unit: "ops/sec", Value: serializeRate, Ops: int64(serializeRuns)},
		{Name: "transaction.average_bytes", Unit: "bytes", Value: averageBytes, Bytes: int64(averageBytes)},
	}
}

// ---------------------------------------------------------------------- #
// VM workloads
// ---------------------------------------------------------------------- #

const transferContractSource = `data := {}
counter := 0

function set(k, v) {
  data[k] = v
}
function get(k) {
  return data[k]
}
function inc() {
  counter = counter + 1
  return counter
}
function count() {
  return counter
}
`

const storageHeavySource = `data := {}

function fill(n) {
  i = 0
  while i < n {
    data[i] = i
    i = i + 1
  }
  return i
}
`

const arithmeticHeavySource = `function work(n) {
  i = 0
  total = 0
  while i < n {
    total = total + i * 3 - 1
    i = i + 1
  }
  return total
}
`

// VM measures SANVM execution throughput for the contract workloads. Each
// workload is deployed once; the returned rate covers the function calls.
func VM(iterations int) []Result {
	if iterations < 1 {
		iterations = 1
	}
	results := []Result{}
	results = append(results, vmWorkload("simple_transfer", transferContractSource, "inc", nil, iterations)...)
	// Storage/arithmetic workloads use a fixed 200-step body and repeat it
	// `iterations` times so every call is measurable on coarse clocks.
	results = append(results, vmWorkload("storage_heavy", storageHeavySource, "fill", []any{int64(200)}, iterations)...)
	results = append(results, vmWorkload("arithmetic_heavy", arithmeticHeavySource, "work", []any{int64(200)}, iterations)...)

	source, sourceErr := readExample("SANRC20", "SANRC20.pena")
	if sourceErr != nil {
		results = append(results, Result{Name: "sanrc20_transfer", Unit: "ops/sec", Error: sourceErr.Error()})
	} else {
		results = append(results, sanrc20Workload(source, iterations)...)
	}
	source, sourceErr = readExample("AMM", "AMM.pena")
	if sourceErr != nil {
		results = append(results, Result{Name: "amm_swap", Unit: "ops/sec", Error: sourceErr.Error()})
	} else {
		results = append(results, ammWorkload(source, iterations)...)
	}
	return results
}

func vmWorkload(name, source, function string, params []any, iterations int) []Result {
	bytecode, err := sanvm.CompilePena(source, "")
	if err != nil {
		return []Result{{Name: name, Unit: "ops/sec", Error: err.Error()}}
	}
	manager := sanvm.NewContractManager(nil)
	gasLimit := int64(50_000_000)
	if err := manager.DeployContract(name, bytecode, &gasLimit); err != nil {
		return []Result{{Name: name, Unit: "ops/sec", Error: err.Error()}}
	}
	start := time.Now()
	totalGas := int64(0)
	for index := 0; index < iterations; index++ {
		if _, err := manager.CallContractFunction(name, function, params, &gasLimit); err != nil {
			return []Result{{Name: name, Unit: "ops/sec", Error: fmt.Sprintf("iteration %d: %v", index, err)}}
		}
		totalGas += manager.LastGasUsed
	}
	elapsed := time.Since(start)
	return []Result{{
		Name:    name,
		Unit:    "ops/sec",
		Value:   perSecond(int64(iterations), elapsed),
		Ops:     int64(iterations),
		Seconds: elapsed.Seconds(),
		Extra: map[string]any{
			"average_gas": float64(totalGas) / float64(iterations),
			"gas_limit":   gasLimit,
		},
	}}
}

func sanrc20Workload(source string, iterations int) []Result {
	bytecode, err := sanvm.CompilePena(source, "")
	if err != nil {
		return []Result{{Name: "sanrc20_transfer", Unit: "ops/sec", Error: err.Error()}}
	}
	manager := sanvm.NewContractManager(nil)
	gasLimit := int64(50_000_000)
	if err := manager.DeployContract("sanrc20", bytecode, &gasLimit); err != nil {
		return []Result{{Name: "sanrc20_transfer", Unit: "ops/sec", Error: err.Error()}}
	}
	owner := "0x" + strings.Repeat("11", 20)
	recipient := "0x" + strings.Repeat("22", 20)
	if _, err := manager.CallContractFunction("sanrc20", "init",
		[]any{"SAN Token", "SANRC20", int64(18), int64(1_000_000_000), owner}, &gasLimit); err != nil {
		return []Result{{Name: "sanrc20_transfer", Unit: "ops/sec", Error: err.Error()}}
	}
	start := time.Now()
	totalGas := int64(0)
	for index := 0; index < iterations; index++ {
		if _, err := manager.CallContractFunction("sanrc20", "transfer",
			[]any{owner, recipient, int64(1)}, &gasLimit); err != nil {
			return []Result{{Name: "sanrc20_transfer", Unit: "ops/sec", Error: fmt.Sprintf("iteration %d: %v", index, err)}}
		}
		totalGas += manager.LastGasUsed
	}
	elapsed := time.Since(start)
	return []Result{{
		Name:    "sanrc20_transfer",
		Unit:    "ops/sec",
		Value:   perSecond(int64(iterations), elapsed),
		Ops:     int64(iterations),
		Seconds: elapsed.Seconds(),
		Extra: map[string]any{
			"average_gas": float64(totalGas) / float64(iterations),
			"gas_limit":   gasLimit,
		},
	}}
}

func ammWorkload(source string, iterations int) []Result {
	bytecode, err := sanvm.CompilePena(source, "")
	if err != nil {
		return []Result{{Name: "amm_swap", Unit: "ops/sec", Error: err.Error()}}
	}
	manager := sanvm.NewContractManager(nil)
	gasLimit := int64(50_000_000)
	if err := manager.DeployContract("amm", bytecode, &gasLimit); err != nil {
		return []Result{{Name: "amm_swap", Unit: "ops/sec", Error: err.Error()}}
	}
	owner := "0x" + strings.Repeat("33", 20)
	trader := "0x" + strings.Repeat("44", 20)
	if _, err := manager.CallContractFunction("amm", "init",
		[]any{"token0", "token1", int64(30), owner}, &gasLimit); err != nil {
		return []Result{{Name: "amm_swap", Unit: "ops/sec", Error: err.Error()}}
	}
	if _, err := manager.CallContractFunction("amm", "addLiquidity",
		[]any{owner, int64(1_000_000), int64(1_000_000)}, &gasLimit); err != nil {
		return []Result{{Name: "amm_swap", Unit: "ops/sec", Error: err.Error()}}
	}
	start := time.Now()
	totalGas := int64(0)
	for index := 0; index < iterations; index++ {
		if _, err := manager.CallContractFunction("amm", "swap0For1",
			[]any{trader, int64(100)}, &gasLimit); err != nil {
			return []Result{{Name: "amm_swap", Unit: "ops/sec", Error: fmt.Sprintf("iteration %d: %v", index, err)}}
		}
		totalGas += manager.LastGasUsed
	}
	elapsed := time.Since(start)
	return []Result{{
		Name:    "amm_swap",
		Unit:    "ops/sec",
		Value:   perSecond(int64(iterations), elapsed),
		Ops:     int64(iterations),
		Seconds: elapsed.Seconds(),
		Extra: map[string]any{
			"average_gas": float64(totalGas) / float64(iterations),
			"gas_limit":   gasLimit,
		},
	}}
}

// ---------------------------------------------------------------------- #
// Repository helpers
// ---------------------------------------------------------------------- #

// readExample loads a PENA example relative to the repository root.
func readExample(directory, name string) (string, error) {
	root, err := RepoRoot()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(root, "PENA", "examples", directory, name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// RepoRoot walks up from this source file until go.mod is found.
func RepoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve the bench source path")
	}
	directory := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("go.mod not found above %s", filepath.Dir(thisFile))
		}
		directory = parent
	}
}
