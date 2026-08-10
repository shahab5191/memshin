package api

import (
	"encoding/json"
	"log"
	"net/http"
)

func (s *Server) handleChat() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("X-User-ID")
		log.Printf("Received chat request from user: %s", userID)

		var payload ChatRequestBody
		json.NewDecoder(r.Body).Decode(&payload)

		res, err := s.engine.Process(r.Context(), userID, payload.Prompt, "you are an assitant")
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
