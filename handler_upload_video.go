package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type FFProbeOutput struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	videoIDString := r.PathValue("videoID")
	uuid, err := uuid.Parse(videoIDString)
	if err != nil {
		http.Error(w, "invalid videoID", http.StatusBadRequest)
		return
	}

	videoMetaData, err := cfg.db.GetVideo(uuid)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "video not found", err)
		return
	}

	// Add debug line here
	fmt.Printf("Debug: videoMetaData.UserID = %v, userID = %v\n", videoMetaData.UserID, userID)

	if videoMetaData.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "user is not video owner", err)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	err = r.ParseMultipartForm(1 << 30)
	if err != nil {
		http.Error(w, "unable to parse form data", http.StatusBadRequest)
		return
	}

	file, fileHeader, err := r.FormFile("video") // Assuming "video" is the form key
	if err != nil {
		http.Error(w, "unable to extract video file from form data", http.StatusBadRequest)
		return
	}
	defer file.Close()

	contentType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, "invalid Content-Type header", http.StatusBadRequest)
		return
	}

	// Check if it's an MP4
	if contentType != "video/mp4" {
		http.Error(w, "only MP4 videos are accepted", http.StatusBadRequest)
		return
	}

	// Create temporary file
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't create temp file", err)
		return
	}
	// Important: defer cleanup in LIFO order
	// Remove should happen after Close
	defer os.Remove(tempFile.Name())
	defer tempFile.Close()

	// Copy uploaded file to temp file
	_, err = io.Copy(tempFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't copy to temp file", err)
		return
	}

	// Reset file pointer to beginning for subsequent reads
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't reset temp file pointer", err)
		return
	}

	// Get aspect ratio
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't determine aspect ratio", err)
		return
	}

	// Determine prefix based on aspect ratio
	var prefix string
	switch aspectRatio {
	case "16:9":
		prefix = "landscape/"
	case "9:16":
		prefix = "portrait/"
	default:
		prefix = "other/"
	}

	// Generate random hex for filename
	randomHex := make([]byte, 16)
	_, err = rand.Read(randomHex)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't generate random hex", err)
		return
	}
	key := prefix + fmt.Sprintf("%x.mp4", randomHex)

	// processing step
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't process video for fast start", err)
		return
	}

	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't open processed video", err)
		return
	}
	defer os.Remove(processedFilePath) // Clean up the processed file when done
	defer processedFile.Close()

	// Upload to S3
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key), // Use the key with prefix
		Body:        processedFile,
		ContentType: aws.String("video/mp4"),
	})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't upload to S3", err)
		return
	}

	// Create comma-delimited string of bucket and key
	videoURL := fmt.Sprintf("https://%s/%s", cfg.s3CfDistribution, key)
	fmt.Printf("Debug: videoURL = %s\n", videoURL)

	// Update video URL in database
	err = cfg.db.UpdateVideoURL(uuid, videoURL)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't update video URL", err)
		return
	}

	// Get the updated video
	video, err := cfg.db.GetVideo(uuid)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't get updated video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)

}

func getVideoAspectRatio(filePath string) (string, error) {
	// Your code here
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var data FFProbeOutput
	if err := json.Unmarshal(stdout.Bytes(), &data); err != nil {
		return "", err
	}

	if len(data.Streams) == 0 {
		return "", fmt.Errorf("no streams found")
	}

	width := data.Streams[0].Width
	height := data.Streams[0].Height

	ratio := float64(width) / float64(height)

	// Use a small tolerance for floating point comparison
	const tolerance = 0.1

	// Check if it's close to 16:9
	if math.Abs(ratio-16.0/9.0) < tolerance {
		return "16:9", nil
	}

	// Check if it's close to 9:16
	if math.Abs(ratio-9.0/16.0) < tolerance {
		return "9:16", nil
	}

	// If neither, it's other
	return "other", nil
}

func processVideoForFastStart(filePath string) (string, error) {
	// Get the file's extension
	ext := filepath.Ext(filePath)

	// Remove the extension from the original path
	base := strings.TrimSuffix(filePath, ext)

	// Append '.processing' before the extension
	outputFilePath := base + ".processing" + ext

	// Create the ffmpeg command
	cmd := exec.Command(
		"ffmpeg",
		"-i", filePath, // Input file
		"-c", "copy", // Copy codec
		"-movflags", "faststart", // Fast start flag
		"-f", "mp4", // Output format
		outputFilePath, // Output file path
	)

	// Run the command and capture any errors
	if err := cmd.Run(); err != nil {
		return "", err // Return the error if the command fails
	}

	// Return the constructed file path
	return outputFilePath, nil
}
