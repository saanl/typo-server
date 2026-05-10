package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ---- model ----

type Note struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Tags      []string `json:"tags"`
	UpdatedAt int64    `json:"updatedAt"`
}

// ---- store ----

type Store struct {
	db *sql.DB
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite serializes writes

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS notes (
		id         TEXT PRIMARY KEY,
		title      TEXT NOT NULL DEFAULT 'Untitled',
		content    TEXT NOT NULL DEFAULT '',
		tags       TEXT NOT NULL DEFAULT '[]',
		updated_at INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return nil, err
	}
	// migration: add tags column to existing databases
	db.Exec(`ALTER TABLE notes ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func scanNoteRow(s scanner, n *Note) error {
	var tagsJSON string
	err := s.Scan(&n.ID, &n.Title, &n.Content, &tagsJSON, &n.UpdatedAt)
	if err != nil {
		return err
	}
	json.Unmarshal([]byte(tagsJSON), &n.Tags)
	if n.Tags == nil {
		n.Tags = []string{}
	}
	return nil
}

type scanner interface {
	Scan(dest ...interface{}) error
}

func marshalTags(tags []string) string {
	if tags == nil {
		return "[]"
	}
	data, _ := json.Marshal(tags)
	return string(data)
}

func (s *Store) List(tag string) ([]Note, error) {
	var rows *sql.Rows
	var err error
	if tag == "" {
		rows, err = s.db.Query("SELECT id, title, content, tags, updated_at FROM notes ORDER BY updated_at DESC")
	} else {
		rows, err = s.db.Query("SELECT id, title, content, tags, updated_at FROM notes WHERE tags LIKE ? ORDER BY updated_at DESC", `%"`+tag+`"%`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	notes := make([]Note, 0)
	for rows.Next() {
		var n Note
		if err := scanNoteRow(rows, &n); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

func (s *Store) Get(id string) (*Note, error) {
	var n Note
	err := scanNoteRow(s.db.QueryRow("SELECT id, title, content, tags, updated_at FROM notes WHERE id = ?", id), &n)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func (s *Store) Create(note Note) error {
	_, err := s.db.Exec("INSERT INTO notes (id, title, content, tags, updated_at) VALUES (?, ?, ?, ?, ?)",
		note.ID, note.Title, note.Content, marshalTags(note.Tags), note.UpdatedAt)
	return err
}

func (s *Store) Update(id, content string, tags []string) (*Note, error) {
	title := extractTitle(content)
	now := time.Now().UnixMilli()
	res, err := s.db.Exec("UPDATE notes SET content = ?, title = ?, tags = ?, updated_at = ? WHERE id = ?",
		content, title, marshalTags(tags), now, id)
	if err != nil {
		return nil, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, sql.ErrNoRows
	}
	return &Note{ID: id, Title: title, Content: content, Tags: tags, UpdatedAt: now}, nil
}

func (s *Store) Delete(id string) error {
	res, err := s.db.Exec("DELETE FROM notes WHERE id = ?", id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ---- helpers ----

var titleRe = regexp.MustCompile(`(?m)^#\s+(.+)$`)
var imageExts = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"image/bmp":  ".bmp",
	"image/svg+xml": ".svg",
}

const uploadDir = "uploads"
const maxUploadSize = 10 << 20 // 10MB

func extractTitle(content string) string {
	m := titleRe.FindStringSubmatch(content)
	if m != nil {
		return strings.TrimSpace(m[1])
	}
	return "Untitled"
}

func generateID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 13)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// ---- http ----

var store *Store

func main() {
	var err error
	store, err = NewStore("typonote.db")
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatalf("create uploads dir: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/notes", handleNotes)
	mux.HandleFunc("/api/notes/", handleNoteByID)
	mux.HandleFunc("/api/images", handleImages)
	mux.Handle("/uploads/", http.StripPrefix("/uploads/", http.FileServer(http.Dir(uploadDir))))

	addr := ":8080"
	log.Printf("TypoNote backend listening on http://localhost%s", addr)
	log.Fatal(http.ListenAndServe(addr, cors(mux)))
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleNotes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		listNotes(w, r)
	case http.MethodPost:
		createNote(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleNoteByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/notes/")
	switch r.Method {
	case http.MethodGet:
		getNote(w, id)
	case http.MethodPut:
		updateNote(w, r, id)
	case http.MethodDelete:
		deleteNote(w, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func listNotes(w http.ResponseWriter, r *http.Request) {
	notes, err := store.List(r.URL.Query().Get("tag"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusOK, notes)
}

func getNote(w http.ResponseWriter, id string) {
	note, err := store.Get(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if note == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	respond(w, http.StatusOK, note)
}

func createNote(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &input); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
	}
	note := Note{
		ID:        generateID(),
		Title:     extractTitle(input.Content),
		Content:   input.Content,
		Tags:      input.Tags,
		UpdatedAt: time.Now().UnixMilli(),
	}
	if err := store.Create(note); err != nil {
		http.Error(w, "create failed", http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusCreated, note)
}

func updateNote(w http.ResponseWriter, r *http.Request, id string) {
	var input struct {
		Content string   `json:"content"`
		Tags    []string `json:"tags"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &input); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	note, err := store.Update(id, input.Content, input.Tags)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusOK, note)
}

func deleteNote(w http.ResponseWriter, id string) {
	err := store.Delete(id)
	if err == sql.ErrNoRows {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	respond(w, http.StatusOK, map[string]string{"ok": "true"})
}

func respond(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// ---- image upload ----

func handleImages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		uploadImage(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func uploadImage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "file too large", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	buf := make([]byte, 512)
	n, _ := file.Read(buf)
	contentType := http.DetectContentType(buf[:n])

	ext, ok := imageExts[contentType]
	if !ok {
		http.Error(w, "unsupported image type: "+contentType, http.StatusBadRequest)
		return
	}

	name := fmt.Sprintf("%d%s%s", time.Now().UnixMilli(), generateID(), ext)
	dst, err := os.Create(filepath.Join(uploadDir, name))
	if err != nil {
		http.Error(w, "save file failed", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	dst.Write(buf[:n])
	if _, err := io.Copy(dst, file); err != nil {
		http.Error(w, "save file failed", http.StatusInternalServerError)
		return
	}

	respond(w, http.StatusCreated, map[string]string{
		"url":      "/uploads/" + name,
		"filename": header.Filename,
	})
}
