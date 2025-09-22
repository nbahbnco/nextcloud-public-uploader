package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config holds the application configuration.
type Config struct {
	NextcloudURL       string
	NextcloudUser      string
	NextcloudAppPass   string
	NextcloudUploadDir string
	UploadTempDir      string // Directory for temporary chunk storage
}

// Global config variable
var appConfig Config

// Upload session tracking
type UploadSession struct {
	Email          string
	Phone          string
	DataOrigin     string
	UploadCount    int
	CompletedCount int
	Mutex          sync.RWMutex
}

// Global map to track upload sessions
var uploadSessions = make(map[string]*UploadSession)
var sessionsMutex sync.RWMutex

// Struct for the /upload-complete request body
type CompleteRequest struct {
	UploadID   string `json:"uploadId"`
	FileName   string `json:"fileName"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	DataOrigin string `json:"dataOrigin"`
	SessionID  string `json:"sessionId"`
	TotalFiles int    `json:"totalFiles"`
}

// Struct for the /upload-session request body
type SessionRequest struct {
	SessionID  string `json:"sessionId"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	DataOrigin string `json:"dataOrigin"`
	TotalFiles int    `json:"totalFiles"`
}

func main() {
	// Load configuration from environment variables
	// TODO: Evaluate a possible configuration file
	appConfig = Config{
		NextcloudURL:       getEnv("NC_URL", ""),
		NextcloudUser:      getEnv("NC_USER", ""),
		NextcloudAppPass:   getEnv("NC_APP_PASSWORD", ""),
		NextcloudUploadDir: getEnv("NC_FOLDER", ""),
		UploadTempDir:      getEnv("UPLOAD_TEMP_DIR", "/tmp/nextcloud-public-uploader/"),
	}

	if appConfig.NextcloudURL == "" || appConfig.NextcloudUser == "" || appConfig.NextcloudAppPass == "" {
		log.Fatal("FATAL: Environment variables NC_URL, NC_USER, and NC_APP_PASSWORD must be set.")
	}
	appConfig.NextcloudURL = strings.TrimSuffix(appConfig.NextcloudURL, "/")

	// Create the temporary upload directory if it doesn't exist
	if err := os.MkdirAll(appConfig.UploadTempDir, os.ModePerm); err != nil {
		log.Fatalf("FATAL: Could not create temporary upload directory: %v", err)
	}

	log.Printf("Server starting...")
	log.Printf("Temporary chunk directory: %s", appConfig.UploadTempDir)
	log.Printf("Uploading to Nextcloud instance at: %s", appConfig.NextcloudURL)

	http.HandleFunc("/", serveForm)
	http.HandleFunc("/upload-session", handleUploadSession)
	http.HandleFunc("/upload-chunk", handleUploadChunk)
	http.HandleFunc("/upload-complete", handleUploadComplete)

	port := ":8080"
	log.Printf("Listening on http://localhost%s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatalf("Could not start server: %s\n", err)
	}
}

func serveForm(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

// handleUploadSession registers a new upload session
func handleUploadSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var reqData SessionRequest
	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		http.Error(w, "Invalid JSON body.", http.StatusBadRequest)
		return
	}

	sessionsMutex.Lock()
	uploadSessions[reqData.SessionID] = &UploadSession{
		Email:          reqData.Email,
		Phone:          reqData.Phone,
		DataOrigin:     reqData.DataOrigin,
		UploadCount:    reqData.TotalFiles,
		CompletedCount: 0,
	}
	sessionsMutex.Unlock()

	log.Printf("INFO: Registered upload session %s with %d files", reqData.SessionID, reqData.TotalFiles)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"message":   "Upload session registered successfully",
		"sessionId": reqData.SessionID,
	})
}

// handleUploadChunk receives and saves a single file chunk.
func handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Max chunk size + metadata (e.g., 5MB + buffer)
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "Could not parse form. Chunk might be too large.", http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("dataFile")
	if err != nil {
		http.Error(w, "Invalid file chunk key.", http.StatusBadRequest)
		return
	}
	defer file.Close()

	uploadID := r.FormValue("uploadId")
	chunkIndex := r.FormValue("chunkIndex")

	// Security: Sanitize uploadID to prevent path traversal attacks.
	// We do not validate the uploadID against a list of active upload IDs in order to keep the code simple and reduce execution complexity.
	// For simplicity, we clean the path.
	cleanUploadID := filepath.Clean(filepath.Base(uploadID))
	if cleanUploadID == "." || cleanUploadID == ".." {
		http.Error(w, "Invalid upload ID.", http.StatusBadRequest)
		return
	}

	chunkDir := filepath.Join(appConfig.UploadTempDir, cleanUploadID)
	if err := os.MkdirAll(chunkDir, os.ModePerm); err != nil {
		log.Printf("ERROR: Could not create chunk directory %s: %v", chunkDir, err)
		http.Error(w, "Server error creating chunk directory.", http.StatusInternalServerError)
		return
	}

	chunkPath := filepath.Join(chunkDir, chunkIndex)
	dst, err := os.Create(chunkPath)
	if err != nil {
		log.Printf("ERROR: Could not create chunk file %s: %v", chunkPath, err)
		http.Error(w, "Server error creating chunk file.", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	if _, err := io.Copy(dst, file); err != nil {
		log.Printf("ERROR: Could not save chunk file %s: %v", chunkPath, err)
		http.Error(w, "Server error saving chunk file.", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Chunk uploaded successfully")
}

// handleUploadComplete assembles chunks and uploads to Nextcloud.
func handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var reqData CompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		http.Error(w, "Invalid JSON body.", http.StatusBadRequest)
		return
	}

	// Security: Sanitize again.
	cleanUploadID := filepath.Clean(filepath.Base(reqData.UploadID))
	if cleanUploadID == "." || cleanUploadID == ".." {
		http.Error(w, "Invalid upload ID.", http.StatusBadRequest)
		return
	}

	chunkDir := filepath.Join(appConfig.UploadTempDir, cleanUploadID)
	defer os.RemoveAll(chunkDir) // Clean up chunks after we're done.

	// Read all chunk files from the directory
	chunkFiles, err := os.ReadDir(chunkDir)
	if err != nil {
		log.Printf("ERROR: Could not read chunk directory %s: %v", chunkDir, err)
		jsonError(w, "Could not find chunks on server.", http.StatusInternalServerError)
		return
	}

	// Sort chunks numerically by their filename (which is their index)
	sort.Slice(chunkFiles, func(i, j int) bool {
		numI, _ := strconv.Atoi(chunkFiles[i].Name())
		numJ, _ := strconv.Atoi(chunkFiles[j].Name())
		return numI < numJ
	})

	// Create a list of readers for the original file content only
	var readers []io.Reader

	// Read chunks (original file content only)
	for _, chunkFile := range chunkFiles {
		path := filepath.Join(chunkDir, chunkFile.Name())
		f, err := os.Open(path)
		if err != nil {
			log.Printf("ERROR: Could not open chunk file %s: %v", path, err)
			jsonError(w, "Error processing chunks.", http.StatusInternalServerError)
			return
		}
		// This cleanup is not 100% reliable, it's assumed that it's running in an ephemeral storage.
		readers = append(readers, f)
	}

	// Combine all readers into one for the original file
	originalFileReader := io.MultiReader(readers...)

	// Create folder name with timestamp, email, and phone
	folderName := createFolderName(reqData.Email, reqData.Phone)

	// Create folder in Nextcloud first
	if err := createNextcloudFolder(folderName); err != nil {
		log.Printf("ERROR: Failed to create folder %s: %v", folderName, err)
		jsonError(w, "Failed to create folder in Nextcloud.", http.StatusInternalServerError)
		return
	}

	// Upload original file to Nextcloud in its own folder
	finalFilename := filepath.Base(reqData.FileName)
	if err := uploadToNextcloudFolder(folderName, finalFilename, originalFileReader); err != nil {
		log.Printf("ERROR: Nextcloud upload failed for %s: %v", finalFilename, err)
		jsonError(w, "Failed to upload to Nextcloud.", http.StatusInternalServerError)
		return
	}

	// Check if this is part of a multi-file session
	var shouldUploadDescription bool
	if reqData.SessionID != "" {
		shouldUploadDescription = checkAndUpdateSession(reqData.SessionID, folderName, reqData.Email, reqData.Phone, reqData.DataOrigin)
	} else {
		// Single file upload - always upload description
		shouldUploadDescription = true
	}

	// Create and upload description text file only if needed
	if shouldUploadDescription {
		// Check if description file already exists
		if checkDescriptionFileExists(folderName) {
			log.Printf("INFO: Description file already exists in folder %s, skipping upload", folderName)
		} else {
			descriptionContent := createDescriptionContent(reqData.FileName, reqData.Email, reqData.Phone, reqData.DataOrigin)
			descriptionReader := strings.NewReader(descriptionContent)
			if err := uploadToNextcloudFolder(folderName, "descripcion.txt", descriptionReader); err != nil {
				log.Printf("ERROR: Failed to upload description file: %v", err)
				jsonError(w, "Failed to upload description file.", http.StatusInternalServerError)
				return
			}
			log.Printf("INFO: Uploaded description file for session %s", reqData.SessionID)
		}
	} else {
		log.Printf("INFO: Skipped description file upload for session %s (not all files complete)", reqData.SessionID)
	}

	// Respond with success
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"message":    "File uploaded successfully!",
		"folderName": folderName,
		"fileName":   finalFilename,
	})
}

// createNextcloudFolder creates a folder in Nextcloud using WebDAV MKCOL
func createNextcloudFolder(folderName string) error {
	webdavURL := fmt.Sprintf(
		"%s/remote.php/dav/files/%s/%s/%s",
		appConfig.NextcloudURL,
		appConfig.NextcloudUser,
		appConfig.NextcloudUploadDir,
		url.PathEscape(folderName),
	)
	req, err := http.NewRequest("MKCOL", webdavURL, nil)
	if err != nil {
		return fmt.Errorf("could not create request: %w", err)
	}
	req.SetBasicAuth(appConfig.NextcloudUser, appConfig.NextcloudAppPass)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request execution failed: %w", err)
	}
	defer resp.Body.Close()

	// MKCOL returns 201 for created, 405 for already exists, both are OK
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusMethodNotAllowed {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bad response from Nextcloud: %s (body: %s)", resp.Status, string(body))
	}
	return nil
}

// uploadToNextcloudFolder uploads a file to a specific folder in Nextcloud
func uploadToNextcloudFolder(folderName, filename string, data io.Reader) error {
	webdavURL := fmt.Sprintf(
		"%s/remote.php/dav/files/%s/%s/%s/%s",
		appConfig.NextcloudURL,
		appConfig.NextcloudUser,
		appConfig.NextcloudUploadDir,
		url.PathEscape(folderName),
		url.PathEscape(filename),
	)

	req, err := http.NewRequest(http.MethodPut, webdavURL, data)
	if err != nil {
		return fmt.Errorf("could not create request: %w", err)
	}
	req.SetBasicAuth(appConfig.NextcloudUser, appConfig.NextcloudAppPass)
	client := &http.Client{Timeout: 60 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request execution failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bad response from Nextcloud: %s (body: %s)", resp.Status, string(body))
	}
	return nil
}

// createFolderName creates a folder name with timestamp, email, and phone (no filename)
func createFolderName(email, phone string) string {
	timestamp := time.Now().Unix()

	// Sanitize email for folder name (remove @ and replace with _at_)
	sanitizedEmail := strings.ReplaceAll(email, "@", "_at_")
	sanitizedEmail = strings.ReplaceAll(sanitizedEmail, ".", "_")

	// Sanitize phone for folder name (remove spaces, dashes, parentheses)
	sanitizedPhone := strings.ReplaceAll(phone, " ", "")
	sanitizedPhone = strings.ReplaceAll(sanitizedPhone, "-", "")
	sanitizedPhone = strings.ReplaceAll(sanitizedPhone, "(", "")
	sanitizedPhone = strings.ReplaceAll(sanitizedPhone, ")", "")
	sanitizedPhone = strings.ReplaceAll(sanitizedPhone, "+", "plus")

	// Build folder name components
	components := []string{fmt.Sprintf("%d", timestamp)}

	// Add email if provided
	if email != "" {
		components = append(components, sanitizedEmail)
	}

	// Add phone if provided
	if phone != "" {
		components = append(components, sanitizedPhone)
	}

	return strings.Join(components, "-")
}

// createDescriptionContent creates the content for the description.txt file
func createDescriptionContent(filename, email, phone, dataOrigin string) string {
	var buffer bytes.Buffer
	buffer.WriteString("--- UPLOAD INFORMATION ---\n")
	buffer.WriteString(fmt.Sprintf("Timestamp (UTC): %s\n", time.Now().UTC().Format(time.RFC3339)))
	buffer.WriteString(fmt.Sprintf("Original Filename: %s\n", filename))
	buffer.WriteString(fmt.Sprintf("Email: %s\n", email))
	if phone != "" {
		buffer.WriteString(fmt.Sprintf("Teléfono: %s\n", phone))
	}
	buffer.WriteString("\n--- DESCRIPCIÓN ---\n")
	buffer.WriteString(dataOrigin)
	buffer.WriteString("\n\n--- FIN ---\n")
	return buffer.String()
}

// getEnv is a helper to read an env var or return a default.
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// checkAndUpdateSession checks if all files in a session are complete and updates the session
func checkAndUpdateSession(sessionID, folderName, email, phone, dataOrigin string) bool {
	sessionsMutex.Lock()
	defer sessionsMutex.Unlock()

	session, exists := uploadSessions[sessionID]
	if !exists {
		log.Printf("WARNING: Session %s not found, treating as single file upload", sessionID)
		return true
	}

	session.Mutex.Lock()
	defer session.Mutex.Unlock()

	session.CompletedCount++
	log.Printf("INFO: Session %s: %d/%d files completed", sessionID, session.CompletedCount, session.UploadCount)

	if session.CompletedCount >= session.UploadCount {
		// All files completed - upload description file and clean up session
		log.Printf("INFO: All files completed for session %s, uploading description file", sessionID)
		delete(uploadSessions, sessionID)
		return true
	}

	// Not all files completed yet
	return false
}

// checkDescriptionFileExists checks if a description file already exists in the folder
func checkDescriptionFileExists(folderName string) bool {
	webdavURL := fmt.Sprintf(
		"%s/remote.php/dav/files/%s/%s/%s/descripcion.txt",
		appConfig.NextcloudURL,
		appConfig.NextcloudUser,
		appConfig.NextcloudUploadDir,
		url.PathEscape(folderName),
	)

	req, err := http.NewRequest("HEAD", webdavURL, nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth(appConfig.NextcloudUser, appConfig.NextcloudAppPass)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// jsonError is a helper to return a JSON error response.
func jsonError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
