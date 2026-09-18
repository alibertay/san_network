package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/alibertay/san_network/internal/ledger"
	"github.com/alibertay/san_network/internal/netnode"
)

// Server is the HTTP handler exposing the FastAPI routes over a netnode.Node.
type Server struct {
	node    *netnode.Node
	config  netnode.NodeConfig
	limiter *rateLimiter
	faucet  *faucetState
	handler http.Handler
}

// NewServer builds the API handler with the configured rate/body limits.
func NewServer(node *netnode.Node, config netnode.NodeConfig) *Server {
	server := &Server{
		node:   node,
		config: config,
		faucet: newFaucetState(config.FaucetCooldown),
	}
	mux := http.NewServeMux()
	server.register(mux)
	server.limiter = newRateLimiter(config.RPCRateLimit, config.RPCRateWindow, config.RPCMaxBody)
	server.handler = server.limiter.wrap(server.wrapAuth(mux))
	return server
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// requireNode mirrors app.routes.get_node: a missing node is a 503.
func (s *Server) requireNode(w http.ResponseWriter) *netnode.Node {
	if s.node == nil {
		writeError(w, http.StatusServiceUnavailable, "Node is not started")
		return nil
	}
	return s.node
}

func (s *Server) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /validators", s.handleValidators)
	mux.HandleFunc("GET /genesis", s.handleGenesis)
	mux.HandleFunc("GET /finality", s.handleFinality)
	mux.HandleFunc("GET /evidence", s.handleEvidence)

	mux.HandleFunc("GET /headers", s.handleHeaders)
	mux.HandleFunc("GET /proof/account/{address}", s.handleAccountProof)
	mux.HandleFunc("GET /proof/tx/{block_index}/{tx_index}", s.handleTransactionProof)
	mux.HandleFunc("GET /snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /receipt/tx/{tx_id}", s.handleReceiptByTx)
	mux.HandleFunc("GET /receipt/{block_index}/{tx_index}", s.handleReceipt)
	mux.HandleFunc("GET /tx/{tx_id}", s.handleTransaction)
	mux.HandleFunc("GET /metrics", s.handleMetrics)

	mux.HandleFunc("GET /account/{address}", s.handleAccount)
	mux.HandleFunc("GET /block/{index}", s.handleBlock)
	s.registerStakeRoutes(mux)
	mux.HandleFunc("GET /mempool", s.handleMempool)
	mux.HandleFunc("GET /contracts", s.handleContracts)
	mux.HandleFunc("POST /contract/query", s.handleContractQuery)

	mux.HandleFunc("GET /sync", s.handleSync)
	mux.HandleFunc("POST /transaction", s.handleSubmitTransaction)
	mux.HandleFunc("POST /faucet", s.handleFaucet)
	mux.HandleFunc("GET /bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /join", s.handleJoin)
}

// ---------------------------------------------------------------------- #
// Status and queries
// ---------------------------------------------------------------------- #

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	tip := node.Tip()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"chain_id":         node.ChainID(),
		"schema_version":   ledger.SchemaVersion,
		"height":           tip.Index,
		"tip_hash":         tip.CurrentBlockHash,
		"state_root":       tip.StateRoot,
		"base_fee":         node.BaseFee(),
		"finalized_height": node.FinalizedHeight(),
		"finalized_hash":   node.FinalizedHash(),
		"peers":            len(node.Peers()),
		"controllers":      node.ControllerCount(),
		"mempool":          node.PendingCount(),
		"contracts":        len(node.ListContracts()),
		"validators":       node.ActiveValidatorCount(),
	})
}

func (s *Server) handleValidators(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	writeJSON(w, http.StatusOK, node.AttestationState())
}

func (s *Server) handleGenesis(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	allocations := map[string]any{}
	for address, units := range node.GenesisAllocations() {
		allocations[address] = strconv.FormatInt(units, 10)
	}
	parameters := map[string]any{}
	for name, value := range node.GenesisParameters() {
		parameters[name] = strconv.FormatInt(value, 10)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":           node.ChainID(),
		"schema_version":     ledger.SchemaVersion,
		"genesis_hash":       node.GenesisHash(),
		"genesis_allocation": allocations,
		"parameters":         parameters,
	})
}

func (s *Server) handleFinality(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	tip := node.Tip()
	writeJSON(w, http.StatusOK, map[string]any{
		"chain_id":             node.ChainID(),
		"height":               tip.Index,
		"tip_hash":             tip.CurrentBlockHash,
		"finalized_height":     node.FinalizedHeight(),
		"finalized_hash":       node.FinalizedHash(),
		"pending_vote_heights": node.PendingFinalityHeights(),
	})
}

func (s *Server) handleEvidence(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": node.EquivocationEvidence()})
}

// ---------------------------------------------------------------------- #
// Light client support
// ---------------------------------------------------------------------- #

func (s *Server) handleHeaders(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	validator := &requestValidator{}
	fromIndex := validator.intQuery(r, "from_index", 0, int64Ptr(0), nil)
	limit := validator.intQuery(r, "limit", 64, int64Ptr(1), int64Ptr(512))
	if !validator.flush(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"headers": node.GetHeaders(fromIndex, int(limit))})
}

func (s *Server) handleAccountProof(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	address := r.PathValue("address")
	proof, err := node.GetAccountProof(address)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if proof == nil {
		writeError(w, http.StatusNotFound, "No account proof for "+address)
		return
	}
	writeJSON(w, http.StatusOK, proof)
}

func (s *Server) handleTransactionProof(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	validator := &requestValidator{}
	blockIndex, _ := validator.intPath(r.PathValue("block_index"), "block_index")
	txIndex, _ := validator.intPath(r.PathValue("tx_index"), "tx_index")
	if !validator.flush(w) {
		return
	}
	proof := node.GetTransactionProof(blockIndex, txIndex)
	if proof == nil {
		writeError(w, http.StatusNotFound, "No such transaction")
		return
	}
	writeJSON(w, http.StatusOK, proof)
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	snapshot := node.FinalizedSnapshot()
	if snapshot == nil {
		writeError(w, http.StatusNotFound, "No finalized snapshot yet")
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (s *Server) handleReceipt(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	validator := &requestValidator{}
	blockIndex, _ := validator.intPath(r.PathValue("block_index"), "block_index")
	txIndex, _ := validator.intPath(r.PathValue("tx_index"), "tx_index")
	if !validator.flush(w) {
		return
	}
	receipt := node.GetReceipt(blockIndex, txIndex)
	if receipt == nil {
		writeError(w, http.StatusNotFound, "No receipt available")
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) handleTransaction(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	txID := r.PathValue("tx_id")
	transaction := node.GetTransaction(txID)
	if transaction == nil {
		writeError(w, http.StatusNotFound, "Unknown transaction "+txID)
		return
	}
	writeJSON(w, http.StatusOK, transaction)
}

func (s *Server) handleReceiptByTx(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	txID := r.PathValue("tx_id")
	transaction := node.GetTransaction(txID)
	receipt, _ := transaction["receipt"].(map[string]any)
	if transaction == nil || receipt == nil {
		writeError(w, http.StatusNotFound, "No receipt for "+txID)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	snapshot := node.MetricsSnapshot()
	snapshot["http_body_rejected"] = BodyLimitRejections()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, RenderMetrics(snapshot))
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	account, err := node.GetAccount(r.PathValue("address"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (s *Server) handleBlock(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	validator := &requestValidator{}
	index, _ := validator.intPath(r.PathValue("index"), "index")
	if !validator.flush(w) {
		return
	}
	block := node.GetBlock(index)
	if block == nil {
		writeError(w, http.StatusNotFound, "Block "+strconv.FormatInt(index, 10)+" not found")
		return
	}
	writeJSON(w, http.StatusOK, block)
}

func (s *Server) handleMempool(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"count":  node.PendingCount(),
		"tx_ids": node.PendingTxIDs(),
	})
}

func (s *Server) handleContracts(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"contracts": node.ListContracts()})
}

func (s *Server) handleContractQuery(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	object, ok := readObjectBody(w, r)
	if !ok {
		return
	}
	validator := &requestValidator{}
	contractID := validator.requiredString(object, "contract_id")
	functionName := validator.requiredString(object, "function_name")
	params := validator.listField(object, "params")
	if !validator.flush(w) {
		return
	}

	result, err := node.QueryContract(contractID, functionName, params)
	if err != nil {
		// QueryContract raises ValueError("Unknown contract: ...") for a
		// missing contract and generic VM errors otherwise (routes.py maps
		// the first to 404 and the second to 400).
		if strings.HasPrefix(err.Error(), "Unknown contract:") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// ---------------------------------------------------------------------- #
// Chain and peer management
// ---------------------------------------------------------------------- #

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	validator := &requestValidator{}
	fromIndex := validator.intQuery(r, "from_index", 0, int64Ptr(0), nil)
	limit := validator.intQuery(r, "limit", 128, int64Ptr(1), int64Ptr(1024))
	if !validator.flush(w) {
		return
	}
	writeJSON(w, http.StatusOK, node.GetSyncPayload(fromIndex, int(limit)))
}

func (s *Server) handleSubmitTransaction(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	payload, ok := readObjectBody(w, r)
	if !ok {
		return
	}
	result, err := node.SubmitTransaction(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	peers := node.Peers()
	peers = append(peers, node.SelfPeerRecord())
	writeJSON(w, http.StatusOK, map[string]any{"peers": peers})
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	bootstrap := ""
	if configured := node.Config().Bootstrap; configured != nil {
		bootstrap = strings.TrimSpace(*configured)
	}
	if bootstrap == "" {
		writeError(w, http.StatusBadRequest, "No bootstrap node configured (set SAN_BOOTSTRAP)")
		return
	}
	peers := node.DiscoverPeers(bootstrap)
	added := node.AddPeers(peers)
	if len(node.Peers()) > 0 {
		node.RegisterToNetwork(r.Context())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "joined",
		"new_peers": added,
		"peers":     node.Peers(),
	})
}

// ---------------------------------------------------------------------- #
// Body-model helpers (Pydantic-style validation)
// ---------------------------------------------------------------------- #

// requiredString mirrors a required Pydantic “str“ field.
func (v *requestValidator) requiredString(object map[string]any, name string) string {
	raw, present := object[name]
	if !present {
		v.add(validationDetail{
			Type: "missing",
			Loc:  []any{"body", name},
			Msg:  "Field required",
		})
		return ""
	}
	text, ok := raw.(string)
	if !ok {
		v.add(validationDetail{
			Type:  "string_type",
			Loc:   []any{"body", name},
			Msg:   "Input should be a valid string",
			Input: raw,
		})
		return ""
	}
	return text
}

// listField mirrors a Pydantic “list“ field with default_factory=list.
func (v *requestValidator) listField(object map[string]any, name string) []any {
	raw, present := object[name]
	if !present {
		return []any{}
	}
	items, ok := raw.([]any)
	if !ok {
		v.add(validationDetail{
			Type:  "list_type",
			Loc:   []any{"body", name},
			Msg:   "Input should be a valid list",
			Input: raw,
		})
		return nil
	}
	return items
}
