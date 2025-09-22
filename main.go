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

// Struct for the /upload-complete request body
type CompleteRequest struct {
	UploadID   string `json:"uploadId"`
	FileName   string `json:"fileName"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	DataOrigin string `json:"dataOrigin"`
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

	// Create folder name with timestamp, email, phone, and sanitized filename
	folderName := createFolderName(reqData.FileName, reqData.Email, reqData.Phone)

	// Upload original file to Nextcloud in its own folder
	finalFilename := filepath.Base(reqData.FileName)
	if err := uploadToNextcloudFolder(folderName, finalFilename, originalFileReader); err != nil {
		log.Printf("ERROR: Nextcloud upload failed for %s: %v", finalFilename, err)
		jsonError(w, "Failed to upload to Nextcloud.", http.StatusInternalServerError)
		return
	}

	// Create and upload description text file
	descriptionContent := createDescriptionContent(reqData.FileName, reqData.Email, reqData.Phone, reqData.DataOrigin)
	descriptionReader := strings.NewReader(descriptionContent)
	if err := uploadToNextcloudFolder(folderName, "descripcion.txt", descriptionReader); err != nil {
		log.Printf("ERROR: Failed to upload description file: %v", err)
		jsonError(w, "Failed to upload description file.", http.StatusInternalServerError)
		return
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

// uploadToNextcloud handles the WebDAV PUT request.
func uploadToNextcloud(filename string, data io.Reader) error {
	webdavURL := fmt.Sprintf(
		"%s/remote.php/dav/files/%s/%s/%s",
		appConfig.NextcloudURL,
		appConfig.NextcloudUser,
		appConfig.NextcloudUploadDir,
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

// sanitizeFilename removes or replaces characters that might cause issues in folder names
func sanitizeFilename(filename string) string {
	// Remove file extension and replace problematic characters
	baseName := strings.TrimSuffix(filename, filepath.Ext(filename))
	// Replace spaces and special characters with underscores
	baseName = strings.ReplaceAll(baseName, " ", "_")
	baseName = strings.ReplaceAll(baseName, "/", "_")
	baseName = strings.ReplaceAll(baseName, "\\", "_")
	baseName = strings.ReplaceAll(baseName, ":", "_")
	baseName = strings.ReplaceAll(baseName, "*", "_")
	baseName = strings.ReplaceAll(baseName, "?", "_")
	baseName = strings.ReplaceAll(baseName, "\"", "_")
	baseName = strings.ReplaceAll(baseName, "<", "_")
	baseName = strings.ReplaceAll(baseName, ">", "_")
	baseName = strings.ReplaceAll(baseName, "|", "_")
	return baseName
}

// createFolderName creates a folder name with timestamp, email, phone, and filename
func createFolderName(filename, email, phone string) string {
	timestamp := time.Now().Unix()
	sanitizedFilename := sanitizeFilename(filename)

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

	// Add filename
	components = append(components, sanitizedFilename)

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

// jsonError is a helper to return a JSON error response.
func jsonError(w http.ResponseWriter, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
