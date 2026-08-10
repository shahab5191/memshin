package api

import (
	"encoding/json"
	"log"
	"net/http"
)

func (s *Server) handleChat() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		r.Body.Read(body)
		log.Println("Received chat request:", string(body))
		response := map[string]string{"message": "This is a placeholder response."}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(response)
	}
}

func (s *Server) handleHealth() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}
