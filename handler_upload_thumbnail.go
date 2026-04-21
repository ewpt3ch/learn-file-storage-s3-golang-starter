package main

import (
	"database/sql"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
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
	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	const maxMemory = 10 << 20
	err = r.ParseMultipartForm(maxMemory)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "parse form failed", err)
		return
	}

	imgFile, imgHeader, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "bad file information", err)
		return
	}

	mediaType, _, err := mime.ParseMediaType(imgHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed to get mediatype", err)
		return
	}

	if mediaType != "image/jpeg" && mediaType != "image/png" {
		respondWithError(w, http.StatusUnsupportedMediaType, "only support .png and .jpeg", err)
		return
	}

	mediaExt, err := mime.ExtensionsByType(imgHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed to get extension", err)
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

	imgFileName := fmt.Sprintf("%s%s", videoIDString, mediaExt[0])

	imgPath := filepath.Join(cfg.assetsRoot, imgFileName)
	fmt.Print(imgPath)
	outfile, err := os.Create(imgPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed create img file", err)
		return
	}
	defer outfile.Close()

	_, err = io.Copy(outfile, imgFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "issue writing img to file", err)
		return
	}

	thumburl := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, imgFileName)

	dbVideo.ThumbnailURL = &thumburl

	err = cfg.db.UpdateVideo(dbVideo)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "updated db error", err)
		return
	}

	respondWithJSON(w, http.StatusOK, dbVideo)
}
