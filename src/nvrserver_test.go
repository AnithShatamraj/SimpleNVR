package main

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAddCamera(t *testing.T) {
	// Setup in-memory test DB
	var err error
	db, err = sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	defer db.Close()

	// Create cameras table
	_, err = db.Exec(`
    CREATE TABLE cameras (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        name TEXT NOT NULL UNIQUE,
        url TEXT NOT NULL UNIQUE,
        output_dir TEXT NOT NULL UNIQUE,
        restream_url TEXT,
        username TEXT,
        password TEXT
    );
    `)
	if err != nil {
		t.Fatalf("failed to create cameras table: %v", err)
	}

	// Prepare test camera JSON input
	input := `{
        "name": "Camera1",
        "url": "rtsp://example.com/stream",
        "output_dir": "/tmp/output",
        "restream": null,
        "username": null,
        "password": null
    }`

	resp := addCamera(input)

	// Check success response format
	if !strings.Contains(resp, `"status": "success"`) {
		t.Errorf("expected success status, got: %s", resp)
	}
	if !strings.Contains(resp, `"message": "Camera created successfully"`) {
		t.Errorf("expected success message, got: %s", resp)
	}

	// Check id field contains a number string
	var respObj struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		ID      string `json:"id"`
	}
	err = json.Unmarshal([]byte(resp), &respObj)
	if err != nil {
		t.Fatalf("error unmarshaling response JSON: %v", err)
	}
	if respObj.ID == "null" || respObj.ID == "" {
		t.Errorf("expected non-null id in success response, got: %q", respObj.ID)
	}

	// Test invalid JSON input
	invalidInput := `invalid json`
	resp = addCamera(invalidInput)
	if !strings.Contains(resp, `"status": "failure"`) {
		t.Errorf("expected failure status for invalid JSON, got: %s", resp)
	}

	// Insert duplicate to trigger DB error
	resp = addCamera(input)
	if !strings.Contains(resp, `"status": "failure"`) {
		t.Errorf("expected failure status for duplicate insert, got: %s", resp)
	}
}
