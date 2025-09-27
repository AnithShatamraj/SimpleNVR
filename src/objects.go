package main

import (
	"database/sql"
	"encoding/json"
)

type JSONNullString sql.NullString

func (ns JSONNullString) MarshalJSON() ([]byte, error) {
	if !ns.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(ns.String)
}

func (ns *JSONNullString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		ns.String = ""
		ns.Valid = false
		return nil
	}
	err := json.Unmarshal(data, &ns.String)
	ns.Valid = (err == nil)
	return err
}

type Config struct {
	SegmentTime   int `json:"segment_time"`
	RetryInterval int `json:"retry_interval"`
	MaxBackoff    int `json:"max_backoff"`
}

type Camera struct {
	ID        int            `json:"id"`
	Name      string         `json:"name"`
	URL       string         `json:"url"`
	OutputDir string         `json:"output_dir"`
	Username  JSONNullString `json:"username"`
	Password  JSONNullString `json:"password"`
	Restream  JSONNullString `json:"restream"`
}
