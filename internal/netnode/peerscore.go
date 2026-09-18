package netnode

import (
	"fmt"
	"log"
	"net"
	"strings"
)

// Local peer management: deterministic integer scoring, inbound caps and
// temporary bans. This is local-only reputation (never consensus state): a
// node's score cannot influence block validity, staking or finality.
//
// Signals and weights (all additive, documented for operators):
//
//	+2  valid health check / successful outbound handshake
//	+2  valid block from the peer
//	+1  valid transaction from the peer
//	+1  accepted inbound session (small bootstrap credit)
//	-1  duplicate or useless announcement
//	-4  invalid finality vote (authenticated voter)
//	-5  malformed message / malformed peer record
//	-3  timeout
//	-8  bad advertisement (record failed verification)
//	-20 rate-limit abuse
//
// A score at or below peerBanThreshold triggers a temporary ban; each repeat
// ban doubles the cooldown up to PeerBanMaxSeconds. Scores are clamped so long
// sessions cannot overflow or outweigh a fresh abuse signal indefinitely.
const (
	peerBanThreshold int64 = -50
	peerScoreMin     int64 = -10_000
	peerScoreMax     int64 = 10_000

	scoreSignalInbound     = "inbound_accepted"
	scoreSignalHealth      = "health_check"
	scoreSignalValidBlock  = "valid_block"
	scoreSignalValidTx     = "valid_tx"
	scoreSignalDuplicate   = "duplicate"
	scoreSignalInvalidVote = "invalid_vote"
	scoreSignalMalformed   = "malformed"
	scoreSignalBadRecord   = "bad_record"
	scoreSignalTimeout     = "timeout"
	scoreSignalRateLimit   = "rate_limit"
)

var peerScoreWeights = map[string]int64{
	scoreSignalInbound:     1,
	scoreSignalHealth:      2,
	scoreSignalValidBlock:  2,
	scoreSignalValidTx:     1,
	scoreSignalDuplicate:   -1,
	scoreSignalInvalidVote: -4,
	scoreSignalMalformed:   -5,
	scoreSignalBadRecord:   -5,
	scoreSignalTimeout:     -3,
	scoreSignalRateLimit:   -20,
}

// peerScoreState is the per-identity local reputation record.
type peerScoreState struct {
	Key         string
	Score       int64
	Rewards     int64
	Penalties   int64
	Bans        int
	BannedUntil float64
	FirstSeen   float64
	LastSeen    float64
}

// subnetKey maps a host to its eclipse-resistance bucket: /24 for IPv4, /64
// for IPv6, the host itself otherwise (DNS names are their own bucket).
func subnetKey(host string) string {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	ip := net.ParseIP(host)
	if ip == nil {
		return strings.ToLower(host)
	}
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.0/24", v4[0], v4[1], v4[2])
	}
	normalized := ip.To16()
	if len(normalized) < 4 {
		return strings.ToLower(host)
	}
	return fmt.Sprintf("%02x%02x:%02x%02x::/64",
		normalized[0], normalized[1], normalized[2], normalized[3])
}

// peerSubnet returns the bucket of a peer record.
func peerSubnet(record map[string]any) string {
	if record == nil {
		return ""
	}
	return subnetKey(stringValue(record["host"]))
}

// peerIdentityKey identifies a peer account: the signed public key when
// present, the dial endpoint otherwise.
func peerIdentityKey(record map[string]any) string {
	if record == nil {
		return ""
	}
	if key, _ := record["public_key"].(string); key != "" {
		return key
	}
	return "peer:" + PeerLabel(record)
}

func (n *Node) scoreClock() float64 { return nowSeconds() }

// notePeerSignalKey applies one scoring signal to a key. It returns the score
// after applying it and whether the key became banned.
func (n *Node) notePeerSignalKey(key string, signal string) (int64, bool) {
	weight, known := peerScoreWeights[signal]
	if key == "" || !known {
		return 0, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.applyScoreLocked(key, weight, n.scoreClock(), false)
}

// notePeerSignal applies a signal to a peer record.
func (n *Node) notePeerSignal(record map[string]any, signal string) (int64, bool) {
	return n.notePeerSignalKey(peerIdentityKey(record), signal)
}

// applyScoreLocked mutates the score table; the caller must hold n.mu.
func (n *Node) applyScoreLocked(key string, weight int64, now float64, forceBan bool) (int64, bool) {
	if n.peerScores == nil {
		n.peerScores = map[string]*peerScoreState{}
	}
	state := n.peerScores[key]
	if state == nil {
		bound := n.config.MaxPeers * 8
		if bound < 64 {
			bound = 64
		}
		for len(n.peerScores) >= bound {
			if !n.evictPeerScoresLocked() {
				break
			}
		}
		state = &peerScoreState{Key: key, FirstSeen: now}
		n.peerScores[key] = state
	}
	state.LastSeen = now
	if weight > 0 {
		state.Rewards += weight
	} else if weight < 0 {
		state.Penalties += -weight
		n.incMetric("peer_penalties")
	}
	state.Score += weight
	if state.Score > peerScoreMax {
		state.Score = peerScoreMax
	}
	if state.Score < peerScoreMin {
		state.Score = peerScoreMin
	}
	banned := false
	if (forceBan || state.Score <= peerBanThreshold) && state.BannedUntil <= now {
		n.banPeerKeyLocked(state, now)
		banned = true
	}
	return state.Score, banned
}

// banPeerKeyLocked sets a temporary ban with exponential cooldown.
func (n *Node) banPeerKeyLocked(state *peerScoreState, now float64) {
	state.Bans++
	if state.Bans < 1 {
		state.Bans = 1
	}
	base := n.config.PeerBanSeconds
	if base <= 0 {
		base = DefaultPeerBanSeconds
	}
	maxBan := n.config.PeerBanMaxSeconds
	if maxBan <= 0 {
		maxBan = DefaultPeerBanMaxSeconds
	}
	cooldown := base
	for i := 1; i < state.Bans && cooldown < maxBan; i++ {
		cooldown *= 2
	}
	if cooldown > maxBan {
		cooldown = maxBan
	}
	state.BannedUntil = now + cooldown
	n.incMetric("peers_banned")
	log.Printf("Peer %s banned for %.0fs (score %d, ban %d)", state.Key, cooldown, state.Score, state.Bans)
}

// evictPeerScoresLocked drops the least recently seen score record so a
// hostile burst of fake identities stays bounded. The caller must hold n.mu.
func (n *Node) evictPeerScoresLocked() bool {
	var victimKey string
	var victim *peerScoreState
	for key, state := range n.peerScores {
		if victim == nil || state.LastSeen < victim.LastSeen ||
			(state.LastSeen == victim.LastSeen && key < victimKey) {
			victimKey = key
			victim = state
		}
	}
	if victimKey == "" {
		return false
	}
	delete(n.peerScores, victimKey)
	return true
}

// peerKeyBanned reports whether a key is currently banned (expired bans are
// cleared lazily).
func (n *Node) peerKeyBanned(key string) bool {
	if key == "" {
		return false
	}
	now := n.scoreClock()
	n.mu.Lock()
	defer n.mu.Unlock()
	state, ok := n.peerScores[key]
	if !ok || state.BannedUntil <= 0 {
		return false
	}
	if state.BannedUntil > now {
		return true
	}
	state.BannedUntil = 0
	return false
}

// peerBanned reports whether a peer record is banned.
func (n *Node) peerBanned(record map[string]any) bool {
	return n.peerKeyBanned(peerIdentityKey(record))
}

// PeerScoreOf returns the current local score for a peer record (0 when the
// peer has no score history). Exported for operators and tests.
func (n *Node) PeerScoreOf(record map[string]any) int64 {
	return n.peerScoreOfKey(peerIdentityKey(record))
}

func (n *Node) peerScoreOfKey(key string) int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	if state, ok := n.peerScores[key]; ok {
		return state.Score
	}
	return 0
}

// peerBanInfo exposes ban bookkeeping (tests, diagnostics).
func (n *Node) peerBanInfo(key string) (bans int, bannedUntil float64, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	state, present := n.peerScores[key]
	if !present {
		return 0, 0, false
	}
	return state.Bans, state.BannedUntil, true
}

// ActivePeerBans counts unexpired bans.
func (n *Node) ActivePeerBans() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return int(n.activePeerBansLocked())
}

// activePeerBansLocked counts unexpired bans; the caller must hold n.mu.
func (n *Node) activePeerBansLocked() int64 {
	now := n.scoreClock()
	active := int64(0)
	for _, state := range n.peerScores {
		if state.BannedUntil > now {
			active++
		}
	}
	return active
}

// scoreSummary renders the local score table for diagnostics.
func (n *Node) scoreSummary() map[string]int64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	summary := map[string]int64{}
	for key, state := range n.peerScores {
		summary[key] = state.Score
	}
	return summary
}

// ResetPeerScores clears the local reputation table (tests).
func (n *Node) ResetPeerScores() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.peerScores = map[string]*peerScoreState{}
}

// ---------------------------------------------------------------------- #
// Inbound caps (per IP and per subnet)
// ---------------------------------------------------------------------- #

// maxInboundPerIP / maxInboundPerSubnet resolve the configured cap, falling
// back to the defaults; <= 0 disables the cap.
func (n *Node) maxInboundPerIP() int {
	if n.config.MaxInboundPerIP == 0 {
		return DefaultMaxInboundPerIP
	}
	return n.config.MaxInboundPerIP
}

func (n *Node) maxInboundPerSubnet() int {
	if n.config.MaxInboundPerSubnet == 0 {
		return DefaultMaxInboundPerSubnet
	}
	return n.config.MaxInboundPerSubnet
}

func splitAddressHost(address string) string {
	address = strings.TrimSpace(address)
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	return strings.Trim(strings.TrimSpace(host), "[]")
}

// AdmitInboundPeer reserves an inbound slot for a remote address. It returns
// false (and increments a metric) when the per-IP or per-subnet cap is
// reached; ReleaseInboundPeer must be called exactly once on success.
func (n *Node) AdmitInboundPeer(address string) bool {
	host := splitAddressHost(address)
	if host == "" {
		return true
	}
	subnet := subnetKey(host)

	n.mu.Lock()
	if n.inboundIPs == nil {
		n.inboundIPs = map[string]int{}
		n.inboundSubnets = map[string]int{}
	}
	ipLimit := n.maxInboundPerIP()
	subnetLimit := n.maxInboundPerSubnet()
	ipCount := n.inboundIPs[host]
	subnetCount := n.inboundSubnets[subnet]
	rejected := (ipLimit > 0 && ipCount >= ipLimit) || (subnetLimit > 0 && subnetCount >= subnetLimit)
	if rejected {
		n.mu.Unlock()
		n.incMetric("peers_rejected_inbound")
		log.Printf("Inbound cap: rejecting %s (ip %d/%d, subnet %s %d/%d)",
			host, ipCount, ipLimit, subnet, subnetCount, subnetLimit)
		return false
	}
	n.inboundIPs[host] = ipCount + 1
	n.inboundSubnets[subnet] = subnetCount + 1
	n.mu.Unlock()
	return true
}

// ReleaseInboundPeer frees a slot reserved by AdmitInboundPeer.
func (n *Node) ReleaseInboundPeer(address string) {
	host := splitAddressHost(address)
	if host == "" {
		return
	}
	subnet := subnetKey(host)
	n.mu.Lock()
	defer n.mu.Unlock()
	if count := n.inboundIPs[host]; count > 1 {
		n.inboundIPs[host] = count - 1
	} else {
		delete(n.inboundIPs, host)
	}
	if count := n.inboundSubnets[subnet]; count > 1 {
		n.inboundSubnets[subnet] = count - 1
	} else {
		delete(n.inboundSubnets, subnet)
	}
}

// inboundUsage exposes current reservations (tests).
func (n *Node) inboundUsage() (perIP, perSubnet map[string]int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	perIP = map[string]int{}
	perSubnet = map[string]int{}
	for key, value := range n.inboundIPs {
		perIP[key] = value
	}
	for key, value := range n.inboundSubnets {
		perSubnet[key] = value
	}
	return perIP, perSubnet
}

// ---------------------------------------------------------------------- #
// Peer-table caps and outbound diversity
// ---------------------------------------------------------------------- #

// maxPeersPerSubnet resolves the peer-table bucket cap (0 disables it).
func (n *Node) maxPeersPerSubnet() int {
	if n.config.MaxPeersPerSubnet == 0 {
		return DefaultMaxPeersPerSubnet
	}
	return n.config.MaxPeersPerSubnet
}

// subnetPeerLimitReachedLocked counts the peer table for a candidate's bucket.
// The caller must hold n.mu.
func (n *Node) subnetPeerLimitReachedLocked(record map[string]any) bool {
	limit := n.maxPeersPerSubnet()
	if limit <= 0 {
		return false
	}
	subnet := peerSubnet(record)
	count := 0
	for _, peer := range n.PEERS {
		if peerSubnet(peer) == subnet {
			count++
		}
	}
	return count >= limit
}

// selectOutboundPlan orders addrman candidates so a single subnet cannot
// occupy every outbound slot while still allowing a fallback when the address
// book only contains one subnet (e.g. a private devnet). It never drops
// candidates: over-cap entries move to the back.
func (n *Node) selectOutboundPlan(candidates []map[string]any) []map[string]any {
	limit := n.config.MaxOutboundPerSubnet
	if limit <= 0 {
		limit = DefaultMaxOutboundPerSubnet
	}
	plan := make([]map[string]any, 0, len(candidates))
	deferred := make([]map[string]any, 0, len(candidates))
	counts := map[string]int{}
	for _, record := range candidates {
		subnet := peerSubnet(record)
		if counts[subnet] >= limit {
			deferred = append(deferred, record)
			continue
		}
		counts[subnet]++
		plan = append(plan, record)
	}
	return append(plan, deferred...)
}

// noteMalformedMessage applies the standard malformed-message penalty and
// counter for an authenticated session key.
func (n *Node) noteMalformedMessage(key string) {
	n.notePeerSignalKey(key, scoreSignalMalformed)
	n.incMetric("peer_malformed_messages")
}
