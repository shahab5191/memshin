package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

func (s *Server) handleChat() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("X-User-ID")
		log.Printf("Received chat request from user: %s", userID)

		var payload ChatRequestBody
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			// Dropping this error let a malformed body through as an empty
			// prompt, which the memory layers then answered from context.
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(payload.Prompt) == "" {
			http.Error(w, "prompt is required", http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
		defer cancel()

		res, err := s.engine.Process(ctx, userID, payload.Prompt, s.cfg.DefaultSystemMessage)
		if err != nil {
			log.Printf("Error processing chat request: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"response": res})
	}
}

func (s *Server) handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}
