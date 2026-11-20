package main

import "net/http"

// codexResetCreditsPath lists a ChatGPT account's reset credits on the
// Codex backend: the codex plugin's reset routes read the account with
// HostService.GetAccount and have the core send this request through the
// account's proxy (CONTRACTS §124, AC43). It is recorded and answered with
// no credits.
const codexResetCreditsPath = "/backend-api/wham/rate-limit-reset-credits"

func (s *server) codexRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+codexResetCreditsPath, func(w http.ResponseWriter, r *http.Request) {
		id := s.record(r, nil, "", false, "")
		s.setStatus(id, http.StatusOK)
		writeJSON(w, http.StatusOK, map[string]any{"credits": []any{}, "available_count": 0})
	})
}
