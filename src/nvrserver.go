package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Worker struct {
	cmd    *exec.Cmd
	camera Camera
	stopCh chan struct{}
}

var (
	db        *sql.DB
	workers   = make(map[int]*Worker)
	workersMu sync.Mutex
	config    Config
)

func main() {
	initDB()
	loadConfig()
	// menuLoop()
	go startCLIServer()
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite", "./nvr.db")
	if err != nil {
		log.Fatal(err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS config (
		id INTEGER PRIMARY KEY,
		segment_time INTEGER NOT NULL,
		retry_interval INTEGER NOT NULL,
		max_backoff INTEGER NOT NULL
	);

	CREATE TABLE IF NOT EXISTS cameras (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		url TEXT NOT NULL UNIQUE,
		output_dir TEXT NOT NULL UNIQUE,
		restream_url TEXT UNIQUE,
		username TEXT,
		password TEXT
	);
	`)
	if err != nil {
		log.Fatal(err)
	}
}

func loadConfig() {
	row := db.QueryRow("SELECT segment_time, retry_interval, max_backoff FROM config WHERE id=1")
	err := row.Scan(&config.SegmentTime, &config.RetryInterval, &config.MaxBackoff)
	if err == sql.ErrNoRows {
		fmt.Println("No config found. Please set it up first.")
		config = Config{SegmentTime: 300, RetryInterval: 10, MaxBackoff: 60}
		saveConfig(config)
	} else if err != nil {
		log.Fatal(err)
	}
}

func saveConfig(cfg Config) {
	_, err := db.Exec(`
	INSERT INTO config (id, segment_time, retry_interval, max_backoff)
	VALUES (1, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET 
		segment_time=excluded.segment_time,
		retry_interval=excluded.retry_interval,
		max_backoff=excluded.max_backoff;
	`, cfg.SegmentTime, cfg.RetryInterval, cfg.MaxBackoff)
	if err != nil {
		log.Fatal(err)
	}
}

func checkIfValueExists(table, column, value string) string {
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s=?", table, column)
	err := db.QueryRow(query, value).Scan(&count)
	if err != nil {
		log.Fatal(err)
	}
	if count > 0 {
		return "true"
	}
	return "false"
}

func startService() {
	//Check if config is set

	rows, err := db.Query("SELECT id, name, url, output_dir, username, password, restream_url FROM cameras")
	if err != nil {
		log.Fatal(err)
		fmt.Println("Error starting service:\n", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cam Camera
		rows.Scan(&cam.ID, &cam.Name, &cam.URL, &cam.OutputDir, &cam.Username, &cam.Password, &cam.Restream)
		startWorker(cam)
	}
	fmt.Println("Service started.")
}

func stopService() {
	workersMu.Lock()
	defer workersMu.Unlock()
	for id := range workers {
		stopWorker(id)
	}
	fmt.Println("Service stopped.")
}

func startWorker(cam Camera) {
	workersMu.Lock()
	defer workersMu.Unlock()

	if _, exists := workers[cam.ID]; exists {
		fmt.Printf("Worker for %s already running\n", cam.Name)
		return
	}

	// Build ffmpeg command
	fullURL := cam.URL
	if cam.Username.Valid && cam.Username.String != "" {
		parts := strings.SplitN(cam.URL, "://", 2)
		fullURL = fmt.Sprintf("%s://%s:%s@%s", parts[0], cam.Username.String, cam.Password.String, parts[1])
	}
	args := []string{
		"-i", fullURL,
		"-c", "copy",
		"-f", "segment",
		"-segment_time", strconv.Itoa(config.SegmentTime),
		fmt.Sprintf("%s/output_%%03d.mp4", cam.OutputDir),
	}

	if cam.Restream.Valid {
		args = append(args, "-f", "rtsp", cam.Restream.String)
	}

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	stopCh := make(chan struct{})
	workers[cam.ID] = &Worker{cmd: cmd, camera: cam, stopCh: stopCh}

	go func() {
		for {
			err := cmd.Run()
			if err != nil {
				fmt.Printf("Worker for %s failed: %v. Retrying...\n", cam.Name, err)
			}
			select {
			case <-stopCh:
				return
			default:
				time.Sleep(time.Duration(config.RetryInterval) * time.Second)
			}
		}
	}()
}

func stopWorker(id int) {
	workersMu.Lock()
	defer workersMu.Unlock()
	if w, ok := workers[id]; ok {
		close(w.stopCh)
		if w.cmd.Process != nil {
			w.cmd.Process.Kill()
		}
		delete(workers, id)
		fmt.Printf("Stopped worker for camera %d\n", id)
	}
}

func restartWorker(id int) {
	stopWorker(id)

	row := db.QueryRow("SELECT id, name, url, output_dir, restream_url FROM cameras WHERE id=?", id)
	var cam Camera
	err := row.Scan(&cam.ID, &cam.Name, &cam.URL, &cam.OutputDir, &cam.Restream)
	if err == nil {
		startWorker(cam)
	}
}

func handleCLIConn(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(line)
		response := handleCLICommand(cmd)
		conn.Write([]byte(response + "\n"))
	}
}

func handleCLICommand(cmd string) string {
	parts := strings.SplitN(cmd, "|", 2)
	switch parts[0] {
	case "list":
		return listCamerasString()
	case "start":
		startService()
		return "Service started."
	case "stop":
		stopService()
		return "Service stopped."
	case "config":
		return fmt.Sprintf("Current config: %+v", config)
	case "addCamera":
		return addCamera(parts[1])
	default:
		return "Unknown command"
	}
}

func addCamera(fields string) string {
	var cam Camera
	err := json.Unmarshal([]byte(fields), &cam)
	if err != nil {
		return fmt.Sprintf(`{"status": "failure", "message": "Invalid JSON: %v", "id": null}`, err)
	}

	res, err := db.Exec(`
		INSERT INTO cameras (name, url, output_dir, restream_url, username, password)
		VALUES (?, ?, ?, ?, ?, ?)`,
		cam.Name, cam.URL, cam.OutputDir, cam.Restream.String, cam.Username.String, cam.Password.String)
	if err != nil {
		return fmt.Sprintf(`{"status": "failure", "message": "Error adding camera: %v", "id": null}`, err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Sprintf(`{"status": "failure", "message": "Error getting inserted ID: %v", "id": null}`, err)
	}

	return fmt.Sprintf(`{"status": "success", "message": "Camera created successfully", "id": "%d"}`, id)
}

func removeCamera(id int) string {
	workersMu.Lock()
	defer workersMu.Unlock()
	if _, ok := workers[id]; ok {
		stopWorker(id)
	}
	_, err := db.Exec("DELETE FROM cameras WHERE id=?", id)
	if err != nil {
		return fmt.Sprintf(`{"status": "failure", "message": "Error removing camera: %v", "id": null}`, err)
	}
	return `{"status": "success", "message": "Camera removed successfully", "id": null}`
}

func listCameras() ([]byte, error) {
	rows, err := db.Query("SELECT * FROM cameras")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cameras []Camera
	for rows.Next() {
		var cam Camera
		if err := rows.Scan(&cam.ID, &cam.Name, &cam.URL); err != nil {
			return nil, err
		}
		cameras = append(cameras, cam)
	}

	return json.Marshal(cameras)
}

func startCLIServer() {
	ln, err := net.Listen("tcp", "127.0.0.1:9000")
	if err != nil {
		log.Fatalf("CLI server error: %v", err)
	}
	fmt.Println("CLI server listening on 127.0.0.1:9000")
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleCLIConn(conn)
	}
}
