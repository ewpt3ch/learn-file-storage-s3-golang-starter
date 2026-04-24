package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {

	const uploadLimit = 1 << 30
	r.Body = http.MaxBytesReader(w, r.Body, uploadLimit)

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

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

	dbVideo, err := cfg.db.GetVideo(videoID)
	if err != nil {
		if err == sql.ErrNoRows {
			respondWithError(w, http.StatusNotFound, "no video", err)
			return
		}
		respondWithError(w, http.StatusInternalServerError, "database error", err)
		return
	}

	if dbVideo.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "user is not author", err)
		return
	}

	vidFile, vidHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "parse form failed", err)
		return
	}
	defer vidFile.Close()

	mediaType, _, err := mime.ParseMediaType(vidHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed parse media header", err)
		return
	}

	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "incorrect media format", nil)
		return
	}

	tmpFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed create temp file", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	_, err = io.Copy(tmpFile, vidFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed copy to tmp", err)
		return
	}

	aspect, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "aspect ratio", err)
		return
	}

	_, err = tmpFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed seek", err)
		return
	}

	fastVideoPath, err := processVideoForFastStart(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "ffmpeg", err)
		return
	}
	fastVideo, err := os.Open(fastVideoPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed open processed video", err)
		return
	}
	defer os.Remove(fastVideoPath)
	defer fastVideo.Close()

	randSrc := make([]byte, 32)
	rand.Read(randSrc)
	key := base64.RawURLEncoding.EncodeToString(randSrc)
	key = fmt.Sprintf("%s/%s.mp4", aspect, key)

	cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &key,
		Body:        fastVideo,
		ContentType: &mediaType,
	})

	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, key)
	dbVideo.VideoURL = &videoURL
	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "updateing db failed", err)
		return
	}

	respVideo, err := cfg.dbVideoToSignedVideo(dbVideo)

	respondWithJSON(w, http.StatusOK, respVideo)
}

func getVideoAspectRatio(filePath string) (string, error) {
	if len(filePath) == 0 {
		return "", fmt.Errorf("no path given")
	}

	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	jsonBuffer := new(bytes.Buffer)
	cmd.Stdout = jsonBuffer
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("failed to run ffprobe: %v", err)
	}

	type videoData struct {
		Streams []struct {
			Aspect string `json:"display_aspect_ratio"`
		} `json:"streams"`
	}

	var jsonData videoData
	decoder := json.NewDecoder(jsonBuffer)
	err = decoder.Decode(&jsonData)
	if err != nil {
		return "", fmt.Errorf("failed to decode json: %v", err)
	}

	aspect := jsonData.Streams[0].Aspect
	fmt.Println(aspect)
	switch aspect {
	case "16:9":
		return "landscape", nil
	case "9:16":
		return "portrait", nil
	default:
		return "other", nil
	}

}

func processVideoForFastStart(filePath string) (string, error) {

	if len(filePath) == 0 {
		return "", fmt.Errorf("no path given")
	}

	outFile := filePath + ".processing"

	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outFile)
	cmdOut := new(bytes.Buffer)
	cmd.Stdout = cmdOut
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("ffmpeg failed: %v", err)
	}

	return outFile, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {

	client := s3.NewPresignClient(s3Client)
	req, err := client.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", err
	}

	return req.URL, nil

}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {

	urlParts := strings.Split(*video.VideoURL, ",")
	url, err := generatePresignedURL(cfg.s3Client, urlParts[0], urlParts[1], time.Minute)
	if err != nil {
		return video, err
	}

	video.VideoURL = &url
	return video, nil
}
