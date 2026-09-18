package api

import "net/http"

// registerStakeRoutes exposes raw validator/stake records. /validators only
// reports the active set; stake reconciliation needs the registry record even
// while a withdrawal is pending.
func (s *Server) registerStakeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /stake/{address}", s.handleStakeInfo)
}

func (s *Server) handleStakeInfo(w http.ResponseWriter, r *http.Request) {
	node := s.requireNode(w)
	if node == nil {
		return
	}
	record, err := node.ValidatorInfo(r.PathValue("address"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, record)
}
