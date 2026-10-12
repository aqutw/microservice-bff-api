package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

func initIdempotency() error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS idempotency_requests (key TEXT PRIMARY KEY, request_hash TEXT NOT NULL, response_json TEXT, response_status INTEGER);`)
	return err
}

// The first write serializes competing requests before reading replay state.
// Business changes, audit events, and the replay response commit together.
func beginMutation(w http.ResponseWriter, r *http.Request, payload any) (*sql.Tx, string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 200 {
		mutationJSON(w, 400, map[string]string{"error": "invalid_idempotency_key"})
		return nil, key, false
	}
	tx, err := db.Begin()
	if err != nil {
		mutationJSON(w, 500, map[string]string{"error": err.Error()})
		return nil, key, false
	}
	if key == "" {
		return tx, key, true
	}
	raw, _ := json.Marshal(payload)
	digest := sha256.Sum256(raw)
	hash := hex.EncodeToString(digest[:])
	_, err = tx.Exec("INSERT INTO idempotency_requests(key,request_hash) VALUES(?,?) ON CONFLICT(key) DO NOTHING", key, hash)
	if err != nil {
		_ = tx.Rollback()
		mutationJSON(w, 500, map[string]string{"error": err.Error()})
		return nil, key, false
	}
	var storedHash string
	var response sql.NullString
	var status sql.NullInt64
	err = tx.QueryRow("SELECT request_hash,response_json,response_status FROM idempotency_requests WHERE key=?", key).Scan(&storedHash, &response, &status)
	if err != nil {
		_ = tx.Rollback()
		mutationJSON(w, 500, map[string]string{"error": err.Error()})
		return nil, key, false
	}
	if storedHash != hash {
		_ = tx.Rollback()
		mutationJSON(w, 409, map[string]string{"error": "idempotency_conflict"})
		return nil, key, false
	}
	if response.Valid {
		_ = tx.Rollback()
		mutationJSON(w, int(status.Int64), json.RawMessage(response.String))
		return nil, key, false
	}
	return tx, key, true
}

func finishMutation(w http.ResponseWriter, tx *sql.Tx, key string, status int, body any) {
	raw, err := json.Marshal(body)
	if err == nil && key != "" {
		_, err = tx.Exec("UPDATE idempotency_requests SET response_json=?,response_status=? WHERE key=?", string(raw), status, key)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	if err != nil {
		mutationJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	mutationJSON(w, status, json.RawMessage(raw))
}

func mutationJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
