package server

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/metrics"
)

// listenHTTP opens the side listener (decision 018). It binds to a local
// address by default and has no authentication: it is for operators on
// the machine, not the internet.
func (s *Server) listenHTTP(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /instruments", s.handleListInstruments)
	mux.HandleFunc("POST /instruments", s.handleAddInstrument)
	mux.HandleFunc("GET /debug/latency", s.handleLatency)
	s.httpLn = ln
	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      s.cfg.AdminTimeout + 5*time.Second, // a POST waits up to AdminTimeout
		IdleTimeout:       60 * time.Second,
	}
	s.log.Info("http listening", "addr", ln.Addr().String())
	return nil
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", metrics.ContentType)
	if err := s.reg.WriteText(w); err != nil {
		s.log.Debug("metrics write", "err", err) // the scraper went away
	}
}

// latencyResponse is GET /debug/latency's body.
type latencyResponse struct {
	// Matching: matchd_matching_latency_seconds' span (dequeue to last
	// event sent), in log-linear buckets (metrics.LogSnapshot).
	Matching metrics.LogSnapshot `json:"matching"`
}

// handleLatency serves the engine's fine-grained latency histogram, counts
// since start. The benchmark reads it at the start and end of its window
// and subtracts (decision 021).
func (s *Server) handleLatency(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, latencyResponse{Matching: s.fine.Snapshot()})
}

// handleHealth: 200 while serving; 503 while shutting down, or for good
// after a durability failure.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	switch {
	case s.durable.Err() != nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "failed", "error": s.durable.Err().Error()})
	case s.state.Load() == stateStopping:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting down"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (s *Server) handleListInstruments(w http.ResponseWriter, _ *http.Request) {
	list, ok := s.pub.Instruments()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, ErrUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// addInstrumentRequest is POST /instruments' body.
type addInstrumentRequest struct {
	Symbol   string `json:"symbol"`
	TickSize int64  `json:"tick_size"`
	LotSize  int64  `json:"lot_size"`
}

func (s *Server) handleAddInstrument(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req addInstrumentRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.AddInstrument(book.Instrument{Symbol: req.Symbol, TickSize: req.TickSize, LotSize: req.LotSize})
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, info)
	case errors.Is(err, ErrInvalidInstrument):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, ErrDuplicateSymbol):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, ErrAdminTimeout):
		writeError(w, http.StatusGatewayTimeout, err)
	default:
		writeError(w, http.StatusServiceUnavailable, err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
