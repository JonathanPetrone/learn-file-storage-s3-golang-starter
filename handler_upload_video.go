package main

import (
	"crypto/rand"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

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

	// Generate random hex for filename
	randomHex := make([]byte, 16)
	_, err = rand.Read(randomHex)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't generate random hex", err)
		return
	}
	key := fmt.Sprintf("%x.mp4", randomHex)

	// Upload to S3
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(key),
		Body:        tempFile,
		ContentType: aws.String("video/mp4"),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't upload to S3", err)
		return
	}

	// Create S3 URL
	s3URL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, key)

	// Update video URL in database
	err = cfg.db.UpdateVideoURL(uuid, s3URL)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "couldn't update video URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, struct{}{})

}
