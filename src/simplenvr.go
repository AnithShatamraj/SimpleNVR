package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Config struct {
	SegmentTime   int
	RetryInterval int
	MaxBackoff    int
}

type Camera struct {
	ID        int
	Name      string
	URL       string
	OutputDir string
	Username  string
	Password  string
	Restream  sql.NullString
}

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
	menuLoop()
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

func menuLoop() {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("\n=== NVR Menu ===")
		fmt.Println("1. List cameras")
		fmt.Println("2. Add camera")
		fmt.Println("3. Edit camera")
		fmt.Println("4. Remove camera")
		fmt.Println("5. Show/Edit config")
		fmt.Println("6. Start service")
		fmt.Println("7. Stop service")
		fmt.Println("8. Exit")
		fmt.Print("Choose: ")

		choiceStr, _ := reader.ReadString('\n')
		choiceStr = strings.TrimSpace(choiceStr)
		switch choiceStr {
		case "1":
			listCameras()
		case "2":
			addCamera(reader)
		case "3":
			editCamera(reader)
		case "4":
			removeCamera(reader)
		case "5":
			editConfig(reader)
		case "6":
			startService()
		case "7":
			stopService()
		case "8":
			fmt.Println("Exiting...")
			return
		default:
			fmt.Println("Invalid choice.")
		}
	}
}

func listCameras() {
	rows, err := db.Query("SELECT id, name, url, output_dir, restream_url FROM cameras")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()

	fmt.Println("Cameras:")
	for rows.Next() {
		var cam Camera
		rows.Scan(&cam.ID, &cam.Name, &cam.URL, &cam.OutputDir, &cam.Restream)
		fmt.Printf("[%d] %s (%s)\n", cam.ID, cam.Name, cam.URL)
	}
}

func promptInput(reader *bufio.Reader, field string) string {
	fmt.Printf("Enter %s: ", field)
	input, _ := reader.ReadString('\n')
	return strings.TrimSpace(input)
}

func promptMandatoryInput(reader *bufio.Reader, field string) string {
	for {
		value := promptInput(reader, field)
		if value == "" {
			fmt.Printf("%s cannot be empty.\n", field)
			continue
		}
		return value
	}
}

func promptUniqueInput(reader *bufio.Reader, field, table, column string) string {
	for {
		value := promptMandatoryInput(reader, field)
		valueExists := checkIfValueExists(table, column, value)
		if valueExists {
			fmt.Printf("'%s' already exists. Please enter a unique %s.\n", value, field)
			continue
		}
		return value
	}
}

func checkIfValueExists(table, column, value string) bool {
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s=?", table, column)
	err := db.QueryRow(query, value).Scan(&count)
	if err != nil {
		log.Fatal(err)
	}
	return count > 0
}

func promptYesNo(reader *bufio.Reader, question string) bool {
	for {
		question = fmt.Sprintf("%s (y/n): ", question)
		choice := promptMandatoryInput(reader, question)
		switch choice {
		case "y":
			return true
		case "n":
			return false
		}
		fmt.Println("Invalid choice. Please enter 'y' or 'n'.")
	}
}

func getRTSPDetails(reader *bufio.Reader) (string, string, string) {
	var url, username, password, parsedURL, rtspFullURL string
	for {
		url = promptMandatoryInput(reader, "RTSP URL: ")
		parsedURL := url
		if strings.HasPrefix(url, "rtsp://") {
			parts := strings.SplitN(url[7:], "@", 2)
			if len(parts) == 2 && strings.Contains(parts[0], ":") {
				up := strings.SplitN(parts[0], ":", 2)
				username = up[0]
				password = up[1]
				parsedURL = "rtsp://" + parts[1]
				fmt.Printf("Parsed URL: %s\nUsername: %s\nPassword: %s\n", parsedURL, username, password)
			}

			url_exists := checkIfValueExists("cameras", "url", url)
			if url_exists {
				fmt.Printf("'%s' already exists. Please enter a unique RTSP URL.\n", url)
				continue
			}

		} else {
			fmt.Println("Invalid RTSP URL. It should start with 'rtsp://'.")
			continue
		}
		if username == "" {
			username = promptInput(reader, "Username (optional): ")
		}
		if username != "" {
			password = promptMandatoryInput(reader, "Password: ")
		}
		rtspFullURL = url
		if username != "" {
			rtspFullURL = fmt.Sprintf("rtsp://%s:%s@%s", username, password, strings.TrimPrefix(parsedURL, "rtsp://"))
		}
		fmt.Println("Checking if the stream is working...")
		ffmpegArgs := []string{"-rtsp_transport", "tcp", "-i", rtspFullURL, "-t", "1", "-f", "null", "-"}
		cmd := exec.Command("ffmpeg", ffmpegArgs...)
		cmd.Stdout = nil
		cmd.Stderr = nil
		err := cmd.Run()
		if err != nil {
			retryRTSP := promptYesNo(reader, "Error accessing the stream. Do you want to re-enter the URL?")
			if retryRTSP {
				continue
			}
		} else {
			fmt.Println("Stream is reachable.")
		}
		break
	}
	// Parse RTSP URL for username and password
	return parsedURL, username, password
}

func addCamera(reader *bufio.Reader) {
	for {
		var name, outputDir string
		name = promptUniqueInput(reader, "Camera name: ", "cameras", "name")
		outputDir = promptUniqueInput(reader, "Output directory: ", "cameras", "output_dir")

		// Parse RTSP URL for username and password
		rtsp_url, rtsp_username, rtsp_password := getRTSPDetails(reader)
		// Sanity check: try to probe the stream with ffmpeg
		restream := promptInput(reader, "Restream URL (optional): ")

		_, err := db.Exec(`
		INSERT INTO cameras (name, url, output_dir, restream_url, username, password)
		VALUES (?, ?, ?, ?, ?, ?)
		`, name, rtsp_url, outputDir, sql.NullString{String: restream, Valid: restream != ""}, rtsp_username, rtsp_password)
		if err != nil {
			log.Fatal(err)
			retry := promptYesNo(reader, fmt.Sprintf("Error adding camera: \n%s.\n\nDo you want to retry?", err))
			if !retry {
				break
			}
		} else {
			fmt.Println("Camera added successfully.")
		}
	}
}

func editCamera(reader *bufio.Reader) {
	listCameras()
	fmt.Print("Enter camera ID to edit: ")
	idStr, _ := reader.ReadString('\n')
	id, _ := strconv.Atoi(strings.TrimSpace(idStr))

	fmt.Print("New name (blank to skip): ")
	name, _ := reader.ReadString('\n')
	fmt.Print("New URL (blank to skip): ")
	url, _ := reader.ReadString('\n')
	fmt.Print("New output dir (blank to skip): ")
	outputDir, _ := reader.ReadString('\n')

	updates := []string{}
	args := []interface{}{}

	if strings.TrimSpace(name) != "" {
		updates = append(updates, "name=?")
		args = append(args, strings.TrimSpace(name))
	}
	if strings.TrimSpace(url) != "" {
		updates = append(updates, "url=?")
		args = append(args, strings.TrimSpace(url))
	}
	if strings.TrimSpace(outputDir) != "" {
		updates = append(updates, "output_dir=?")
		args = append(args, strings.TrimSpace(outputDir))
	}
	if len(updates) == 0 {
		fmt.Println("No changes.")
		return
	}
	args = append(args, id)
	query := "UPDATE cameras SET " + strings.Join(updates, ", ") + " WHERE id=?"
	_, err := db.Exec(query, args...)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Camera updated.")

	// If worker running, restart
	restartWorker(id)
}

func removeCamera(reader *bufio.Reader) {
	listCameras()
	fmt.Print("Enter camera ID to remove: ")
	idStr, _ := reader.ReadString('\n')
	id, _ := strconv.Atoi(strings.TrimSpace(idStr))

	stopWorker(id)
	_, err := db.Exec("DELETE FROM cameras WHERE id=?", id)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Camera removed.")
}

func editConfig(reader *bufio.Reader) {
	fmt.Printf("Current config: %+v\n", config)

	fmt.Print("New segment time (sec, blank to skip): ")
	segStr, _ := reader.ReadString('\n')
	if strings.TrimSpace(segStr) != "" {
		config.SegmentTime, _ = strconv.Atoi(strings.TrimSpace(segStr))
	}

	fmt.Print("New retry interval (sec, blank to skip): ")
	retryStr, _ := reader.ReadString('\n')
	if strings.TrimSpace(retryStr) != "" {
		config.RetryInterval, _ = strconv.Atoi(strings.TrimSpace(retryStr))
	}

	fmt.Print("New max backoff (sec, blank to skip): ")
	backStr, _ := reader.ReadString('\n')
	if strings.TrimSpace(backStr) != "" {
		config.MaxBackoff, _ = strconv.Atoi(strings.TrimSpace(backStr))
	}

	saveConfig(config)
	fmt.Println("Config updated.")
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
	if cam.Username != "" {
		parts := strings.SplitN(cam.URL, "://", 2)
		fullURL = fmt.Sprintf("%s://%s:%s@%s", parts[0], cam.Username, cam.Password, parts[1])
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
