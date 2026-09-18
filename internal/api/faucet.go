package api

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alibertay/san_network/internal/ledger"
)

// faucetIPLimit bounds faucet requests per client IP within one cooldown
// window (in addition to the per-address cooldown below).
const faucetIPLimit = 10

// faucetState is the in-memory cooldown/rate-limit bookkeeping. It is only
// created when the faucet is enabled, so a disabled node keeps no state.
type faucetState struct {
	mu         sync.Mutex
	cooldown   time.Duration
	lastByAddr map[string]time.Time
	ipHits     map[string][]time.Time
}

func newFaucetState(cooldownSeconds float64) *faucetState {
	cooldown := time.Duration(cooldownSeconds * float64(time.Second))
	if cooldown < 0 {
		cooldown = 0
	}
	return &faucetState{
		cooldown:   cooldown,
		lastByAddr: map[string]time.Time{},
		ipHits:     map[string][]time.Time{},
	}
}

// allowIP applies the per-IP sliding window. It counts every request,
// including ones later rejected by the cooldown check.
func (f *faucetState) allowIP(ip string, now time.Time) bool {
	window := f.cooldown
	if window <= 0 {
		window = time.Minute
	}
	cutoff := now.Add(-window)
	bucket := f.ipHits[ip]
	start := 0
	for start < len(bucket) && !bucket[start].After(cutoff) {
		start++
	}
	if start > 0 {
		bucket = append([]time.Time{}, bucket[start:]...)
	}
	if len(bucket) >= faucetIPLimit {
		f.ipHits[ip] = bucket
		return false
	}
	f.ipHits[ip] = append(bucket, now)
	return true
}

// allowAddress reports whether the address is outside its cooldown, and how
// many seconds remain when it is not.
func (f *faucetState) allowAddress(address string, now time.Time) (bool, int64) {
	if f.cooldown <= 0 {
		return true, 0
	}
	last, seen := f.lastByAddr[address]
	if !seen {
		return true, 0
	}
	remaining := f.cooldown - now.Sub(last)
	if remaining <= 0 {
		return true, 0
	}
	seconds := int64(remaining / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return false, seconds
}

func (f *faucetState) recordAddress(address string, now time.Time) {
	f.lastByAddr[address] = now
}

// handleFaucet funds an address with a transfer signed by the node identity.
// The transaction goes through the ordinary mempool admission path, so it
// behaves exactly like a user transfer.
func (s *Server) handleFaucet(w http.ResponseWriter, r *http.Request) {
	if !s.config.FaucetEnabled {
		writeError(w, http.StatusNotFound, "faucet is disabled on this node (set SAN_FAUCET=1)")
		return
	}
	node := s.requireNode(w)
	if node == nil {
		return
	}

	payload, ok := readObjectBody(w, r)
	if !ok {
		return
	}
	rawAddress, _ := payload["address"].(string)
	rawAddress = strings.TrimSpace(rawAddress)
	if rawAddress == "" {
		writeError(w, http.StatusBadRequest, "address is required")
		return
	}
	if !ledger.IsValidAddress(rawAddress) {
		writeError(w, http.StatusBadRequest, "Invalid address: "+rawAddress)
		return
	}
	address, _ := ledger.NormalizeAddress(rawAddress)

	amountUnits := s.config.FaucetAmount
	if rawAmount, present := payload["amount"]; present && rawAmount != nil {
		parsed, err := ledger.SanToUnits(rawAmount)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		amountUnits = parsed
	}
	if amountUnits <= 0 {
		writeError(w, http.StatusBadRequest, "amount must be a positive SAN number")
		return
	}
	if amountUnits > s.config.FaucetMax {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"amount %s SAN exceeds the faucet maximum of %s SAN",
			ledger.UnitsToSAN(amountUnits), ledger.UnitsToSAN(s.config.FaucetMax)))
		return
	}

	now := time.Now()
	client := clientKey(r)
	s.faucet.mu.Lock()
	if !s.faucet.allowIP(client, now) {
		s.faucet.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, "faucet rate limit exceeded for this IP")
		return
	}
	allowed, retrySeconds := s.faucet.allowAddress(address, now)
	if !allowed {
		s.faucet.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf(
			"faucet cooldown active for %s; try again in %ds", address, retrySeconds))
		return
	}
	// Record the cooldown before submitting so concurrent requests for the
	// same address cannot both pass the check.
	s.faucet.recordAddress(address, now)
	s.faucet.mu.Unlock()

	identity := node.Identity()
	if identity == nil || !identity.CanSign() {
		writeError(w, http.StatusInternalServerError, "faucet signer is not configured")
		return
	}
	sender, err := ledger.AddressFromPublicKey(identity.PublicKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "faucet signer has an invalid public key")
		return
	}
	account, err := node.GetAccount(sender)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read the faucet balance: "+err.Error())
		return
	}
	nonce := faucetInt64(account["nonce"])

	tx, err := ledger.NewTransaction(map[string]any{
		"chain_id": node.ChainID(),
		"sender":   identity.PublicKeyHex(),
		"nonce":    nonce,
		"receiver": address,
		"value":    ledger.UnitsToSAN(amountUnits),
	}, nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot build the faucet transfer: "+err.Error())
		return
	}
	balance := faucetInt64(account["balance_units"])
	if balance < amountUnits+tx.Fee {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf(
			"faucet balance too low: %s SAN available, %s SAN needed (amount plus fee)",
			ledger.UnitsToSAN(balance), ledger.UnitsToSAN(amountUnits+tx.Fee)))
		return
	}
	signature, err := ledger.SignPayload(tx.Payload, identity.PrivateKey)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot sign the faucet transfer: "+err.Error())
		return
	}
	tx.Payload["signature"] = signature
	txID := ledger.TxID(tx.Payload)

	result, err := node.SubmitTransaction(tx.Payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if status, _ := result["status"].(string); status == "rejected" {
		reason, _ := result["reason"].(string)
		writeError(w, http.StatusBadRequest, "faucet transfer rejected: "+reason)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "pooled",
		"tx_id":  txID,
		"amount": ledger.UnitsToSAN(amountUnits),
	})
}

// faucetInt64 reads an integer from a canonical-decoded account field.
func faucetInt64(value any) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		return int64(typed)
	default:
		return 0
	}
}
